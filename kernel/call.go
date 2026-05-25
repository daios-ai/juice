package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

// CallRequest is input to the central Call() operation.
type CallRequest struct {
	// SubjectID is the authenticated user making the call.
	SubjectID string
	// ProcessID is the budgeted execution context.
	ProcessID string
	// ParentTraceID is the trace from which this call originates.
	// For top-level calls it is the process root trace ID.
	ParentTraceID string
	// TargetUserID is the owner of the action.
	TargetUserID string
	// ActionName is the action's name field.
	ActionName string
	// Args is the JSON-decoded input arguments.
	Args map[string]any
}

// CallReply is the response from a successful Call().
type CallReply struct {
	Result  map[string]any
	TxID    string
	TraceID string
}

// Call executes the central kernel transition.
// Preconditions are checked in order per Section 5.1 of the requirements.
func (k *Kernel) Call(ctx context.Context, req CallRequest) (*CallReply, error) {
	logger := k.log.With(ctx)

	// 1. Subject must be authenticated (already resolved by caller; just validate non-empty).
	if req.SubjectID == "" {
		return nil, ErrUnauthenticated.Wrap("subject is required")
	}

	// 2. Process must exist and be open.
	process, err := k.store.ReadProcess(ctx, req.ProcessID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}

	// 3. Subject is the process owner.
	if process.OwnerUserID != req.SubjectID {
		return nil, ErrUnauthorized.Wrap("subject is not the process owner")
	}

	// 4. Resolve action.
	target, err := k.store.ReadUserByHandle(ctx, req.TargetUserID)
	if err != nil || target == nil {
		// Also try reading by ID.
		target, err = k.store.ReadUser(ctx, req.TargetUserID)
		if err != nil || target == nil {
			return nil, ErrNotFound.Wrap("target user not found")
		}
	}

	action, err := k.store.ReadActionByOwnerName(ctx, target.ID, req.ActionName)
	if err != nil || action == nil {
		return nil, ErrNotFound.Wrapf("action %s/%s not found", req.TargetUserID, req.ActionName)
	}

	// 5. Action must be active (non-owners only).
	if !action.Active && action.OwnerUserID != req.SubjectID {
		return nil, ErrNotFound.Wrapf("action %s/%s not found", req.TargetUserID, req.ActionName)
	}

	// 6. ACL check — CanCall(subject, action).
	if action.OwnerUserID != req.SubjectID {
		canCall, err := k.canCall(ctx, req.SubjectID, action.ID)
		if err != nil {
			return nil, err
		}
		if !canCall {
			return nil, ErrUnauthorized.Wrap("call permission denied")
		}
	}

	// 7. Validate input schema before locking funds.
	if err := ValidateInput(action.InputSchema, req.Args); err != nil {
		return nil, err
	}

	// 8. Check process has sufficient available funds.
	if process.Available < action.Price {
		return nil, ErrInsufficientFunds.Wrapf("process has %d credits, action costs %d", process.Available, action.Price)
	}

	// 9. Lock funds atomically.
	if action.Price > 0 {
		if err := k.store.LockFunds(ctx, req.ProcessID, action.Price); err != nil {
			return nil, ErrInsufficientFunds.Wrap("could not lock funds")
		}
	}

	// 10. Create child trace.
	now := time.Now().UTC()
	trace := &Trace{
		ID:            uuid.New().String(),
		ProcessID:     req.ProcessID,
		ParentTraceID: req.ParentTraceID,
		CreatedAt:     now,
	}
	if err := k.store.CreateTrace(ctx, trace); err != nil {
		_ = k.store.RefundFunds(ctx, req.ProcessID, action.Price)
		return nil, ErrInternal.Wrap("could not create trace")
	}

	ctx = log.WithTraceID(ctx, trace.ID)
	ctx = log.WithActionID(ctx, action.ID)
	logger = k.log.With(ctx)
	logger.Info("call.start", "action", action.Name, "price", action.Price)

	// Prepare transaction skeleton.
	net, fee := ComputeFee(action.Price, k.cfg.FeeBPS)
	txID := uuid.New().String()
	tx := &Transaction{
		ID:            txID,
		ProcessID:     req.ProcessID,
		TraceID:       trace.ID,
		ParentTraceID: req.ParentTraceID,
		OwnerUserID:   process.OwnerUserID,
		SubjectUserID: req.SubjectID,
		TargetUserID:  target.ID,
		ActionID:      action.ID,
		Status:        TxFailure,
		Gross:         0, // will be set on success
		Net:           0,
		Fee:           0,
		StartedAt:     now,
	}

	argsJSON, _ := json.Marshal(req.Args)
	tx.ArgsJSON = string(argsJSON)

	// 11. Execute.
	started := time.Now()
	reply, execErr := k.execute(ctx, action, req.Args, trace, process.OwnerUserID)
	latency := time.Since(started).Seconds()
	tx.EndedAt = time.Now().UTC()

	// Handle execution failure.
	if execErr != nil {
		_ = k.store.RefundFunds(ctx, req.ProcessID, action.Price)
		tx.Status = TxFailure
		tx.Reason = execErr.Error()
		_ = k.store.CreateTransaction(ctx, tx)
		k.updateStats(ctx, action.ID, tx, latency)
		logger.Warn("call.failed", "action", action.Name, "error", execErr)
		return nil, execErr
	}

	// 12. Validate output schema.
	if err := ValidateInput(action.OutputSchema, anyOf(reply)); err != nil {
		_ = k.store.RefundFunds(ctx, req.ProcessID, action.Price)
		tx.Status = TxFailure
		tx.Reason = "output schema violation: " + err.Error()
		_ = k.store.CreateTransaction(ctx, tx)
		k.updateStats(ctx, action.ID, tx, latency)
		return nil, err
	}

	// 13. Record successful transaction.
	replyJSON, _ := json.Marshal(reply)
	tx.ReplyJSON = string(replyJSON)
	tx.Status = TxSuccess
	tx.Gross = action.Price
	tx.Net = net
	tx.Fee = fee
	if err := k.store.CreateTransaction(ctx, tx); err != nil {
		_ = k.store.RefundFunds(ctx, req.ProcessID, action.Price)
		return nil, ErrInternal.Wrap("could not record transaction")
	}

	// 14. Settle payment.
	if action.Price > 0 {
		if err := k.store.SettleCall(ctx, req.ProcessID, target.ID, k.cfg.FeeRecipientID, net, fee); err != nil {
			logger.Error("call.settle_failed", "error", err)
			// Transaction is already recorded; log and continue.
		}
	}

	// 15. Update stats.
	k.updateStats(ctx, action.ID, tx, latency)

	logger.Info("call.success", "action", action.Name, "tx_id", txID, "latency_ms", fmt.Sprintf("%.1f", latency*1000))

	return &CallReply{
		Result:  reply,
		TxID:    txID,
		TraceID: trace.ID,
	}, nil
}

// canCall checks ACL(subject, action, call) OR ACL(subject, action, admin).
func (k *Kernel) canCall(ctx context.Context, subjectID, actionID string) (bool, error) {
	ok, err := k.store.CheckACL(ctx, subjectID, actionID, PermCall)
	if err != nil {
		return false, ErrInternal.Wrapf("acl check: %v", err)
	}
	if ok {
		return true, nil
	}
	ok, err = k.store.CheckACL(ctx, subjectID, actionID, PermAdmin)
	if err != nil {
		return false, ErrInternal.Wrapf("acl check: %v", err)
	}
	return ok, nil
}

// execute dispatches to the correct execution backend.
func (k *Kernel) execute(ctx context.Context, action *Action, args map[string]any, trace *Trace, ownerUserID string) (map[string]any, error) {
	switch action.Kind {
	case KindHTTP:
		return k.executeHTTP(ctx, action, args)
	case KindWasm:
		return k.executeWasm(ctx, action, args, trace, ownerUserID)
	case KindNative:
		return nil, ErrInvalidState.Wrap("native actions cannot be called directly")
	default:
		return nil, ErrInvalidState.Wrapf("unknown action kind %q", action.Kind)
	}
}

// executeHTTP calls an external HTTP endpoint.
func (k *Kernel) executeHTTP(ctx context.Context, action *Action, args map[string]any) (map[string]any, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, ErrInvalidInput.Wrap("could not serialize args")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action.Source, strings.NewReader(string(body)))
	if err != nil {
		return nil, ErrInvalidInput.Wrapf("invalid action URL: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrExecutionFailed.Wrapf("HTTP call failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, ErrExecutionFailed.Wrap("could not read response body")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, ErrExecutionFailed.Wrapf("action returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, ErrExecutionFailed.Wrap("action response is not valid JSON")
	}
	return result, nil
}

// executeWasm runs a compiled WASM artifact.
func (k *Kernel) executeWasm(ctx context.Context, action *Action, args map[string]any, trace *Trace, ownerUserID string) (map[string]any, error) {
	if k.scripts == nil {
		return nil, ErrInvalidState.Wrap("script executor not configured")
	}

	inputJSON, err := json.Marshal(args)
	if err != nil {
		return nil, ErrInvalidInput.Wrap("could not serialize args")
	}

	host := &kernelHostFunctions{
		kernel:      k,
		processID:   trace.ProcessID,
		traceID:     trace.ID,
		ownerUserID: ownerUserID,
	}

	outputJSON, err := k.scripts.Execute(ctx, []byte(action.Source), inputJSON, host)
	if err != nil {
		return nil, ErrExecutionFailed.Wrapf("wasm execution failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(outputJSON, &result); err != nil {
		return nil, ErrExecutionFailed.Wrap("wasm output is not valid JSON")
	}
	return result, nil
}

// kernelHostFunctions implements HostFunctions using the kernel itself.
// Scripts never receive the subject's JWT — they inherit process+trace authority.
type kernelHostFunctions struct {
	kernel      *Kernel
	processID   string
	traceID     string
	ownerUserID string
}

func (h *kernelHostFunctions) Call(ctx context.Context, actionName string, argsJSON []byte) ([]byte, error) {
	// Parse "handle/action-name" form.
	parts := strings.SplitN(actionName, "/", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidInput.Wrap("actionName must be handle/name")
	}

	p, err := h.kernel.store.ReadProcess(ctx, h.processID)
	if err != nil {
		return nil, err
	}

	var args map[string]any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return nil, ErrInvalidInput.Wrap("args must be a JSON object")
	}

	reply, err := h.kernel.Call(ctx, CallRequest{
		SubjectID:     p.OwnerUserID,
		ProcessID:     h.processID,
		ParentTraceID: h.traceID,
		TargetUserID:  parts[0],
		ActionName:    "/" + parts[1],
		Args:          args,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(reply.Result)
}

func (h *kernelHostFunctions) Emit(ctx context.Context, event string, argsJSON []byte) error {
	var args map[string]any
	if len(argsJSON) > 0 {
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return ErrInvalidInput.Wrap("emit args must be a JSON object")
		}
	}
	_, err := h.kernel.EmitEvent(ctx, h.ownerUserID, event, args)
	return err
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

func (h *kernelHostFunctions) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, ErrInvalidState.Wrap("get/put not yet implemented")
}

func (h *kernelHostFunctions) Put(_ context.Context, _ string, _ []byte) error {
	return ErrInvalidState.Wrap("get/put not yet implemented")
}

// updateStats applies call outcome to action statistics.
func (k *Kernel) updateStats(ctx context.Context, actionID string, tx *Transaction, latency float64) {
	stats, err := k.store.ReadStats(ctx, actionID)
	if err != nil || stats == nil {
		stats = DefaultStats(actionID)
	}
	UpdateStats(stats, tx, latency)
	_ = k.store.UpsertStats(ctx, stats)
}

// anyOf converts a map[string]any to any for schema validation.
func anyOf(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// ComputeFee splits a gross amount into (net, fee) using basis points.
// fee is rounded up to the nearest credit (ceiling division).
// Invariant: net + fee == gross.
func ComputeFee(gross, feeBPS int64) (net, fee int64) {
	if gross == 0 || feeBPS == 0 {
		return gross, 0
	}
	fee = (gross*feeBPS + 9999) / 10000
	net = gross - fee
	return
}
