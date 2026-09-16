// SPDX-License-Identifier: AGPL-3.0-only

// Package script provides WebAssembly script execution via wazero.
package script

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Executor implements kernel.ScriptExecutor using wazero.
type Executor struct {
	runtime wazero.Runtime
	cfg     Config

	mu    sync.Mutex                       // guards cache
	cache map[string]wazero.CompiledModule // keyed by artifact hash
}

// hostKey carries the per-call kernel.HostFunctions through the execution context.
// The "juice" host module is instantiated once and stateless; each invocation reads
// the active call's host from the context, so executions run concurrently and may
// nest (a WASM action calling another via juice.call) without a global lock.
type hostKey struct{}

func hostFromCtx(ctx context.Context) kernel.HostFunctions {
	h, _ := ctx.Value(hostKey{}).(kernel.HostFunctions)
	return h
}

// Config holds script execution limits.
type Config struct {
	TimeoutMS   int64 // milliseconds per execution
	MemoryBytes int64 // maximum linear memory
}

// New creates a wazero-backed Executor.
func New(cfg Config) *Executor {
	rCfg := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	if cfg.MemoryBytes > 0 {
		rCfg = rCfg.WithMemoryLimitPages(MemoryPages(cfg.MemoryBytes))
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, rCfg)
	// Provide WASI host functions required by TinyGo's wasip1 target.
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	// Instantiate the "juice" host module once. Its callbacks are stateless and read
	// the active call's HostFunctions from the context, so it is shared across all
	// (concurrent and nested) executions. The guest imports are hardwired to module
	// "juice" (//go:wasmimport juice ...), so the name is fixed by the SDK.
	hostBuilder := rt.NewHostModuleBuilder("juice")
	registerHostFunctions(hostBuilder)
	if _, err := hostBuilder.Instantiate(ctx); err != nil {
		panic("script: instantiate juice host module: " + err.Error())
	}
	return &Executor{
		runtime: rt,
		cfg:     cfg,
		cache:   make(map[string]wazero.CompiledModule),
	}
}

// Compile validates and compiles WASM bytes, returning the artifact and its SHA-256 hash.
func (e *Executor) Compile(ctx context.Context, source []byte) ([]byte, string, error) {
	compiled, err := e.runtime.CompileModule(ctx, source)
	if err != nil {
		return nil, "", kernel.ErrInvalidInput.Wrapf("wasm compile error: %v", err)
	}

	h := sha256.Sum256(source)
	hash := hex.EncodeToString(h[:])

	e.mu.Lock()
	e.cache[hash] = compiled
	e.mu.Unlock()

	return source, hash, nil
}

// Execute runs a compiled WASM artifact.
// The module must export a function `run(inputPtr, inputLen) (outputPtr, outputLen)`.
// Input/output are JSON bytes exchanged through linear memory.
func (e *Executor) Execute(ctx context.Context, artifact []byte, input []byte, host kernel.HostFunctions) ([]byte, error) {
	if e.cfg.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(e.cfg.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	h := sha256.Sum256(artifact)
	hash := hex.EncodeToString(h[:])

	e.mu.Lock()
	compiled, ok := e.cache[hash]
	e.mu.Unlock()

	if !ok {
		var err error
		compiled, err = e.runtime.CompileModule(ctx, artifact)
		if err != nil {
			return nil, kernel.ErrExecutionFailed.Wrapf("wasm recompile: %v", err)
		}
		e.mu.Lock()
		e.cache[hash] = compiled
		e.mu.Unlock()
	}

	// Carry this call's host functions through the context so the shared, persistent
	// "juice" host module (instantiated in New) dispatches to the right kernel state.
	// This is what lets executions run concurrently and nest without a global lock.
	ctx = context.WithValue(ctx, hostKey{}, host)

	// WithName("") leaves the guest module unregistered in the runtime's namespace,
	// so concurrent and nested guests never collide on a name.
	// WithStartFunctions() skips _start so proc_exit(0) never closes the module.
	// TinyGo's wasip1 runtime initializes lazily on first exported-function call.
	modCfg := wazero.NewModuleConfig().WithName("").WithStartFunctions()
	mod, err := e.runtime.InstantiateModule(ctx, compiled, modCfg)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("instantiate wasm module: %v", err)
	}
	defer mod.Close(ctx)

	// Allocate input in wasm memory via the exported `alloc` function.
	alloc := mod.ExportedFunction("alloc")
	if alloc == nil {
		return nil, kernel.ErrExecutionFailed.Wrap("wasm module missing export: alloc")
	}
	inputLen := uint64(len(input))
	res, err := alloc.Call(ctx, inputLen)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("alloc failed: %v", err)
	}
	inputPtr := res[0]

	mem := mod.Memory()
	if !mem.Write(uint32(inputPtr), input) {
		return nil, kernel.ErrExecutionFailed.Wrap("failed to write input to wasm memory")
	}

	// Call the exported `run` function.
	run := mod.ExportedFunction("run")
	if run == nil {
		return nil, kernel.ErrExecutionFailed.Wrap("wasm module missing export: run")
	}
	results, err := run.Call(ctx, inputPtr, inputLen)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("run failed: %v", err)
	}
	// Support two calling conventions:
	// - (i32, i32): hand-crafted WASM with multi-value return
	// - (i64):      TinyGo wasm-unknown packed return (upper 32 = ptr, lower 32 = len)
	var outputPtr, outputLen uint32
	switch len(results) {
	case 2:
		outputPtr, outputLen = uint32(results[0]), uint32(results[1])
	case 1:
		outputPtr, outputLen = uint32(results[0]>>32), uint32(results[0])
	default:
		return nil, kernel.ErrExecutionFailed.Wrap("run must return (ptr, len) or packed i64")
	}

	output, ok := mem.Read(outputPtr, outputLen)
	if !ok {
		return nil, kernel.ErrExecutionFailed.Wrap("failed to read output from wasm memory")
	}

	// Validate it's JSON.
	var check any
	if err := json.Unmarshal(output, &check); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrap("wasm output is not valid JSON")
	}
	return output, nil
}

// registerHostFunctions wires kernel.HostFunctions into the wazero host module.
// Each host function receives/returns JSON via wasm linear memory. The module is
// instantiated once and shared; each callback reads the active call's host from the
// execution context (see hostKey).
func registerHostFunctions(b wazero.HostModuleBuilder) {
	// juice.call(actionNamePtr, actionNameLen, argsPtr, argsLen) -> packedI64
	// Returns resultPtr in upper 32 bits and resultLen in lower 32 bits.
	// TinyGo //go:wasmimport only supports a single return value.
	b.NewFunctionBuilder().
		WithGoModuleFunction(
			api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
				host := hostFromCtx(ctx)
				namePtr, nameLen := uint32(stack[0]), uint32(stack[1])
				argsPtr, argsLen := uint32(stack[2]), uint32(stack[3])
				mem := mod.Memory()
				name, _ := mem.Read(namePtr, nameLen)
				args, _ := mem.Read(argsPtr, argsLen)
				result, err := host.Call(ctx, string(name), args)
				if err != nil {
					panic(err.Error())
				}
				ptrs := writeToMem(ctx, mod, result)
				stack[0] = ptrs[0]<<32 | ptrs[1]
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI64},
		).Export("call")

	// juice.log(levelPtr, levelLen, msgPtr, msgLen)
	b.NewFunctionBuilder().
		WithGoModuleFunction(
			api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
				host := hostFromCtx(ctx)
				lvlPtr, lvlLen := uint32(stack[0]), uint32(stack[1])
				msgPtr, msgLen := uint32(stack[2]), uint32(stack[3])
				mem := mod.Memory()
				level, _ := mem.Read(lvlPtr, lvlLen)
				msg, _ := mem.Read(msgPtr, msgLen)
				_ = host.Log(ctx, string(level), string(msg))
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export("log")

	// juice.step_create(partialArgsPtr, partialArgsLen,
	//                   requiredCallerPtr, requiredCallerLen, actionIDPtr, actionIDLen) -> packedI64
	b.NewFunctionBuilder().
		WithGoModuleFunction(
			api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
				host := hostFromCtx(ctx)
				mem := mod.Memory()
				partialArgs, _ := mem.Read(uint32(stack[0]), uint32(stack[1]))
				requiredCaller, _ := mem.Read(uint32(stack[2]), uint32(stack[3]))
				actionID, _ := mem.Read(uint32(stack[4]), uint32(stack[5]))
				stepID, err := host.StepCreate(ctx, partialArgs, string(requiredCaller), string(actionID))
				if err != nil {
					panic(err.Error())
				}
				ptrs := writeToMem(ctx, mod, []byte(`"`+stepID+`"`))
				stack[0] = ptrs[0]<<32 | ptrs[1]
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32,
				api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI64},
		).Export("step_create")

	// juice.step_complete(stepIDPtr, stepIDLen, inputPtr, inputLen) -> packedI64
	b.NewFunctionBuilder().
		WithGoModuleFunction(
			api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
				host := hostFromCtx(ctx)
				mem := mod.Memory()
				stepID, _ := mem.Read(uint32(stack[0]), uint32(stack[1]))
				input, _ := mem.Read(uint32(stack[2]), uint32(stack[3]))
				result, err := host.StepComplete(ctx, string(stepID), input)
				if err != nil {
					panic(err.Error())
				}
				ptrs := writeToMem(ctx, mod, result)
				stack[0] = ptrs[0]<<32 | ptrs[1]
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI64},
		).Export("step_complete")
}

// writeToMem writes data into the module's memory via `alloc` and returns (ptr, len).
func writeToMem(ctx context.Context, mod api.Module, data []byte) []uint64 {
	alloc := mod.ExportedFunction("alloc")
	if alloc == nil {
		return []uint64{0, 0}
	}
	res, err := alloc.Call(ctx, uint64(len(data)))
	if err != nil || len(res) == 0 {
		return []uint64{0, 0}
	}
	ptr := uint32(res[0])
	if !mod.Memory().Write(ptr, data) {
		return []uint64{0, 0}
	}
	return []uint64{uint64(ptr), uint64(len(data))}
}

// MemoryPages converts bytes to wasm memory pages (64KiB each), rounding up.
func MemoryPages(bytes int64) uint32 {
	if bytes <= 0 {
		return 1
	}
	pages := (bytes + 65535) / 65536
	if pages > 65536 {
		pages = 65536
	}
	return uint32(pages)
}
