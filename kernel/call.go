package kernel

import (
	"context"
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
	// ProcessID is the budgeted execution context.
	ProcessID string
	// ParentTraceID is the trace from which this call originates.
	// For top-level calls it is the process root trace ID.
	// For event-triggered calls it may reference a trace in another process.
	ParentTraceID string
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
	// EventID, if non-empty, causes CommitCall to settle the event atomically.
	// Set only by ConsumeEvent; leave empty for all direct calls.
	EventID string
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
	Gross     int64          `json:"gross"`
}

// Call executes the central kernel transition.
// Preconditions are checked in order per Section 5.1 of the requirements.
func (k *Kernel) Call(ctx context.Context, req CallRequest) (*CallReply, error) {
	logger := k.log.With(ctx)

	// 1. Subject must be authenticated: non-empty, exists, and not suspended.
	if req.CallerID == "" {
		return nil, ErrUnauthenticated.Wrap("subject is required")
	}
	if _, err := k.requireActiveUser(ctx, req.CallerID); err != nil {
		return nil, err
	}

	// 2. Process must exist and be open.
	process, err := k.store.ReadProcess(ctx, req.ProcessID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}

	// 3. Subject may use this process: is the owner, or owns the action executing in the parent trace.
	if process.OwnerUserID != req.CallerID {
		authorized := false
		if req.ParentTraceID != "" {
			parent, parentErr := k.store.ReadTrace(ctx, req.ParentTraceID)
			if parentErr == nil && parent.ActionOwnerID == req.CallerID {
				authorized = true
			}
		}
		if !authorized {
			return nil, ErrUnauthorized.Wrap("caller is not the process owner")
		}
	}

	// 4. Resolve action.
	// If ActionRef is set ("@owner/name"), parse it into TargetUserID and ActionName.
	if req.ActionRef != "" {
		ref := req.ActionRef
		if !strings.HasPrefix(ref, "@") {
			return nil, ErrInvalidInput.Wrap("action ref must be @owner/name")
		}
		idx := strings.Index(ref[1:], "/")
		if idx < 0 || ref[1:idx+1] == "" || ref[idx+2:] == "" {
			return nil, ErrInvalidInput.Wrap("action ref must be @owner/name")
		}
		req.TargetUserID = ref[:idx+1] // handle including @
		req.ActionName = ref[idx+2:]
	}
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

	// 5. CanCall(process.owner, action) — active(a) ∧ (public(a) ∨ owner = action.owner)
	if !canCall(process.OwnerUserID, action) {
		if !action.Active {
			return nil, ErrInvalidState.Wrap("action is inactive")
		}
		return nil, ErrUnauthorized.Wrap("call permission denied")
	}

	// 7. Validate input schema before locking funds.
	if err := ValidateInput(action.InputSchema, req.Args); err != nil {
		return nil, err
	}

	// 8. Check process has sufficient available funds.
	if process.Available < action.Price {
		return nil, ErrInsufficientFunds.Wrapf("process has %d credits, action costs %d", process.Available, action.Price)
	}

	// 9. Validate or resolve parent trace — precondition check, no state change yet.
	if req.ParentTraceID != "" {
		if _, err := k.store.ReadTrace(ctx, req.ParentTraceID); err != nil {
			return nil, ErrInvalidInput.Wrap("parent trace not found")
		}
	} else {
		root, err := k.store.ReadRootTrace(ctx, req.ProcessID)
		if err != nil {
			return nil, ErrInternal.Wrap("could not resolve root trace for process")
		}
		req.ParentTraceID = root.ID
	}

	// Kernel must be bootstrapped before any call can be committed.
	if err := k.requireReceiptSigningReady(); err != nil {
		return nil, err
	}

	// 10–11. Atomically lock funds and create child trace.
	now := time.Now().UTC()
	trace := &Trace{
		ID:            uuid.New().String(),
		ProcessID:     req.ProcessID,
		ParentTraceID: strPtr(req.ParentTraceID),
		ActionOwnerID: action.OwnerUserID,
		CreatedAt:     now,
	}
	if err := k.store.BeginCall(ctx, req.ProcessID, trace, action.Price); err != nil {
		if errors.Is(err, ErrInsufficientFunds) || errors.Is(err, ErrInvalidState) {
			return nil, err
		}
		return nil, ErrInternal.Wrap("could not begin call")
	}

	ctx = log.WithProcessID(ctx, req.ProcessID)
	ctx = log.WithCallerUserID(ctx, req.CallerID)
	ctx = log.WithTraceID(ctx, trace.ID)
	ctx = log.WithActionID(ctx, action.ID)
	logger = k.log.With(ctx)
	logger.Info("call.start", "action", action.Name, "price", action.Price)

	// Prepare transaction skeleton (fee computed post-execution once subCost is known).
	txID := uuid.New().String()
	tx := &Transaction{
		ID:            txID,
		ProcessID:     req.ProcessID,
		TraceID:       trace.ID,
		ParentTraceID: req.ParentTraceID,
		OwnerUserID:   process.OwnerUserID,
		CallerUserID: req.CallerID,
		TargetUserID:  target.ID,
		ActionID:      action.ID,
		ActionName:    action.Name,
		Status:        TxFailure,
		Gross:         0, // will be set on success
		Net:           0,
		Fee:           0,
		StartedAt:     now,
	}

	argsJSON, _ := json.Marshal(req.Args)
	tx.ArgsJSON = json.RawMessage(argsJSON)

	// 12. Execute. Dispatch is based on action.Kind; KindRemoteProxy is handled inside execute().
	started := time.Now()
	var (
		reply             map[string]any
		subCost           int64
		remoteReceiptHash string
		execErr           error
	)
	reply, subCost, remoteReceiptHash, execErr = k.execute(ctx, action, req.Args, trace, action.OwnerUserID, req.CallerID, process.OwnerUserID)
	if remoteReceiptHash != "" {
		tx.RemoteReceiptHash = sha256Hex(remoteReceiptHash)
		tx.RemoteReceiptJSON = remoteReceiptHash
	}
	latency := time.Since(started).Seconds()
	tx.EndedAt = time.Now().UTC()

	// Handle execution failure: refund and record atomically.
	if execErr != nil {
		tx.Status = TxFailure
		tx.Reason = execErr.Error()
		if err := k.settleFailedCall(ctx, logger, tx, req, action, latency, execErr); err != nil {
			return nil, err
		}
		logger.Warn("call.failed", "action", action.Name, "error", execErr)
		return nil, execErr
	}

	// 13. Validate output schema.
	if schemaErr := ValidateInput(action.OutputSchema, anyOf(reply)); schemaErr != nil {
		tx.Status = TxFailure
		tx.Reason = "output schema violation: " + schemaErr.Error()
		if err := k.settleFailedCall(ctx, logger, tx, req, action, latency, schemaErr); err != nil {
			return nil, err
		}
		return nil, schemaErr
	}

	// 14 & 15. Record transaction, settle payment, update trace cost/latency, and upsert stats — all atomic.
	// VAT fee: each kernel taxes only the value it adds (gross minus direct sub-call cost).
	taxable := action.Price - subCost
	if taxable < 0 {
		taxable = 0
	}
	net, fee := ComputeFee(taxable, action.Price, k.cfg.FeeBPS)
	replyJSON, _ := json.Marshal(reply)
	tx.ReplyJSON = json.RawMessage(replyJSON)
	tx.Status = TxSuccess
	tx.Gross = action.Price
	tx.Net = net
	tx.Fee = fee
	stats := k.computeStats(ctx, action.ID, tx, latency)
	receipt, receiptErr := k.buildReceipt(tx)
	if receiptErr != nil {
		if refundErr := k.store.RefundFunds(ctx, req.ProcessID, action.Price); refundErr != nil {
			logger.Error("call.refund_failed_on_receipt_error", "action", action.Name, "refund_error", refundErr)
		}
		return nil, ErrInternal.Wrap("could not build receipt")
	}
	if err := k.store.CommitCall(ctx, tx, receipt, req.ProcessID, target.ID, k.cfg.FeeRecipientID, net, fee, stats, req.EventID, req.IdempotencyRecordID); err != nil {
		if refundErr := k.store.RefundFunds(ctx, req.ProcessID, action.Price); refundErr != nil {
			logger.Error("call.refund_failed", "action", action.Name, "commit_error", err, "refund_error", refundErr)
			return nil, ErrInternal.Wrap("could not refund funds after failed commit")
		}
		return nil, ErrInternal.Wrap("could not commit transaction")
	}

	logger.Info("call.success", "action", action.Name, "tx_id", txID, "latency_ms", latency*1000)

	return &CallReply{
		Result:    reply,
		TxID:      txID,
		TraceID:   trace.ID,
		ReceiptID: receipt.ID,
		Gross:     action.Price,
	}, nil
}

// canCall returns true iff the action is callable by a process owned by ownerID.
// CanCall(ownerID, a) := active(a) ∧ (public(a) ∨ ownerID = a.OwnerUserID)
func canCall(ownerID string, action *Action) bool {
	return action.Active && (action.Public || ownerID == action.OwnerUserID)
}

// execute dispatches to the correct execution backend.
// Returns (result, subCost, remoteReceiptHash, error). remoteReceiptHash is non-empty only
// for successful KindRemoteProxy calls and holds the raw receipt JSON from the remote kernel.
// targetID is the action's owner; callerID is the call caller; ownerUserID is the process owner.
func (k *Kernel) execute(ctx context.Context, action *Action, args map[string]any, trace *Trace, targetID, callerID, ownerUserID string) (map[string]any, int64, string, error) {
	switch action.Kind {
	case KindHTTP:
		if k.http == nil {
			return nil, 0, "", ErrInvalidState.Wrap("HTTP executor not configured")
		}
		res, err := k.http.Execute(ctx, action.Source, args)
		return res, 0, "", err
	case KindWasm:
		res, cost, err := k.executeWasm(ctx, action, args, trace, targetID)
		return res, cost, "", err
	case KindNative:
		res, err := k.executeNative(ctx, action, args, targetID, callerID, ownerUserID, trace.ProcessID, trace.ID)
		return res, 0, "", err
	case KindRemoteProxy:
		if fe, ok := k.http.(FederationExecutor); ok {
			idempotencyKey := uuid.New().String()
			res, receiptJSON, err := fe.ExecuteFederation(ctx, action.Source, idempotencyKey, args)
			return res, 0, receiptJSON, err
		}
		return nil, 0, "", ErrInvalidState.Wrap("federation executor not configured")
	default:
		return nil, 0, "", ErrInvalidState.Wrapf("unknown action kind %q", action.Kind)
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
// Returns (result, subCost, error) where subCost is the gross paid to direct sub-calls during execution.
func (k *Kernel) executeWasm(ctx context.Context, action *Action, args map[string]any, trace *Trace, targetID string) (result map[string]any, subCost int64, execErr error) {
	defer func() {
		if r := recover(); r != nil {
			execErr = ErrExecutionFailed.Wrapf("wasm panic: %v", r)
		}
	}()
	if k.scripts == nil {
		return nil, 0, ErrInvalidState.Wrap("script executor not configured")
	}

	inputJSON, err := json.Marshal(args)
	if err != nil {
		return nil, 0, ErrInvalidInput.Wrap("could not serialize args")
	}

	artifact, _, err := k.scripts.Compile(ctx, []byte(action.Source))
	if err != nil {
		return nil, 0, ErrExecutionFailed.Wrapf("wasm compile failed: %v", err)
	}

	host := &kernelHostFunctions{
		kernel:    k,
		processID: trace.ProcessID,
		traceID:   trace.ID,
		targetID:  targetID,
	}

	outputJSON, err := k.scripts.Execute(ctx, artifact, inputJSON, host)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, 0, ErrTimeout.Wrapf("wasm execution timed out: %v", err)
		}
		return nil, 0, ErrExecutionFailed.Wrapf("wasm execution failed: %v", err)
	}

	if err := json.Unmarshal(outputJSON, &result); err != nil {
		return nil, 0, ErrExecutionFailed.Wrap("wasm output is not valid JSON")
	}
	return result, host.subCost, nil
}

// kernelHostFunctions implements HostFunctions using the kernel itself.
// Scripts never receive the caller's JWT — they inherit process+trace authority.
type kernelHostFunctions struct {
	kernel    *Kernel
	processID string
	traceID   string
	targetID  string // action owner; used as CallerID for subcalls
	subCost   int64  // gross paid to direct sub-calls; used for VAT fee computation
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
		ProcessID:     h.processID,
		ParentTraceID: h.traceID,
		TargetUserID:  parts[0],
		ActionName:    parts[1],
		Args:          args,
	})
	if err != nil {
		return nil, err
	}
	h.subCost += reply.Gross
	return json.Marshal(reply.Result)
}

func (h *kernelHostFunctions) Emit(ctx context.Context, event string, argsJSON []byte) error {
	var args map[string]any
	if len(argsJSON) > 0 {
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return ErrInvalidInput.Wrap("emit args must be a JSON object")
		}
	}
	// Pass current trace ID as causal context (FOLLOWS_FROM) for listener-triggered traces.
	_, err := h.kernel.EmitEvent(ctx, h.targetID, h.targetID, event, args, h.traceID)
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

// computeStats builds a delta Stats for this call outcome. The SQL in CommitCall/CommitFailedCall
// applies these as incremental updates, making concurrent calls safe.
func (k *Kernel) computeStats(_ context.Context, actionID string, tx *Transaction, latency float64) *Stats {
	stats := DefaultStats(actionID)
	UpdateStats(stats, tx, latency)
	return stats
}

// settleFailedCall builds a receipt, commits the failed transaction atomically, and upserts
// stat tags. tx.Status and tx.Reason must be set by the caller before invoking this.
func (k *Kernel) settleFailedCall(ctx context.Context, logger *log.Logger, tx *Transaction, req CallRequest, action *Action, latency float64, callErr error) error {
	if len(tx.ReplyJSON) == 0 {
		tx.ReplyJSON = json.RawMessage("null")
	}
	stats := k.computeStats(ctx, action.ID, tx, latency)
	receipt, receiptErr := k.buildReceipt(tx)
	if receiptErr != nil {
		if refundErr := k.store.RefundFunds(ctx, req.ProcessID, action.Price); refundErr != nil {
			logger.Error("call.refund_failed_on_receipt_error", "action", action.Name, "refund_error", refundErr)
		}
		return ErrInternal.Wrap("could not build receipt")
	}
	if settlErr := k.store.CommitFailedCall(ctx, tx, receipt, req.ProcessID, action.Price, stats, req.IdempotencyRecordID, KernelErrorCode(callErr)); settlErr != nil {
		logger.Error("call.settlement_failed", "action", action.Name, "error", callErr, "settlement_error", settlErr)
		return ErrInternal.Wrap("could not record failure transaction")
	}
	return nil
}

// anyOf converts a map[string]any to any for schema validation.
func anyOf(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// isInsufficientFunds reports whether err wraps ErrInsufficientFunds.
func isInsufficientFunds(err error) bool {
	return errors.Is(err, ErrInsufficientFunds)
}

// strPtr returns nil for an empty string, otherwise a pointer to s.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ComputeFee computes (net, fee) for a gross amount using VAT-style basis points.
// Only the taxable portion (gross minus direct sub-call cost) is subject to the fee.
// fee is rounded up (ceiling division). Invariant: net + fee == gross.
func ComputeFee(taxable, gross, feeBPS int64) (net, fee int64) {
	if gross == 0 || feeBPS == 0 || taxable == 0 {
		return gross, 0
	}
	fee = (taxable*feeBPS + 9999) / 10000
	net = gross - fee
	return
}
