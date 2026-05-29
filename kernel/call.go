package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	subject, err := k.store.ReadUser(ctx, req.SubjectID)
	if err != nil || subject == nil {
		return nil, ErrUnauthorized.Wrap("subject user not found")
	}
	if subject.SuspendedAt != nil {
		return nil, ErrUnauthenticated.Wrap("account suspended")
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
	if err := k.requireReceiptSigningReady(); err != nil {
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

	// 10. Lock funds atomically — first state change.
	if action.Price > 0 {
		if err := k.store.LockFunds(ctx, req.ProcessID, action.Price); err != nil {
			return nil, ErrInsufficientFunds.Wrap("could not lock funds")
		}
	}

	// 11. Create child trace.
	now := time.Now().UTC()
	trace := &Trace{
		ID:              uuid.New().String(),
		ProcessID:       req.ProcessID,
		ParentTraceID:   req.ParentTraceID,
		CausedByTraceID: strPtr(req.CausedByTraceID),
		CreatedAt:       now,
	}
	if err := k.store.CreateTrace(ctx, trace); err != nil {
		if refundErr := k.store.RefundFunds(ctx, req.ProcessID, action.Price); refundErr != nil {
			logger.Error("call.refund_failed_on_trace_error", "refund_error", refundErr)
			return nil, ErrInternal.Wrap("could not refund funds after trace creation failure")
		}
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

	// 12. Execute. If the target is a remote kernel and the HTTP executor supports federation,
	// use ExecuteFederation to carry an idempotency key and capture the remote receipt hash.
	started := time.Now()
	var reply map[string]any
	var execErr error
	if target.RemoteBaseURL != "" {
		if fe, ok := k.http.(federationExecutor); ok {
			idempotencyKey := uuid.New().String()
			var receiptJSON string
			reply, receiptJSON, execErr = fe.ExecuteFederation(ctx, action.Source, idempotencyKey, req.Args)
			if execErr == nil && receiptJSON != "" {
				tx.RemoteReceiptHash = sha256Hex(receiptJSON)
			}
		} else {
			reply, execErr = k.execute(ctx, action, req.Args, trace, action.OwnerUserID)
		}
	} else {
		// Pass action.OwnerUserID so host functions operate on behalf of the action author.
		reply, execErr = k.execute(ctx, action, req.Args, trace, action.OwnerUserID)
	}
	latency := time.Since(started).Seconds()
	tx.EndedAt = time.Now().UTC()

	// Handle execution failure: refund and record atomically.
	if execErr != nil {
		tx.Status = TxFailure
		tx.Reason = execErr.Error()
		stats := k.computeStats(ctx, action.ID, tx, latency)
		receipt, receiptErr := k.buildReceipt(tx)
		if receiptErr != nil {
			if refundErr := k.store.RefundFunds(ctx, req.ProcessID, action.Price); refundErr != nil {
				logger.Error("call.refund_failed_on_receipt_error", "action", action.Name, "refund_error", refundErr)
			}
			return nil, ErrInternal.Wrap("could not build receipt")
		}
		if settlErr := k.store.CommitFailedCall(ctx, tx, receipt, req.ProcessID, action.Price, stats); settlErr != nil {
			logger.Error("call.settlement_failed", "action", action.Name, "exec_error", execErr, "settlement_error", settlErr)
			return nil, ErrInternal.Wrap("could not record failure transaction")
		}
		k.upsertStatTag(ctx, action.ID, stats)
		logger.Warn("call.failed", "action", action.Name, "error", execErr)
		return nil, execErr
	}

	// 13. Validate output schema.
	if schemaErr := ValidateInput(action.OutputSchema, anyOf(reply)); schemaErr != nil {
		tx.Status = TxFailure
		tx.Reason = "output schema violation: " + schemaErr.Error()
		stats := k.computeStats(ctx, action.ID, tx, latency)
		receipt, receiptErr := k.buildReceipt(tx)
		if receiptErr != nil {
			if refundErr := k.store.RefundFunds(ctx, req.ProcessID, action.Price); refundErr != nil {
				logger.Error("call.refund_failed_on_receipt_error", "action", action.Name, "refund_error", refundErr)
			}
			return nil, ErrInternal.Wrap("could not build receipt")
		}
		if settlErr := k.store.CommitFailedCall(ctx, tx, receipt, req.ProcessID, action.Price, stats); settlErr != nil {
			logger.Error("call.settlement_failed", "action", action.Name, "schema_error", schemaErr, "settlement_error", settlErr)
			return nil, ErrInternal.Wrap("could not record failure transaction")
		}
		k.upsertStatTag(ctx, action.ID, stats)
		return nil, schemaErr
	}

	// 14 & 15. Record transaction, settle payment, update trace cost/latency, and upsert stats — all atomic.
	replyJSON, _ := json.Marshal(reply)
	tx.ReplyJSON = string(replyJSON)
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
	if err := k.store.CommitCall(ctx, tx, receipt, req.ProcessID, target.ID, k.cfg.FeeRecipientID, net, fee, stats); err != nil {
		if refundErr := k.store.RefundFunds(ctx, req.ProcessID, action.Price); refundErr != nil {
			logger.Error("call.refund_failed", "action", action.Name, "commit_error", err, "refund_error", refundErr)
			return nil, ErrInternal.Wrap("could not refund funds after failed commit")
		}
		return nil, ErrInternal.Wrap("could not commit transaction")
	}
	k.upsertStatTag(ctx, action.ID, stats)

	logger.Info("call.success", "action", action.Name, "tx_id", txID, "latency_ms", fmt.Sprintf("%.1f", latency*1000))

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
func (k *Kernel) execute(ctx context.Context, action *Action, args map[string]any, trace *Trace, ownerUserID string) (map[string]any, error) {
	switch action.Kind {
	case KindHTTP:
		if k.http == nil {
			return nil, ErrInvalidState.Wrap("HTTP executor not configured")
		}
		return k.http.Execute(ctx, action.Source, args)
	case KindWasm:
		return k.executeWasm(ctx, action, args, trace, ownerUserID)
	case KindNative:
		return k.executeNative(ctx, action, args)
	default:
		return nil, ErrInvalidState.Wrapf("unknown action kind %q", action.Kind)
	}
}

// executeNative dispatches to built-in native action implementations.
func (k *Kernel) executeNative(ctx context.Context, action *Action, args map[string]any) (map[string]any, error) {
	switch action.Name {
	case "/lookup":
		return k.executeLookup(ctx, args)
	case "/llm/chat":
		return k.executeChat(ctx, args)
	default:
		return nil, ErrInvalidState.Wrapf("unknown native action %q", action.Name)
	}
}

// executeLookup implements the /lookup native action.
func (k *Kernel) executeLookup(ctx context.Context, args map[string]any) (map[string]any, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, ErrInvalidInput.Wrap("lookup requires query argument")
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}
	results, err := k.Lookup(ctx, LookupRequest{Query: query, Limit: limit})
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
			"score":        r.Score,
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
func (k *Kernel) executeWasm(ctx context.Context, action *Action, args map[string]any, trace *Trace, ownerUserID string) (map[string]any, error) {
	if k.scripts == nil {
		return nil, ErrInvalidState.Wrap("script executor not configured")
	}

	inputJSON, err := json.Marshal(args)
	if err != nil {
		return nil, ErrInvalidInput.Wrap("could not serialize args")
	}

	artifact, _, err := k.scripts.Compile(ctx, []byte(action.Source))
	if err != nil {
		return nil, ErrExecutionFailed.Wrapf("wasm compile failed: %v", err)
	}

	host := &kernelHostFunctions{
		kernel:      k,
		processID:   trace.ProcessID,
		traceID:     trace.ID,
		ownerUserID: ownerUserID,
	}

	outputJSON, err := k.scripts.Execute(ctx, artifact, inputJSON, host)
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
	subActionName := "/" + parts[1]

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
	_, err := h.kernel.EmitEvent(ctx, h.ownerUserID, event, args, h.traceID)
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

// computeStats reads current stats, applies the call outcome, and returns the updated Stats.
// The returned value is passed to CommitCall/CommitFailedCall so the upsert is atomic with settlement.
func (k *Kernel) computeStats(ctx context.Context, actionID string, tx *Transaction, latency float64) *Stats {
	stats, err := k.store.ReadStats(ctx, actionID)
	if err != nil || stats == nil {
		stats = DefaultStats(actionID)
	}
	UpdateStats(stats, tx, latency)
	return stats
}

// upsertStatTag writes the latency bucket tag. Best-effort: errors are logged, not fatal.
func (k *Kernel) upsertStatTag(ctx context.Context, actionID string, stats *Stats) {
	_ = k.store.UpsertStatTag(ctx, &StatTag{
		ActionID:  actionID,
		Key:       "latency_bucket",
		Value:     latencyBucket(stats.LatencyMean),
		Source:    "kernel",
		UpdatedAt: time.Now().UTC(),
	})
}

// latencyBucket categorises observed mean latency for lookup filtering.
func latencyBucket(meanSeconds float64) string {
	switch {
	case meanSeconds < 0.1:
		return "fast"
	case meanSeconds < 1.0:
		return "medium"
	default:
		return "slow"
	}
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
