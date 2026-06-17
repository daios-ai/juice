package kernel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

// CallRequest is input to the central Call() operation.
type CallRequest struct {
	// CallerID is the authenticated user making the call.
	CallerID string
	// ParentTraceID is the trace from which this call originates.
	// For subcalls it is the parent trace ID.
	// For step-completion calls it is set by BeginStepCall's trace.
	ParentTraceID string
	// Action, when non-nil, is the pre-validated action from beginRun.
	// Call uses it directly and skips the DB read, eliminating the TOCTOU window
	// between process/trace creation and execution.
	Action *Action
	// ActionRef is the action reference in "@owner/name" format.
	// When set, it is parsed into TargetUserID and ActionName inside Call.
	// Set either ActionRef or (TargetUserID + ActionName), not both.
	ActionRef string
	// TargetUserID is the owner of the action (handle or ID).
	TargetUserID string
	// ActionName is the action's name field.
	ActionName string
	// Args is the JSON-decoded input arguments.
	Args map[string]any
	// StepID, if non-empty, causes CommitCall/CommitFailedCall to atomically mark the step done.
	// Also signals CallerStep wallet kind (BeginStepCall was used, no lock to release).
	StepID string
	// ExistingTraceID, when non-empty, signals that the root trace was already created atomically
	// by BeginRun. Call uses this trace instead of calling BeginSubcall.
	ExistingTraceID string
	// IdempotencyRecordID, if non-empty, causes CommitCall/CommitFailedCall to atomically
	// mark the pending idempotency record as complete. Set only by federation handlers.
	IdempotencyRecordID string
}

// CallReply is the response from a successful Call().
type CallReply struct {
	Result    map[string]any `json:"result"`
	TxID      string         `json:"tx_id"`
	TraceID   string         `json:"trace_id"`
	ReceiptID string         `json:"receipt_id"`
}

// ParseActionRef splits an "@owner/name" action reference into owner handle and action name.
// Returns ErrInvalidInput if the format is invalid.
func ParseActionRef(ref string) (ownerHandle, actionName string, err error) {
	if !strings.HasPrefix(ref, "@") {
		return "", "", ErrInvalidInput.Wrap("action ref must be @owner/name")
	}
	idx := strings.Index(ref[1:], "/")
	if idx < 0 || ref[1:idx+1] == "" || ref[idx+2:] == "" {
		return "", "", ErrInvalidInput.Wrap("action ref must be @owner/name")
	}
	return ref[:idx+1], ref[idx+2:], nil
}

// Call executes the central kernel transition.
// Preconditions are checked in order per §5.1 of the requirements.
// For root calls (req.ExistingTraceID), the process and trace must already have been created by Run().
// For subcalls, the parent trace must have sufficient available funds.
// For step-completion calls, BeginStepCall must have been called before invoking Call.
func (k *Kernel) Call(ctx context.Context, req CallRequest) (*CallReply, error) {
	logger := k.log.With(ctx)

	// 1. Subject must be authenticated.
	if req.CallerID == "" {
		return nil, ErrUnauthenticated.Wrap("subject is required")
	}
	if _, err := k.requireActiveUser(ctx, req.CallerID); err != nil {
		return nil, err
	}

	// 1.5: Derive processID from the trace reference; for subcalls also read the parent trace.
	var processID string
	var preReadParent *Trace
	switch {
	case req.ExistingTraceID != "":
		rt, err := k.store.ReadTrace(ctx, req.ExistingTraceID)
		if err != nil {
			return nil, ErrNotFound.Wrap("trace not found")
		}
		processID = rt.ProcessID
	case req.StepID != "":
		if req.ParentTraceID == "" {
			return nil, ErrInvalidInput.Wrap("ParentTraceID required for step calls")
		}
		st, err := k.store.ReadTrace(ctx, req.ParentTraceID)
		if err != nil {
			return nil, ErrNotFound.Wrap("step trace not found")
		}
		processID = st.ProcessID
	case req.ParentTraceID != "":
		pt, err := k.store.ReadTrace(ctx, req.ParentTraceID)
		if err != nil {
			return nil, ErrInvalidInput.Wrap("parent trace not found")
		}
		processID = pt.ProcessID
		preReadParent = pt
	default:
		return nil, ErrInvalidInput.Wrap("no trace reference provided")
	}

	// 2. Process must exist and be open.
	process, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}

	// 3. For subcalls: validate process-use authority.
	// Root calls (ExistingTraceID) and step-completion calls skip this check.
	var parentTrace *Trace
	if req.ExistingTraceID == "" && req.StepID == "" {
		// preReadParent is the parent trace (derived processID came from it, so membership is implicit).
		// Non-owner callers must have action_owner_id on the parent trace.
		if process.OwnerUserID != req.CallerID && preReadParent.ActionOwnerID != req.CallerID {
			return nil, ErrUnauthorized.Wrap("caller is not authorized to use this process")
		}
		parentTrace = preReadParent
	}

	// 4. Resolve action.
	// Root calls and step completions supply a pre-resolved Action (validated by beginRun /
	// read by CompleteStep), so no DB read is needed — this binds execution to the exact action
	// that was funded and eliminates the TOCTOU window. canCall still runs (gated below).
	// Subcalls and direct test invocations use the owner/name path.
	var action *Action
	var target *User
	if req.Action != nil {
		action = req.Action
		target, err = k.store.ReadUser(ctx, action.OwnerUserID)
		if err != nil || target == nil {
			return nil, ErrNotFound.Wrap("target user not found")
		}
	} else {
		if req.ActionRef != "" {
			var parseErr error
			req.TargetUserID, req.ActionName, parseErr = ParseActionRef(req.ActionRef)
			if parseErr != nil {
				return nil, parseErr
			}
		}
		target, err = k.store.ReadUserByHandle(ctx, req.TargetUserID)
		if err != nil || target == nil {
			target, err = k.store.ReadUser(ctx, req.TargetUserID)
			if err != nil || target == nil {
				return nil, ErrNotFound.Wrap("target user not found")
			}
		}
		action, err = k.store.ReadActionByOwnerName(ctx, target.ID, req.ActionName)
		if err != nil || action == nil {
			return nil, ErrNotFound.Wrapf("action %s/%s not found", req.TargetUserID, req.ActionName)
		}
	}

	// 5. CanCall(process.owner, action). Skip for root calls: beginRun already validated this,
	// and making it single-pass eliminates the TOCTOU window between BeginRun and Call.
	if req.ExistingTraceID == "" && !canCall(process.OwnerUserID, action) {
		if !action.Active {
			return nil, ErrInvalidState.Wrap("action is inactive")
		}
		return nil, ErrUnauthorized.Wrap("call permission denied")
	}

	// 6. Validate input schema. Skip for root calls: same reason as step 5.
	if req.ExistingTraceID == "" {
		if err := ValidateInput(action.InputSchema, req.Args); err != nil {
			return nil, err
		}
	}

	// 7. Funds check (step calls pre-funded by BeginStepCall; ExistingTraceID calls pre-funded by BeginRun).
	if req.StepID == "" && req.ExistingTraceID == "" {
		if parentTrace != nil && parentTrace.Available < action.Price {
			return nil, ErrInsufficientFunds.Wrapf("parent trace has %d credits, action costs %d", parentTrace.Available, action.Price)
		}
	}

	// Kernel must be bootstrapped. Skip for ExistingTraceID calls — beginRun already
	// verified this, and the signing key cannot change at runtime.
	if req.ExistingTraceID == "" {
		if err := k.requireReceiptSigningReady(); err != nil {
			return nil, err
		}
	}

	// 8. Atomically lock funds and create child trace.
	now := time.Now().UTC()
	var parentTracePtr *string
	if req.ParentTraceID != "" {
		s := req.ParentTraceID
		parentTracePtr = &s
	}
	trace := &Trace{
		ID:            uuid.New().String(),
		ProcessID:     processID,
		ParentTraceID: parentTracePtr,
		ActionOwnerID: action.OwnerUserID,
		ActionID:      action.ID,
		CallerUserID:  req.CallerID,
		CreatedAt:     now,
	}

	// For remote_proxy: action.Price = q = proxyPrice (mp + import duty), set at ImportRemoteAction.
	// Derive the original remote manifest price (mp) from q for clamping and receipt audit.
	// lockPrice = q (already correct; no re-addition of duty).
	lockPrice := action.Price
	var mp int64 = action.Price // for non-remote-proxy: mp unused; for remote-proxy: corrected below
	if action.Kind == KindRemoteProxy {
		// mp_original = floor(q * 10000 / (10000 + ImportBPS))
		mp = action.Price * 10000 / (10000 + k.cfg.ImportBPS)
		key := uuid.New().String()
		trace.IdempotencyKey = &key
		djsonBytes, _ := json.Marshal(map[string]any{
			"args":         req.Args,
			"step_id":      req.StepID,
			"remote_price": mp,
		})
		djson := string(djsonBytes)
		trace.DispatchJSON = &djson
	}

	callerWalletID, callerWalletKind := k.callerWallet(req, process, parentTrace)

	switch {
	case req.StepID != "":
		// BeginStepCall was already called by CompleteStep; skip BeginRootCall/BeginSubcall.
		// trace was created by BeginStepCall; use the trace ID from req.
		trace.ID = req.ParentTraceID // for step calls, ParentTraceID IS the new trace (set by CompleteStep)
		// Fetch the real parent_trace_id from DB so the tx records it correctly (not a self-reference).
		if dbTrace, err := k.store.ReadTrace(ctx, trace.ID); err == nil {
			trace.ParentTraceID = dbTrace.ParentTraceID
			trace.IdempotencyKey = dbTrace.IdempotencyKey
			trace.DispatchJSON = dbTrace.DispatchJSON
			trace.Available = dbTrace.Available
			// BeginStepCall funded this trace with exactly step.price, so gross must be that
			// snapshot — not the action's current price, which may have changed since step creation.
			lockPrice = dbTrace.Available
		}
	case req.ExistingTraceID != "":
		// Root trace was pre-created atomically by BeginRun; load its full state.
		trace.ID = req.ExistingTraceID
		if dbTrace, err := k.store.ReadTrace(ctx, req.ExistingTraceID); err == nil {
			trace.Available      = dbTrace.Available
			lockPrice            = dbTrace.Available // use the pre-locked amount, not current action.Price
			trace.IdempotencyKey = dbTrace.IdempotencyKey
			trace.DispatchJSON   = dbTrace.DispatchJSON
		}
	default:
		if err := k.store.BeginSubcall(ctx, req.ParentTraceID, trace, lockPrice); err != nil {
			if errors.Is(err, ErrInsufficientFunds) {
				return nil, err
			}
			return nil, ErrInternal.Wrap("could not begin subcall")
		}
	}

	ctx = log.WithProcessID(ctx, processID)
	ctx = log.WithCallerUserID(ctx, req.CallerID)
	ctx = log.WithCallerHandle(ctx, k.callerHandle(ctx, req.CallerID))
	ctx = log.WithTraceID(ctx, trace.ID)
	ctx = log.WithActionID(ctx, action.ID)
	logger = k.log.With(ctx)
	logger.Info("call.start", "action", action.Name, "price", lockPrice)

	txID := uuid.New().String()
	var parentTraceIDStr string
	if trace.ParentTraceID != nil {
		parentTraceIDStr = *trace.ParentTraceID
	}
	ktx := &Transaction{
		ID:             txID,
		ProcessID:      processID,
		TraceID:        trace.ID,
		ParentTraceID:  parentTraceIDStr,
		OwnerUserID:    process.OwnerUserID,
		CallerUserID:   req.CallerID,
		TargetUserID:   target.ID,
		ActionID:       action.ID,
		ActionName:     action.Name,
		RemoteActionID: action.RemoteActionID,
		Status:         TxFailure,
		Gross:          lockPrice,
		StartedAt:      now,
	}
	argsJSON, _ := json.Marshal(req.Args)
	ktx.ArgsJSON = json.RawMessage(argsJSON)

	// 9. Execute. Remote proxy calls use ExecuteFederation directly with the stored idempotency key.
	started := time.Now()

	if action.Kind == KindRemoteProxy {
		fe, ok := k.http.(FederationExecutor)
		if !ok {
			// The trace is already funded (BeginSubcall / BeginRun). Returning here without
			// settling would commit no transaction and strand the locked allocation. Route the
			// misconfiguration through the normal failure path so a failure tx + receipt commits
			// and the funds refund — the "no settlement" rule is only for a network timeout
			// awaiting a remote receipt, not a local adapter being absent.
			cfgErr := ErrInvalidState.Wrap("federation executor not configured")
			ktx.Status = TxFailure
			ktx.Reason = cfgErr.Error()
			ktx.EndedAt = time.Now().UTC()
			if err := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, 0, cfgErr); err != nil {
				return nil, err
			}
			return nil, cfgErr
		}
		ikey := ""
		if trace.IdempotencyKey != nil {
			ikey = *trace.IdempotencyKey
		}
		fr, _ := fe.ExecuteFederation(ctx, action.Source, ikey, req.Args)
		latency := time.Since(started).Seconds()
		ktx.EndedAt = time.Now().UTC()
		return k.settleRemoteCall(ctx, logger, action, ktx, trace, callerWalletID, callerWalletKind, req, target, mp, fr, latency)
	}

	reply, _, execErr := k.execute(ctx, action, req.Args, trace, action.OwnerUserID, req.CallerID, process.OwnerUserID)
	latency := time.Since(started).Seconds()
	ktx.EndedAt = time.Now().UTC()

	if execErr != nil {
		ktx.Status = TxFailure
		ktx.Reason = execErr.Error()
		if err := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, execErr); err != nil {
			return nil, err
		}
		logger.Warn("call.failed", "action", action.Name, "error", execErr)
		return nil, execErr
	}

	// 10. Validate output schema.
	if schemaErr := ValidateInput(action.OutputSchema, any(reply)); schemaErr != nil {
		ktx.Status = TxFailure
		ktx.Reason = "output schema violation: " + schemaErr.Error()
		if err := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, schemaErr); err != nil {
			return nil, err
		}
		return nil, schemaErr
	}

	// 11. Read trace.available post-execution — this is the taxable amount.
	// trace.available decreases with each subcall (BeginSubcall) and step park (CreateStep).
	postTrace, readErr := k.store.ReadTrace(ctx, trace.ID)
	if readErr != nil {
		// If we can't read the trace, settle as failure to avoid fund loss.
		ktx.Status = TxFailure
		ktx.Reason = "could not read trace post-execution"
		_ = k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, readErr)
		return nil, ErrInternal.Wrap("could not read trace")
	}
	taxable := postTrace.Available
	net, fee := ComputeFee(taxable, k.cfg.FeeBPS)

	replyJSON, _ := json.Marshal(reply)
	ktx.ReplyJSON = json.RawMessage(replyJSON)
	ktx.Status = TxSuccess
	ktx.Net = net
	ktx.Fee = fee
	stats := k.computeStats(ctx, action.ID, ktx, latency)
	receipt, receiptErr := k.buildReceipt(ktx, ktx.Gross) // success: charge = gross
	if receiptErr != nil {
		ktx.Status = TxFailure
		ktx.Reason = "could not build receipt"
		_ = k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, receiptErr)
		return nil, ErrInternal.Wrap("could not build receipt")
	}
	if err := k.store.CommitCall(ctx, ktx, receipt, trace.ID, callerWalletID, callerWalletKind, target.ID, k.cfg.FeeRecipientID, net, fee, stats, req.IdempotencyRecordID, req.StepID); err != nil {
		return nil, ErrInternal.Wrap("could not commit transaction")
	}

	logger.Info("call.success", "action", action.Name, "tx_id", txID, "latency_ms", latency*1000)

	return &CallReply{
		Result:    reply,
		TxID:      txID,
		TraceID:   trace.ID,
		ReceiptID: receipt.ID,
	}, nil
}

// callerWallet returns the callerWalletID and callerWalletKind for CommitCall/CommitFailedCall.
func (k *Kernel) callerWallet(req CallRequest, process *Process, parentTrace *Trace) (id, kind string) {
	if req.StepID != "" {
		return "", CallerStep
	}
	if req.ExistingTraceID != "" {
		return process.ID, CallerProcess
	}
	if parentTrace != nil {
		return parentTrace.ID, CallerTrace
	}
	return process.ID, CallerProcess
}

// canCall returns true iff the action is callable by a process owned by ownerID.
// CanCall(ownerID, a) := active(a) ∧ (public(a) ∨ ownerID = a.OwnerUserID)
func canCall(ownerID string, action *Action) bool {
	return action.Active && (action.Public || ownerID == action.OwnerUserID)
}

// execute dispatches to the correct execution backend for HTTP, WASM, and native actions.
// KindRemoteProxy is handled separately in Call() via ExecuteFederation.
func (k *Kernel) execute(ctx context.Context, action *Action, args map[string]any, trace *Trace, targetID, callerID, ownerUserID string) (map[string]any, string, error) {
	switch action.Kind {
	case KindHTTP:
		if k.http == nil {
			return nil, "", ErrInvalidState.Wrap("HTTP executor not configured")
		}
		res, err := k.http.Execute(ctx, action, args)
		return res, "", err
	case KindWasm:
		res, err := k.executeWasm(ctx, action, args, trace, targetID)
		return res, "", err
	case KindNative:
		res, err := k.executeNative(ctx, action, args, targetID, callerID, ownerUserID, trace.ProcessID, trace.ID)
		return res, "", err
	default:
		return nil, "", ErrInvalidState.Wrapf("unknown action kind %q", action.Kind)
	}
}

// executeNative dispatches to a registered native action handler.
func (k *Kernel) executeNative(ctx context.Context, action *Action, args map[string]any, targetID, callerID, ownerUserID, processID, parentTraceID string) (map[string]any, error) {
	fn, ok := k.nativeHandlers[action.Name]
	if !ok {
		return nil, ErrInvalidState.Wrapf("unknown native action %q", action.Name)
	}
	return fn(ctx, args, targetID, callerID, ownerUserID, processID, parentTraceID)
}

// executeWasm runs a compiled WASM artifact.
func (k *Kernel) executeWasm(ctx context.Context, action *Action, args map[string]any, trace *Trace, targetID string) (result map[string]any, execErr error) {
	defer func() {
		if r := recover(); r != nil {
			execErr = ErrExecutionFailed.Wrapf("wasm panic: %v", r)
		}
	}()
	if k.scripts == nil {
		return nil, ErrInvalidState.Wrap("script executor not configured")
	}

	inputJSON, err := json.Marshal(args)
	if err != nil {
		return nil, ErrInvalidInput.Wrap("could not serialize args")
	}

	wasmBytes := []byte(action.Source)
	if action.WasmArtifact != "" {
		decoded, decErr := base64.StdEncoding.DecodeString(action.WasmArtifact)
		if decErr != nil {
			return nil, ErrExecutionFailed.Wrapf("wasm artifact decode failed: %v", decErr)
		}
		wasmBytes = decoded
	}
	artifact, _, err := k.scripts.Compile(ctx, wasmBytes)
	if err != nil {
		return nil, ErrExecutionFailed.Wrapf("wasm compile failed: %v", err)
	}

	host := &kernelHostFunctions{
		kernel:   k,
		traceID:  trace.ID,
		targetID: targetID,
	}

	outputJSON, err := k.scripts.Execute(ctx, artifact, inputJSON, host)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout.Wrapf("wasm execution timed out: %v", err)
		}
		return nil, ErrExecutionFailed.Wrapf("wasm execution failed: %v", err)
	}

	if err := json.Unmarshal(outputJSON, &result); err != nil {
		return nil, ErrExecutionFailed.Wrap("wasm output is not valid JSON")
	}
	return result, nil
}

// kernelHostFunctions implements HostFunctions using the kernel itself.
// Scripts never receive the caller's JWT — they inherit trace authority.
type kernelHostFunctions struct {
	kernel   *Kernel
	traceID  string
	targetID string // action owner; used as CallerID for subcalls
}

func (h *kernelHostFunctions) Call(ctx context.Context, actionName string, argsJSON []byte) ([]byte, error) {
	parts := strings.SplitN(actionName, "/", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidInput.Wrap("actionName must be handle/name")
	}
	var args map[string]any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return nil, ErrInvalidInput.Wrap("args must be a JSON object")
	}
	reply, err := h.kernel.Call(ctx, CallRequest{
		CallerID:      h.targetID,
		ParentTraceID: h.traceID,
		TargetUserID:  parts[0],
		ActionName:    parts[1],
		Args:          args,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(reply.Result)
}

func (h *kernelHostFunctions) StepCreate(ctx context.Context, partialArgs []byte, requiredCallerUserID, actionID string) (string, error) {
	step, err := h.kernel.CreateStep(ctx, h.traceID, actionID,
		json.RawMessage(partialArgs), requiredCallerUserID)
	if err != nil {
		return "", err
	}
	return step.ID, nil
}

func (h *kernelHostFunctions) StepComplete(ctx context.Context, stepID string, input []byte) ([]byte, error) {
	reply, err := h.kernel.CompleteStep(ctx, h.targetID, stepID, json.RawMessage(input))
	if err != nil {
		return nil, err
	}
	return json.Marshal(reply)
}

func (h *kernelHostFunctions) Log(ctx context.Context, level, msg string) error {
	switch level {
	case "debug":
		h.kernel.log.Debug(msg, "trace_id", h.traceID)
	case "warn":
		h.kernel.log.Warn(msg, "trace_id", h.traceID)
	case "error":
		h.kernel.log.Error(msg, "trace_id", h.traceID)
	default:
		h.kernel.log.Info(msg, "trace_id", h.traceID)
	}
	return nil
}

// computeStats builds a delta Stats for this call outcome. The SQL in CommitCall/CommitFailedCall
// applies these as incremental updates, making concurrent calls safe.
func (k *Kernel) computeStats(_ context.Context, actionID string, tx *Transaction, latency float64) *Stats {
	if actionID == "" {
		return nil
	}
	stats := DefaultStats(actionID)
	UpdateStats(stats, tx, latency)
	return stats
}

// settleFailedCall commits the failed transaction atomically.
// The receipt is built inside CommitFailedCall's transaction so that the signed charge
// (gross − refund) is guaranteed to match what is committed.
// tx.Status and tx.Reason must be set by the caller before invoking this.
func (k *Kernel) settleFailedCall(ctx context.Context, logger *log.Logger, tx *Transaction, traceID, callerWalletID, callerWalletKind string, req CallRequest, action *Action, latency float64, callErr error) error {
	if len(tx.ReplyJSON) == 0 {
		tx.ReplyJSON = json.RawMessage("null")
	}
	// Pre-settle any unsettled direct child traces (e.g. remote subcalls that timed out).
	// recoverTrace settles each child as a failure, crediting its refund back into this
	// trace's available and zeroing its locked. CommitFailedCall below then includes those
	// funds in the refund it returns to the caller, preventing stranded allocations.
	if children, childErr := k.store.ListDirectUnsettledChildren(ctx, traceID); childErr == nil {
		for _, child := range children {
			if err := k.recoverTrace(ctx, logger, child, "parent call failed", ""); err != nil {
				logger.Error("call.pre_settle_child_failed", "child_trace_id", child.ID, "error", err)
			}
		}
	}
	stats := k.computeStats(ctx, action.ID, tx, latency)
	buildFn := func(refund int64) (*Receipt, error) {
		return k.buildReceipt(tx, tx.Gross-refund)
	}
	if settlErr := k.store.CommitFailedCall(ctx, tx, buildFn, traceID, callerWalletID, callerWalletKind, tx.Gross, stats, req.IdempotencyRecordID, KernelErrorCode(callErr), req.StepID); settlErr != nil {
		logger.Error("call.settlement_failed", "action", action.Name, "error", callErr, "settlement_error", settlErr)
		return ErrInternal.Wrap("could not record failure transaction")
	}
	return nil
}

// ComputeFee computes (net, fee) from the taxable amount (= trace.available post-execution).
// fee = ceil(taxable * feeBPS / 10000). Invariant: net + fee == taxable.
func ComputeFee(taxable, feeBPS int64) (net, fee int64) {
	if taxable == 0 || feeBPS == 0 {
		return taxable, 0
	}
	fee = (taxable*feeBPS + 9999) / 10000
	net = taxable - fee
	return
}
