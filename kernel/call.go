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
	// SubjectID is the authenticated user making the call.
	SubjectID string
	// ProcessID is the budgeted execution context.
	ProcessID string
	// ParentTraceID is the trace from which this call originates.
	// For top-level calls it is the process root trace ID.
	ParentTraceID string
	// CausedByTraceID is a FOLLOWS_FROM reference set for event-triggered calls.
	// It references the emitting action's trace (may cross process boundaries).
	// Leave empty for direct calls.
	CausedByTraceID string
	// TargetUserID is the owner of the action.
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
	Result  map[string]any `json:"result"`
	TxID    string         `json:"tx_id"`
	TraceID string         `json:"trace_id"`
}

// Call executes the central kernel transition.
// Preconditions are checked in order per Section 5.1 of the requirements.
func (k *Kernel) Call(ctx context.Context, req CallRequest) (*CallReply, error) {
	logger := k.log.With(ctx)

	// 1. Subject must be authenticated: non-empty, exists, and not suspended.
	if req.SubjectID == "" {
		return nil, ErrUnauthenticated.Wrap("subject is required")
	}
	if _, err := k.authenticatedSubject(ctx, req.SubjectID); err != nil {
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

	// 3. Subject is the process owner or has explicit process authority.
	if process.OwnerUserID != req.SubjectID {
		authorized, authErr := k.store.CheckProcessAuthority(ctx, req.SubjectID, req.ProcessID)
		if authErr != nil {
			return nil, ErrInternal.Wrapf("process authority check failed: %v", authErr)
		}
		if !authorized {
			return nil, ErrUnauthorized.Wrap("subject is not the process owner")
		}
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

	// 5. Action must be active.
	if !action.Active {
		return nil, ErrInvalidState.Wrap("action is inactive")
	}

	// 6. ACL check — CanCall(subject, action).
	if action.OwnerUserID != req.SubjectID {
		canCall, err := k.canCall(ctx, req.SubjectID, action)
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

	// 9. Validate or resolve parent trace — precondition check, no state change yet.
	if req.ParentTraceID != "" {
		parent, err := k.store.ReadTrace(ctx, req.ParentTraceID)
		if err != nil {
			return nil, ErrInvalidInput.Wrap("parent trace not found")
		}
		if parent.ProcessID != req.ProcessID {
			return nil, ErrInvalidInput.Wrap("parent trace belongs to a different process")
		}
	} else {
		root, err := k.store.ReadRootTrace(ctx, req.ProcessID)
		if err != nil {
			return nil, ErrInternal.Wrap("could not resolve root trace for process")
		}
		req.ParentTraceID = root.ID
	}

	// Causal trace invariants.
	// Direct calls must not carry a FOLLOWS_FROM reference.
	if req.CausedByTraceID != "" && req.EventID == "" {
		return nil, ErrInvalidInput.Wrap("CausedByTraceID must be empty for direct calls")
	}
	// FOLLOWS_FROM and CHILD_OF must reference distinct traces.
	if req.CausedByTraceID != "" && req.CausedByTraceID == req.ParentTraceID {
		return nil, ErrInvalidInput.Wrap("CausedByTraceID must differ from ParentTraceID")
	}

	// Kernel must be bootstrapped before any call can be committed.
	if err := k.requireReceiptSigningReady(); err != nil {
		return nil, err
	}

	// 10–11. Atomically lock funds and create child trace.
	now := time.Now().UTC()
	trace := &Trace{
		ID:              uuid.New().String(),
		ProcessID:       req.ProcessID,
		ParentTraceID:   req.ParentTraceID,
		CausedByTraceID: strPtr(req.CausedByTraceID),
		CreatedAt:       now,
	}
	if err := k.store.BeginCall(ctx, req.ProcessID, trace, action.Price); err != nil {
		if errors.Is(err, ErrInsufficientFunds) || errors.Is(err, ErrInvalidState) {
			return nil, err
		}
		return nil, ErrInternal.Wrap("could not begin call")
	}

	ctx = log.WithProcessID(ctx, req.ProcessID)
	ctx = log.WithSubjectUserID(ctx, req.SubjectID)
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
	tx.ArgsJSON = json.RawMessage(argsJSON)

	// 12. Execute. If the target is a remote kernel and the HTTP executor supports federation,
	// use ExecuteFederation to carry an idempotency key and capture the remote receipt hash.
	started := time.Now()
	var reply map[string]any
	var subCost int64
	var execErr error
	if action.Kind == KindRemoteProxy {
		if fe, ok := k.http.(federationExecutor); ok {
			idempotencyKey := uuid.New().String()
			var receiptJSON string
			reply, receiptJSON, execErr = fe.ExecuteFederation(ctx, action.Source, idempotencyKey, req.Args)
			if execErr == nil && receiptJSON != "" {
				tx.RemoteReceiptHash = sha256Hex(receiptJSON)
			}
		} else {
			reply, subCost, execErr = k.execute(ctx, action, req.Args, trace, action.OwnerUserID, req.SubjectID)
		}
	} else {
		// Pass action.OwnerUserID so host functions operate on behalf of the action author.
		reply, subCost, execErr = k.execute(ctx, action, req.Args, trace, action.OwnerUserID, req.SubjectID)
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
	if fee > 0 && k.cfg.FeeRecipientID == "" {
		if refundErr := k.store.RefundFunds(ctx, req.ProcessID, action.Price); refundErr != nil {
			logger.Error("call.refund_failed_no_fee_recipient", "action", action.Name, "refund_error", refundErr)
		}
		return nil, ErrInvalidState.Wrap("fee recipient not configured")
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
		Result:  reply,
		TxID:    txID,
		TraceID: trace.ID,
	}, nil
}

// canCall checks public flag, ACL(subject, action, call), or ACL(subject, action, admin).
func (k *Kernel) canCall(ctx context.Context, subjectID string, action *Action) (bool, error) {
	if action.Public {
		return true, nil
	}
	ok, err := k.store.CheckACL(ctx, subjectID, action.ID, PermCall)
	if err != nil {
		return false, ErrInternal.Wrapf("acl check: %v", err)
	}
	if ok {
		return true, nil
	}
	ok, err = k.store.CheckACL(ctx, subjectID, action.ID, PermAdmin)
	if err != nil {
		return false, ErrInternal.Wrapf("acl check: %v", err)
	}
	return ok, nil
}

// execute dispatches to the correct execution backend.
// Returns (result, subCost, error) where subCost is the total gross paid to direct WASM sub-calls.
func (k *Kernel) execute(ctx context.Context, action *Action, args map[string]any, trace *Trace, ownerUserID, subjectID string) (map[string]any, int64, error) {
	switch action.Kind {
	case KindHTTP:
		if k.http == nil {
			return nil, 0, ErrInvalidState.Wrap("HTTP executor not configured")
		}
		result, err := k.http.Execute(ctx, action.Source, args)
		return result, 0, err
	case KindWasm:
		return k.executeWasm(ctx, action, args, trace, ownerUserID)
	case KindNative:
		result, err := k.executeNative(ctx, action, args, subjectID)
		return result, 0, err
	case KindRemoteProxy:
		return nil, 0, ErrInvalidState.Wrap("federation executor not configured")
	default:
		return nil, 0, ErrInvalidState.Wrapf("unknown action kind %q", action.Kind)
	}
}

// executeNative dispatches to built-in native action implementations.
func (k *Kernel) executeNative(ctx context.Context, action *Action, args map[string]any, subjectID string) (map[string]any, error) {
	switch action.Name {
	case "lookup":
		return k.executeLookup(ctx, args, subjectID)
	case "llm/chat":
		return k.executeChat(ctx, args)
	default:
		return nil, ErrInvalidState.Wrapf("unknown native action %q", action.Name)
	}
}

// executeLookup implements the /lookup native action.
func (k *Kernel) executeLookup(ctx context.Context, args map[string]any, subjectID string) (map[string]any, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, ErrInvalidInput.Wrap("lookup requires query argument")
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}
	results, err := k.Lookup(ctx, LookupRequest{Query: query, Limit: limit, SubjectID: subjectID})
	if err != nil {
		return nil, err
	}
	items := make([]any, len(results))
	for i, r := range results {
		items[i] = map[string]any{
			"action_id":    r.Action.ID,
			"name":         r.Action.Name,
			"owner_handle": r.OwnerHandle,
			"description":  r.Action.Description,
			"score":        float64(r.Score),
		}
	}
	return map[string]any{"results": items}, nil
}

// executeChat implements the /llm/chat native action.
func (k *Kernel) executeChat(ctx context.Context, args map[string]any) (map[string]any, error) {
	if k.chatter == nil {
		return nil, ErrInvalidState.Wrap("chat service not configured")
	}

	rawMsgs, ok := args["messages"]
	if !ok {
		return nil, ErrInvalidInput.Wrap("chat requires messages argument")
	}
	msgList, ok := rawMsgs.([]any)
	if !ok {
		return nil, ErrInvalidInput.Wrap("messages must be an array")
	}

	var messages []ChatMessage
	if sys, ok := args["system"].(string); ok && sys != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: sys})
	}
	for _, item := range msgList {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, ErrInvalidInput.Wrap("each message must be an object")
		}
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "" || content == "" {
			return nil, ErrInvalidInput.Wrap("each message must have role and content")
		}
		messages = append(messages, ChatMessage{Role: role, Content: content})
	}

	reply, err := k.chatter.Chat(ctx, messages)
	if err != nil {
		return nil, ErrExecutionFailed.Wrapf("chat failed: %v", err)
	}
	return map[string]any{
		"message": map[string]any{
			"role":    reply.Role,
			"content": reply.Content,
		},
	}, nil
}

// executeWasm runs a compiled WASM artifact.
// Returns (result, subCost, error) where subCost is the gross paid to direct sub-calls during execution.
func (k *Kernel) executeWasm(ctx context.Context, action *Action, args map[string]any, trace *Trace, ownerUserID string) (result map[string]any, subCost int64, execErr error) {
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
		kernel:      k,
		processID:   trace.ProcessID,
		traceID:     trace.ID,
		ownerUserID: ownerUserID,
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
// Scripts never receive the subject's JWT — they inherit process+trace authority.
type kernelHostFunctions struct {
	kernel      *Kernel
	processID   string
	traceID     string
	ownerUserID string
	subCost     int64 // gross paid to direct sub-calls; used for VAT fee computation
}

func (h *kernelHostFunctions) Call(ctx context.Context, actionName string, argsJSON []byte) ([]byte, error) {
	// Parse "handle/action-name" form.
	parts := strings.SplitN(actionName, "/", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidInput.Wrap("actionName must be handle/name")
	}
	subActionName := parts[1]

	var args map[string]any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return nil, ErrInvalidInput.Wrap("args must be a JSON object")
	}

	// Resolve target user and action to get the price upfront.
	target, err := h.kernel.store.ReadUserByHandle(ctx, parts[0])
	if err != nil {
		target, err = h.kernel.store.ReadUser(ctx, parts[0])
		if err != nil {
			return nil, ErrNotFound.Wrap("target user not found")
		}
	}
	action, err := h.kernel.store.ReadActionByOwnerName(ctx, target.ID, subActionName)
	if err != nil || action == nil {
		return nil, ErrNotFound.Wrapf("action %s not found", actionName)
	}

	// Contractor model: create an ephemeral process owned by the calling action's
	// owner. The caller's process is not charged for sub-calls.
	now := time.Now().UTC()
	ep := &Process{
		ID:          uuid.New().String(),
		OwnerUserID: h.ownerUserID,
		Status:      ProcessOpen,
		CreatedAt:   now,
	}
	epRoot := &Trace{
		ID:        uuid.New().String(),
		ProcessID: ep.ID,
		CreatedAt: now,
	}
	epRoot.ParentTraceID = epRoot.ID
	epRoot.CausedByTraceID = strPtr(h.traceID) // FOLLOWS_FROM: ephemeral process root explains why this process was created
	if err := h.kernel.store.StartProcess(ctx, ep, epRoot, h.ownerUserID, action.Price); err != nil {
		if isInsufficientFunds(err) {
			return nil, ErrInsufficientFunds.Wrap("action owner has insufficient balance for sub-call")
		}
		return nil, ErrInternal.Wrap("could not create ephemeral process")
	}
	ep.Available = action.Price
	// Always close the ephemeral process on return; any unused funds go back to owner.
	// Use context.Background() so a cancelled request context does not prevent cleanup.
	defer func() { _ = h.kernel.store.EndProcess(context.Background(), ep.ID) }()

	reply, err := h.kernel.Call(ctx, CallRequest{
		SubjectID:    h.ownerUserID,
		ProcessID:    ep.ID,
		TargetUserID: target.ID,
		ActionName:   subActionName,
		Args:         args,
	})
	if err != nil {
		return nil, err
	}
	h.subCost += action.Price // only count gross for successful sub-calls (VAT model)
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
	_, err := h.kernel.EmitEvent(ctx, h.ownerUserID, h.ownerUserID, event, args, h.traceID)
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
	if settlErr := k.store.CommitFailedCall(ctx, tx, receipt, req.ProcessID, action.Price, stats, req.IdempotencyRecordID, kernelErrorCode(callErr)); settlErr != nil {
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
