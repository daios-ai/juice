// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

// readOpenProcess reads a process and rejects a closed one — §4 precondition 2, the gate every
// spend path shares (a closed process can neither call nor park work).
func (k *Kernel) readOpenProcess(ctx context.Context, processID string) (*Process, error) {
	process, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}
	return process, nil
}

// Recover settles interrupted calls and re-parks crashed task completions.
// Must be called after SetSigningKey (buildReceipt requires the platform signing key).
func (k *Kernel) Recover(ctx context.Context) error {
	logger := k.log.With(ctx)

	// 0: Retry pending remote dispatches first. If the remote kernel is reachable and
	// the call committed, we settle cleanly here rather than force-failing via recoverTrace.
	// Any still-pending remote traces are force-failed by settleFailedCall when their
	// orphan parent traces are settled in phase C below.
	k.RetryPendingRemoteDispatches(ctx)
	k.SettleReady(ctx)

	// A: Re-park empty task-completion traces; collect non-empty ones for phase C.
	// Empty means no subcall started (trace.locked==0, trace.available==task.price, no settled subtx).
	orphanTasks, err := k.store.ListOrphanRunningTasks(ctx, "")
	if err != nil {
		return err
	}
	taskByTrace := map[string]string{} // completionTraceID → taskID for non-empty completions
	for _, row := range orphanTasks {
		isEmpty := !row.HasSettled && row.TraceLocked == 0 && row.TraceAvailable == row.Price
		if isEmpty {
			// A failed re-park aborts recovery — and so the boot. Continuing would let the final
			// sweep mark this task waiting with its price still in an unreferenced completion trace,
			// a task nothing can complete again (G4: restart loses no funds, and never pretends).
			if err := k.store.ResetTaskAndRepark(ctx, row.TaskID); err != nil {
				return fmt.Errorf("recovery: re-park task %s: %w", row.TaskID, err)
			}
		} else {
			taskByTrace[row.CompletionTraceID] = row.TaskID
		}
	}

	// C: Settle orphan traces deepest-first. Non-empty task-completion traces are included;
	// taskByTrace routes them to CallerTask semantics so the refund goes to process.available.
	// Children settle before parents, so a child's refund reaches the parent before the parent settles.
	traces, err := k.store.ListOrphanTraces(ctx)
	if err != nil {
		return err
	}
	for _, trace := range traces {
		// Deepest first, so a child's settlement has already climbed to its parent by the time the
		// loop reaches it (settleTrace then finds it settled). An orphan whose children are still
		// in flight — dispatched remote calls the retry loop owns — records its outcome and is
		// settled by the last of them (D3); at startup nothing executes, so it is truly interrupted.
		if err := k.recoverTrace(ctx, logger, trace, "interrupted", taskByTrace[trace.ID]); err != nil {
			return fmt.Errorf("recovery: settle trace %s: %w", trace.ID, err)
		}
	}

	// B: Reset remaining running tasks. After C, non-empty completion tasks have tx_id set,
	// so only truly pre-BeginTaskCall-crash tasks (status=running, tx_id=null) remain here.
	if err := k.store.ResetRunningTasks(ctx); err != nil {
		return err
	}
	// Tasks made before the mailbox existed are delivered once, by the one writer (D4).
	return k.store.RedeliverTasks(ctx)
}

// newTraceFailureTx builds the failure Transaction skeleton for a trace settled after the fact.
func newTraceFailureTx(trace *Trace, process *Process, action *Action, gross int64, now time.Time) *Transaction {
	tx := &Transaction{
		ID: uuid.New().String(), ProcessID: trace.ProcessID, TraceID: trace.ID, OwnerUserID: process.OwnerUserID,
		ActionID: trace.ActionID, ActionName: action.Name, Status: TxFailure, Gross: gross,
		StartedAt: trace.CreatedAt, EndedAt: now,
	}
	tx.setParties(trace)
	return tx
}

// settleTrace commits an outcome onto an unsettled trace, after the fact: the deferred settlement
// D3 orders after the trace's children, and startup recovery's `interrupted`. It rebuilds the
// transaction from the trace and the outcome alone, so what is committed is exactly what was
// recorded, then goes through the same two settlement paths a live call takes.
func (k *Kernel) settleTrace(ctx context.Context, trace *Trace, o TraceOutcome) error {
	if settled, err := k.store.TraceHasTransaction(ctx, trace.ID); err != nil || settled {
		return err
	}
	logger := k.log.With(ctx)
	process, err := k.store.ReadProcess(ctx, trace.ProcessID)
	if err != nil {
		return err
	}
	action, err := k.store.ReadAction(ctx, trace.ActionID)
	if err != nil || action == nil {
		action = &Action{ID: trace.ActionID, Name: "unknown", OwnerUserID: trace.ActionOwnerID}
	}
	callerWalletID, callerWalletKind := callerWalletFor(o.TaskID, process.ID, trace.ParentTraceID)
	// The allocation: recorded with the outcome by a live call; for a trace recovered after a
	// crash, what is left on it plus what its settled children consumed, which left it for good.
	gross := o.Gross
	if gross == 0 {
		consumed, err := k.store.ConsumedByChildren(ctx, trace.ID)
		if err != nil {
			return err
		}
		gross = trace.Available + trace.Locked + consumed
	}
	ktx := &Transaction{
		ID: uuid.New().String(), ProcessID: trace.ProcessID, TraceID: trace.ID, OwnerUserID: process.OwnerUserID,
		ActionID: trace.ActionID, ActionName: action.Name, Status: o.Status, Reason: o.Reason,
		Gross: gross, StartedAt: trace.CreatedAt, EndedAt: o.EndedAt,
		ArgsJSON: o.Args, ReplyJSON: o.Reply,
	}
	ktx.setParties(trace)
	if trace.ParentTraceID != nil {
		ktx.ParentTraceID = *trace.ParentTraceID
	}
	if len(ktx.ReplyJSON) == 0 {
		ktx.ReplyJSON = json.RawMessage("null")
	}
	if len(ktx.ArgsJSON) == 0 && trace.IdempotencyRecordID != nil {
		if rec, err := k.store.ReadIdempotencyRecordByID(ctx, *trace.IdempotencyRecordID); err == nil && rec.ArgsJSON != "" {
			ktx.ArgsJSON = json.RawMessage(rec.ArgsJSON)
		}
	}
	req := callRequest{TaskID: o.TaskID}
	if trace.IdempotencyRecordID != nil {
		req.IdempotencyRecordID = *trace.IdempotencyRecordID
	}
	latency := o.EndedAt.Sub(trace.CreatedAt).Seconds()
	if o.Status != TxSuccess {
		// The recorded reason is the failure's class, and the class is what a peer's replay must
		// see (P4): the typed error is rebuilt from it, never reported as an internal one.
		_, _, err := k.settleFailedCall(ctx, logger, ktx, trace, callerWalletID, callerWalletKind, req, action, latency, ErrorFromCode(o.Reason).Wrap(o.Reason))
		return err
	}
	// Success, after the fact: the budget's remainder is what settled children left, so the
	// margin is final; the charge is the fixed price (P5). The commit verifies the remainder it
	// is handed is still the row's, and records the outcome again otherwise.
	fresh, err := k.store.ReadTrace(ctx, trace.ID)
	if err != nil {
		return err
	}
	net, fee := k.econ.Fee(fresh.Available)
	ktx.Net, ktx.Fee = net, fee
	stats := k.computeStats(ctx, action.ID, ktx, latency)
	sale, err := k.sold(ctx, trace)
	if err != nil {
		return err
	}
	k.markEvidenceEligible(ctx, action, ktx)
	receipt, err := k.buildReceipt(ktx, ktx.Gross, k.econ.Premium(ktx.Gross, sale.RemoteBPS), trace.Value, trace.ValueTo, sale)
	if err != nil {
		return err
	}
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	err = k.store.CommitCall(sctx, ktx, receipt, trace.ID, callerWalletID, callerWalletKind, trace.ActionOwnerID, k.cfg.FeeRecipientID, net, fee, stats, req.IdempotencyRecordID, req.TaskID)
	if errors.Is(err, ErrSettlementDeferred) {
		return nil // recorded again; the next pass settles it
	}
	if err != nil {
		return ErrInternal.Wrap("could not commit deferred transaction")
	}
	k.observeSettled(action, ktx, req.IdempotencyRecordID)
	return nil
}

// recoverTrace settles a trace that nothing else will: recovery at startup and forced closure. An
// outcome the call itself recorded is honoured; only a call that recorded none was interrupted.
func (k *Kernel) recoverTrace(ctx context.Context, _ *log.Logger, trace *Trace, reason, taskID string) error {
	if trace.OutcomeJSON != nil {
		var o TraceOutcome
		if json.Unmarshal([]byte(*trace.OutcomeJSON), &o) == nil {
			return k.settleTrace(ctx, trace, o)
		}
	}
	return k.settleTrace(ctx, trace, TraceOutcome{Status: TxFailure, Reason: reason, EndedAt: time.Now().UTC(), TaskID: taskID})
}

// CreateTask creates a new waiting task. The task records a future Call that a designated caller can resume.
// The creating authority is derived from Trace(traceID).action_owner_id (implicit for in-execution creation).
// For external creation (POST /v1/tasks) the service layer must enforce precondition-4 before calling this.
func (k *Kernel) CreateTask(ctx context.Context, traceID, actionID string, partialArgs json.RawMessage, caller Principal) (*Task, error) {
	if traceID == "" {
		return nil, ErrInvalidInput.Wrap("trace_id is required")
	}
	parent, err := k.store.ReadTrace(ctx, traceID)
	if err != nil {
		return nil, ErrNotFound.Wrap("parent trace not found")
	}
	if _, err := k.readOpenProcess(ctx, parent.ProcessID); err != nil {
		return nil, err
	}
	action, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	// Creation parks money, so it is a funding boundary: a legacy proxy heals here, before its
	// frozen total is parked and its completion dispatches a wrong seller price (§16).
	if action, err = k.ensureBasePrice(ctx, action); err != nil {
		return nil, err
	}
	// A task is a partially applied future Call: the creator names the target, so visibility binds
	// here against the creating trace's action owner (§4 binding rule) — completion re-checks only
	// liveness, never visibility, so the completer needs no sight of a target the creator captured.
	if !canCall(parent.Target(), action) {
		if !action.Active {
			return nil, ErrInvalidState.Wrap("action is inactive")
		}
		return nil, ErrUnauthorized.Wrap("creator cannot call action")
	}
	// The required caller must be someone who can complete the task (§10): a user here, or a peer
	// this kernel knows — the kernel itself, or a user of it by its stable id there, which the
	// peer's signed completion must name (P8), so a remote handle rename never mis-addresses it.
	if caller.Local() {
		if _, err := k.store.ReadUser(ctx, caller.UserID); err != nil {
			return nil, ErrNotFound.Wrap("required caller not found")
		}
	} else if rk, err := k.store.ReadKernel(ctx, caller.Kernel); err != nil || rk == nil {
		return nil, ErrNotFound.Wrap("required caller's kernel is not known here")
	}
	// Omitted and null both mean nothing is filled in yet; anything else must be an object.
	var partial map[string]any
	if len(partialArgs) > 0 && json.Unmarshal(partialArgs, &partial) != nil {
		return nil, ErrInvalidInput.Wrap("partial_args must be a JSON object")
	}
	if partial == nil {
		partialArgs = json.RawMessage("{}")
	}
	now := time.Now().UTC()
	task := &Task{
		ID:                   uuid.New().String(),
		ParentTraceID:        &traceID,
		RequiredCallerKernel: caller.Kernel,
		RequiredCallerUserID: caller.UserID,
		RequiredCallerHandle: caller.Handle,
		ActionID:             actionID,
		PartialArgs:          partialArgs,
		Price:                action.Price,
		Status:               TaskWaiting,
		CreatedAt:            now,
	}
	// A funding boundary (§16 Price Snapshot Pattern): the price is parked now and may settle long
	// after import_bps changes, so the rate is frozen alongside it. Proxies only.
	if action.Kind == KindRemoteProxy {
		ibps := k.econ.ImportBPS
		task.ImportBPS = &ibps
	}
	// The park is a funding statement (D2) and every settlement commit refuses over a task
	// beneath it (D3): the store carries the order, so no lock does.
	if err := k.store.CreateTask(ctx, task); err != nil {
		return nil, err
	}
	k.WakeTell()
	k.log.With(ctx).Info("task.created", "task_id", task.ID, "trace_id", traceID, "status", "success")
	return task, nil
}

// CreateTaskByRef is CreateTask for a creator that names the action and the required caller by
// address, here or on a peer, as a script and the HTTP surface both do. It answers with the task as
// delivered, which is how every surface reads one.
func (k *Kernel) CreateTaskByRef(ctx context.Context, traceID, actionRef, requiredCaller string, partialArgs json.RawMessage) (*TaskEntry, error) {
	action, err := k.ResolveAction(ctx, actionRef)
	if err != nil {
		return nil, err
	}
	caller, err := k.ResolvePrincipal(ctx, requiredCaller)
	if err != nil {
		return nil, err
	}
	task, err := k.CreateTask(ctx, traceID, action.ID, partialArgs, caller)
	if err != nil {
		return nil, err
	}
	return k.store.ReadMailbox(ctx, task.ID)
}

// ReadTask returns a mailbox entry (D4) if the caller may read it: the party it is addressed to, the
// owner of the process that funds it, or the superuser (D6).
func (k *Kernel) ReadTask(ctx context.Context, callerID, taskID string) (*TaskEntry, error) {
	e, err := k.store.ReadMailbox(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if callerID == "" || !e.RequiredCaller.IsUser(callerID) && callerID != e.OwnerUserID && !k.IsSuperuser(ctx, callerID) {
		return nil, ErrUnauthorized.Wrap("read permission denied")
	}
	return e, nil
}

// ListTasks returns the mailbox entries visible to the caller, wherever each task is held.
func (k *Kernel) ListTasks(ctx context.Context, callerID string, f TaskFilter) ([]*TaskEntry, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	f.CallerUserID, f.Superuser = callerID, k.isUserSuperuser(ctx, u)
	return k.store.ListMailbox(ctx, f)
}

// CancelTask declines a waiting task (D6) and returns its entry as it now stands. A task held by a
// peer is declined there, as the caller; one held here, by its required caller or process owner.
func (k *Kernel) CancelTask(ctx context.Context, callerID, taskID string) (*TaskEntry, error) {
	e, err := k.ReadTask(ctx, callerID, taskID)
	if err != nil {
		return nil, err
	}
	if e.Status != TaskWaiting {
		return nil, ErrInvalidState.Wrap("task is not waiting")
	}
	if e.HolderKey != k.SelfKey(ctx) {
		_, err = k.sendTask(ctx, callerID, e, "cancel", nil)
	} else {
		err = k.CancelTaskHeld(ctx, User(callerID), e.ID)
	}
	if err != nil {
		return nil, err
	}
	return k.store.ReadMailbox(ctx, e.ID)
}

// CancelTaskHeld declines a waiting task this kernel holds, for its required caller — a peer when
// the decline arrives over P8, as the principal its scope matched — or its process owner.
func (k *Kernel) CancelTaskHeld(ctx context.Context, caller Principal, taskID string) error {
	if _, err := k.requireCaller(ctx, caller); err != nil {
		return err
	}
	e, err := k.store.ReadMailbox(ctx, taskID)
	if err != nil {
		return err
	}
	if !caller.Same(e.RequiredCaller) && !caller.IsUser(e.OwnerUserID) {
		return ErrUnauthorized.Wrap("only the task's required caller or its process owner may decline it")
	}
	if err := k.store.CancelTask(ctx, taskID); err != nil {
		return err
	}
	k.WakeTell()
	k.log.With(ctx).Info("task.cancelled", "task_id", taskID)
	return nil
}

// CompleteTask resumes a waiting task by merging caller input with partial_args and executing the next call.
// No superuser exception: only required_caller_user_id may complete the task.
//
// Outcome contract — callers must read these, never re-read the task's status, which is racy and
// cannot distinguish "another completer claimed it" from "my own resumed call is still in flight":
//
//	err == nil                            the task was resumed and settled; reply is the result
//	reply != nil (with err)               a transaction committed for THIS completion and then
//	                                      failed; reply carries its TxID/ReceiptID (same contract
//	                                      as Call and RunFederated)
//	errors.Is(err, ErrTaskNotClaimed)     this completion never took the task — already claimed
//	                                      or already resolved by someone else; nothing happened
//	errors.Is(err, ErrTimeout)            claimed, dispatched to a peer, and awaiting its receipt;
//	                                      the task stays running for RetryPendingRemoteDispatches
//	otherwise (reply == nil)              rejected before anything settled; the task is waiting again
func (k *Kernel) CompleteTask(ctx context.Context, callerID, taskID string, input json.RawMessage) (*TaskReply, error) {
	e, err := k.ReadTask(ctx, callerID, taskID)
	if err != nil {
		return nil, err
	}
	if e.Status != TaskWaiting {
		return nil, ErrInvalidState.Wrap("task is not waiting").Because(ErrTaskNotClaimed)
	}
	if e.HolderKey != k.SelfKey(ctx) {
		return k.sendTask(ctx, callerID, e, "complete", input)
	}
	return k.completeTask(ctx, User(callerID), taskID, input, "", "", peerCompleter{})
}

// CompleteTaskInTrace resumes a task from inside a running execution — a WASM juice.task_complete or
// the HTTP capability twin. Authority there is the executing trace, not a session: the caller may only
// complete a task its own trace parked, so an endpoint holding a capability cannot reach tasks living
// in another process (§9 "no other trace"). Session and federated completion are unaffected: their
// authority is the account itself, which is exactly what required_caller names.
func (k *Kernel) CompleteTaskInTrace(ctx context.Context, callerID, traceID, taskID string, input json.RawMessage) (*TaskReply, error) {
	if traceID == "" {
		return nil, ErrUnauthorized.Wrap("in-execution completion requires an authorizing trace")
	}
	return k.completeTask(ctx, User(callerID), taskID, input, "", traceID, peerCompleter{})
}

// CompleteTaskFederated resumes a task on behalf of a peer, threading the inbound cross-kernel
// idempotency record (§13) so the commit that settles the call completes that record atomically —
// including a settlement that only happens later, via the remote-dispatch retry loop. Mirrors
// RunFederated, which does the same for an inbound call. scope is the principal the task is
// addressed to, already matched to the signed request (P8); forUserID is who on the peer completed.
func (k *Kernel) CompleteTaskFederated(ctx context.Context, scope Principal, taskID string, input json.RawMessage, idempotencyRecordID, forUserID string, superuser bool) (*TaskReply, error) {
	return k.completeTask(ctx, scope, taskID, input, idempotencyRecordID, "", peerCompleter{UserID: forUserID, Superuser: superuser})
}

// peerCompleter is who on the peer completed a task, as its home kernel signed it (P8): the user's
// stable id there, or its operator for a kernel-addressed task. Zero for a local completion.
type peerCompleter struct {
	UserID    string
	Superuser bool
}

// TaskRequiredCaller returns who a task is parked for, for the federation completion path to match
// against the signed request (P8).
func (k *Kernel) TaskRequiredCaller(ctx context.Context, taskID string) (Principal, error) {
	task, err := k.store.ReadTask(ctx, taskID)
	if err != nil {
		return Principal{}, err
	}
	return task.RequiredCaller(), nil
}

func (k *Kernel) completeTask(ctx context.Context, caller Principal, taskID string, input json.RawMessage, idempotencyRecordID, authorizingTraceID string, completer peerCompleter) (*TaskReply, error) {
	callerAcct, err := k.requireCaller(ctx, caller)
	if err != nil {
		return nil, err
	}
	task, err := k.store.ReadTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.Status != TaskWaiting {
		return nil, ErrInvalidState.Wrap("task is not waiting").Because(ErrTaskNotClaimed)
	}
	// Derive process from the task's parent trace.
	if task.ParentTraceID == nil {
		return nil, ErrInvalidState.Wrap("task has no parent trace")
	}
	parentTrace, err := k.store.ReadTrace(ctx, *task.ParentTraceID)
	if err != nil {
		return nil, ErrNotFound.Wrap("parent trace not found")
	}
	if _, err := k.readOpenProcess(ctx, parentTrace.ProcessID); err != nil {
		return nil, err
	}
	if !caller.Same(task.RequiredCaller()) {
		return nil, ErrUnauthorized.Wrap("only the task's required caller may complete it")
	}
	// In-execution completion is confined to the trace that authorized it (§10): a capability or WASM
	// host may resume only a task its own trace parked. Without this, holding one trace's capability
	// would reach every task addressed to that action's owner anywhere in the kernel — spending a
	// third party's parked funds. Session and federated callers pass "" and are unaffected.
	if authorizingTraceID != "" && *task.ParentTraceID != authorizingTraceID {
		return nil, ErrUnauthorized.Wrap("task belongs to another trace")
	}

	// Look up action early — needed for derived allowed schema validation.
	action, err := k.store.ReadAction(ctx, task.ActionID)
	if err != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}

	// Parse and validate input against derived allowed schema: action.input_schema \ keys(partial_args).
	var inputArgs, partial map[string]any
	if len(input) > 0 && json.Unmarshal(input, &inputArgs) != nil {
		return nil, ErrInvalidInput.Wrap("input must be a JSON object")
	}
	_ = json.Unmarshal(task.PartialArgs, &partial) // stored by CreateTask: an object, or a legacy null
	allowedSchema := DeriveAllowedSchema(action.InputSchema, task.PartialArgs)
	if len(allowedSchema) > 0 {
		if err := ValidateInput(allowedSchema, inputArgs); err != nil {
			return nil, err
		}
	}
	// Enforce the allowed-input rule §10: allowed input = action.input_schema \ keys(partial_args).
	// This only constrains keys when the schema declares properties; an unconstrained schema imposes
	// no restriction (the merge rule governs collisions there). The derived-schema check above misses
	// the all-bound case: when partial_args binds every declared property, DeriveAllowedSchema reduces
	// properties to {}, which ValidateInput treats as accept-any — letting a completer supply an
	// undeclared key or, worse, overwrite a creator-fixed value. Reject before any state mutation so
	// the task stays waiting and no action failure is recorded.
	if declared, ok := action.InputSchema["properties"].(map[string]any); ok && len(declared) > 0 {
		for key := range inputArgs {
			_, isDeclared := declared[key]
			_, isBound := partial[key]
			if !isDeclared || isBound {
				return nil, ErrSchemaViolation.Wrapf("input key %q is not an allowed completion key", key)
			}
		}
	}
	// The shallow merge (D6): input overwrites partial_args. A fresh map, so a null on either side
	// is simply nothing supplied.
	args := make(map[string]any, len(partial)+len(inputArgs))
	for key, v := range partial {
		args[key] = v
	}
	for key, v := range inputArgs {
		args[key] = v
	}

	// Value transfer (D18): a Task whose action bears the transfer effect delivers value on completion,
	// to a beneficiary here or on another kernel. Stage it over the merged args so BeginTaskCall locks
	// the value from the completer's own balance atomic with claiming the task — the completer funds the
	// value, the task's execution price stays creator-parked. A peer completer is refused (its account
	// holds nothing), and a non-transfer task stages nothing.
	eff, err := k.prepareTransferEffect(ctx, caller, action, args)
	if err != nil {
		return nil, err
	}
	// BeginTaskCall atomically marks the task as running, releases its parked price
	// from parent_trace.locked, and creates a new trace with available=task.price.
	taskTrace := &Trace{
		ID:            uuid.New().String(),
		ProcessID:     parentTrace.ProcessID,
		ParentTraceID: task.ParentTraceID,
		ActionID:      action.ID,
		CreatedAt:     time.Now().UTC(),
	}
	taskTrace.setTarget(action.Owner())
	// The completer as a principal (D15): a user here; on a peer, a user attested by its home kernel
	// and already matched to the task's addressing — the handle the task was made for — or its
	// operator completing a kernel-addressed task, that kernel's `sys`; else the peer kernel itself.
	done := caller
	switch {
	case caller.Local() || completer.UserID == "":
	case task.RequiredCallerUserID == completer.UserID:
		done.UserID, done.Handle = completer.UserID, task.RequiredCallerHandle
	case completer.Superuser:
		done.UserID, done.Handle = completer.UserID, SuperuserHandle
	default:
		done.UserID = completer.UserID
	}
	taskTrace.setCaller(done)
	if eff != nil {
		eff.stage(taskTrace)
	}
	// Same as a root call (kernel.go): the inbound record rides on the trace so any settlement
	// releases it, for every action kind rather than only remote proxies — and it is where the
	// receipt reads the request it answers (P4), so the trace's dispatch record stays free for the
	// proxy dispatch below.
	if idempotencyRecordID != "" {
		taskTrace.IdempotencyRecordID = &idempotencyRecordID
	}
	// For remote-proxy actions, generate and persist the idempotency key and dispatch
	// payload atomically with the trace creation. BeginTaskCall passes these through
	// insertTraceTx so they land in the DB before any network dispatch. This ensures
	// that on restart, ListPendingRemoteTraces finds the trace and RetryPendingRemoteDispatches
	// can resume with the same idempotency key — matching the §5 recovery guarantee.
	if action.Kind == KindRemoteProxy {
		key := uuid.New().String()
		taskTrace.IdempotencyKey = &key
		// Gross is the price the task PARKED, not the action's current one — those differ once the
		// catalog reprices — and the rate is the one frozen at creation, so a fee change between
		// parking and completion cannot move this task's arithmetic (§16).
		ibps := k.econ.ImportBPS
		if task.ImportBPS != nil {
			ibps = *task.ImportBPS
		}
		if err := k.prepareDispatch(ctx, taskTrace, action, args, taskID, task.Price, ibps, callerAcct); err != nil {
			return nil, err
		}
	}
	// A lost waiting→running CAS already carries ErrTaskNotClaimed from the store, which marks
	// only the two genuine claim races. Deliberately NOT relabelled here: BeginTaskCall also
	// reports a park-invariant violation as ErrInvalidState, and a blanket relabel would present
	// that ledger corruption to a gate as an ordinary lost race and silently drop the contribution.
	if err := k.store.BeginTaskCall(ctx, taskID, taskTrace); err != nil {
		return nil, err
	}

	reply, callErr := k.call(ctx, callRequest{
		Caller:              done,
		ExistingTraceID:     taskTrace.ID, // BeginTaskCall pre-created and funded this completion trace
		Action:              action,
		Args:                args,
		TaskID:              taskID, // marks the task done at commit + selects CallerTask wallet
		IdempotencyRecordID: idempotencyRecordID,
	})
	if callErr != nil {
		// A non-nil reply means Call committed a transaction, decided BEFORE classifying the error:
		// ErrTimeout is both a WASM timeout, which settles and charges, and a parked remote dispatch,
		// which commits nothing.
		if reply != nil {
			// Committed: the task is already done via CommitFailedCall, so no re-park.
			return &TaskReply{CallReply: reply, TaskID: taskID}, callErr
		}
		// Nothing committed. A remote dispatch may still be in flight: leave the task running so
		// RetryPendingRemoteDispatches can recover it via ListPendingRemoteTraces. Do NOT re-park —
		// that would delete the pending trace and lose the retry state.
		if errors.Is(callErr, ErrTimeout) {
			return nil, callErr
		}
		// Rejected before anything settled: re-park and reset to waiting so the task can be retried.
		if resetErr := k.store.ResetTaskAndRepark(ctx, taskID); resetErr != nil {
			k.log.With(ctx).Error("task.reset_failed", "task_id", taskID, "error", resetErr, "call_error", callErr)
		}
		return nil, callErr
	}
	k.log.With(ctx).Info("task.completed", "task_id", taskID, "tx_id", reply.TxID, "status", "success")
	return &TaskReply{CallReply: reply, TaskID: taskID}, nil
}

// DeriveAllowedSchema returns the subset of actionSchema that is not already covered by partialArgs.
// Properties and required fields whose keys appear in partialArgs are removed. It is the completer's
// allowed input (§10) and is surfaced on the task read view so a required caller can complete without
// separately reading a private target action.
func DeriveAllowedSchema(actionSchema map[string]any, partialArgs json.RawMessage) map[string]any {
	if len(actionSchema) == 0 {
		return actionSchema
	}
	var partial map[string]any
	if len(partialArgs) > 0 {
		_ = json.Unmarshal(partialArgs, &partial)
	}
	if len(partial) == 0 {
		return actionSchema
	}
	result := make(map[string]any, len(actionSchema))
	for k, v := range actionSchema {
		result[k] = v
	}
	if props, ok := result["properties"].(map[string]any); ok {
		newProps := make(map[string]any, len(props))
		for k, v := range props {
			if _, bound := partial[k]; !bound {
				newProps[k] = v
			}
		}
		result["properties"] = newProps
	}
	if req, ok := result["required"].([]any); ok {
		var newReq []any
		for _, r := range req {
			if s, ok := r.(string); ok {
				if _, bound := partial[s]; !bound {
					newReq = append(newReq, s)
				}
			}
		}
		result["required"] = newReq
	}
	return result
}
