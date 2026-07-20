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

// CallRequest is input to the central Call() operation. The dispatch mode is selected by which
// trace reference is set: ParentTraceID for a subcall (funded here by BeginSubcall), or
// ExistingTraceID for a pre-created, pre-funded trace — a root call (BeginRun) or a step
// completion (BeginStepCall). StepID is an orthogonal flag, not a third trace mode.
type CallRequest struct {
	// CallerID is the authenticated user making the call.
	CallerID string
	// ParentTraceID is the parent trace of a subcall; the call's funds are moved from it by
	// BeginSubcall. Empty for root calls and step completions (those set ExistingTraceID).
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
	// StepID, if non-empty, causes CommitCall/CommitFailedCall to atomically mark the step done
	// and selects the CallerStep wallet kind (BeginStepCall already released the parent lock).
	// It accompanies ExistingTraceID on a step completion; it is not itself a trace reference.
	StepID string
	// ExistingTraceID names a trace already created and funded atomically by its wrapper —
	// BeginRun (root call) or BeginStepCall (step completion). Call adopts it instead of
	// calling BeginSubcall, and uses its pre-locked amount as gross.
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

// ResolveAction resolves an action reference to an Action. It accepts "@owner/name"
// (with or without a leading "@") or a raw action ID, disambiguated by the "/" that a
// UUID never contains. This is the single action-resolution entry point shared by Call,
// Run, the WASM host, and the service layer; do not re-inline the lookup elsewhere.
func (k *Kernel) ResolveAction(ctx context.Context, ref string) (*Action, error) {
	if i := strings.Index(ref, "/"); i >= 0 {
		ownerRef, name := ref[:i], ref[i+1:]
		if ownerRef == "" || name == "" {
			return nil, ErrInvalidInput.Wrap("action ref must be @owner/name")
		}
		// The owner segment is itself a user reference, resolved uniformly (@handle, key, or id).
		owner, err := k.ResolveUser(ctx, ownerRef)
		if err != nil || owner == nil {
			return nil, ErrNotFound.Wrapf("action %s not found", ref)
		}
		a, err := k.store.ReadActionByOwnerName(ctx, owner.ID, name)
		if err != nil || a == nil {
			return nil, ErrNotFound.Wrapf("action %s not found", ref)
		}
		return a, nil
	}
	a, err := k.store.ReadAction(ctx, ref)
	if err != nil || a == nil {
		return nil, ErrNotFound.Wrapf("action %s not found", ref)
	}
	return a, nil
}

// ResolveUser resolves a user reference to a User. It accepts "@handle" (or a bare
// handle), a base64url public key, or a raw user ID — the shapes are disjoint, so a
// single lookup disambiguates. This is the single user-resolution entry point shared by
// Call, Run, the WASM host, native actions, federation, and the service layer.
func (k *Kernel) ResolveUser(ctx context.Context, ident string) (*User, error) {
	if !strings.HasPrefix(ident, "@") {
		if u, err := k.store.ReadUserByPublicKey(ctx, ident); err == nil && u != nil {
			return u, nil
		}
	}
	if u, err := k.store.ReadUserByHandle(ctx, NormalizeHandle(ident)); err == nil && u != nil {
		return u, nil
	}
	if u, err := k.store.ReadUser(ctx, ident); err == nil && u != nil {
		return u, nil
	}
	return nil, ErrNotFound.Wrapf("user %s not found", ident)
}

// Call executes the central kernel transition.
// Preconditions are checked in order per §5.1 of the requirements.
// For root calls (req.ExistingTraceID), the process and trace must already have been created by Run().
// For subcalls, the parent trace must have sufficient available funds.
// For step-completion calls, BeginStepCall must have been called before invoking Call.
func (k *Kernel) Call(ctx context.Context, req CallRequest) (*CallReply, error) {
	logger := k.log.With(ctx)

	// 1. Subject must be authenticated. The caller User is retained for the §4 precondition-6
	// visibility check (canCall is caller-scoped).
	if req.CallerID == "" {
		return nil, ErrUnauthenticated.Wrap("subject is required")
	}
	caller, err := k.requireActiveUser(ctx, req.CallerID)
	if err != nil {
		return nil, err
	}

	// 1.5: Derive processID from the trace reference and read the referenced trace once.
	// ExistingTraceID names a trace already created and funded by its wrapper — BeginRun for a
	// root call, BeginStepCall for a step completion; preReadExisting is reused in section 8 to
	// avoid a second read. ParentTraceID names a subcall's parent.
	var processID string
	var preReadParent, preReadExisting *Trace
	switch {
	case req.ExistingTraceID != "":
		rt, err := k.store.ReadTrace(ctx, req.ExistingTraceID)
		if err != nil {
			return nil, ErrNotFound.Wrap("trace not found")
		}
		processID = rt.ProcessID
		preReadExisting = rt
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

	// 3. Process-use authority (§4 precondition 4), enforced here for subcalls. Root calls have
	// C = P by construction (beginRun) and step completions are checked by CompleteStep; both
	// arrive via ExistingTraceID and satisfy it before reaching Call.
	var parentTrace *Trace
	if req.ExistingTraceID == "" {
		// preReadParent is the parent trace (derived processID came from it, so membership is implicit).
		// Non-owner callers must have action_owner_id on the parent trace.
		if process.OwnerUserID != req.CallerID && preReadParent.ActionOwnerID != req.CallerID {
			return nil, ErrUnauthorized.Wrap("caller is not authorized to use this process")
		}
		parentTrace = preReadParent
	}

	// 4. Resolve action.
	// Root calls and step completions supply a pre-resolved Action (read by beginRun /
	// CompleteStep), so no DB read is needed — this binds execution to the exact action that
	// was funded and eliminates the TOCTOU window. The snapshot is still validated below.
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
			action, err = k.ResolveAction(ctx, req.ActionRef)
			if err != nil || action == nil {
				return nil, ErrNotFound.Wrapf("action %s not found", req.ActionRef)
			}
		} else {
			target, err = k.ResolveUser(ctx, req.TargetUserID)
			if err != nil || target == nil {
				return nil, ErrNotFound.Wrap("target user not found")
			}
			action, err = k.store.ReadActionByOwnerName(ctx, target.ID, req.ActionName)
			if err != nil || action == nil {
				return nil, ErrNotFound.Wrapf("action %s/%s not found", req.TargetUserID, req.ActionName)
			}
		}
		// The ActionRef path resolves the action directly; load its owner for the role law below.
		if target == nil {
			target, err = k.store.ReadUser(ctx, action.OwnerUserID)
			if err != nil || target == nil {
				return nil, ErrNotFound.Wrap("target user not found")
			}
		}
	}

	// 5 + 6. CanCall(process.owner, action) and input-schema validation, enforced for every
	// path (root, step, subcall). The pre-resolved snapshot (req.Action) is validated, so root
	// calls are checked here too with no extra DB read and no TOCTOU window — Call is the single
	// validity function; no entry path bypasses it (beginRun runs the same check before funding).
	if err := k.checkCallPreconditions(ctx, caller, process.OwnerUserID, action, req.Args); err != nil {
		return nil, err
	}

	// 7. Funds check. Only subcalls check here; ExistingTraceID calls (root via BeginRun, step
	// completion via BeginStepCall) are pre-funded with their exact allocation.
	if req.ExistingTraceID == "" {
		if parentTrace != nil && parentTrace.Available < action.Price {
			return nil, ErrInsufficientFunds.Wrapf("parent trace has %d credits, action costs %d", parentTrace.Available, action.Price)
		}
	}

	// Kernel must be bootstrapped (signing key present) to issue receipts.
	if err := k.requireReceiptSigningReady(); err != nil {
		return nil, err
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
		mp = k.remoteManifestPrice(action.Price)
		key := uuid.New().String()
		trace.IdempotencyKey = &key
		// Subcalls carry no inbound record: only a ROOT call serves a peer directly, and completing
		// the inbound record from a settling subcall would answer the peer before its own call resolved.
		trace.DispatchJSON = marshalDispatch(req.Args, req.StepID, mp, "")
	}

	callerWalletID, callerWalletKind := k.callerWallet(req, process, parentTrace)

	switch {
	case req.ExistingTraceID != "":
		// Trace was pre-created and funded atomically by its wrapper (BeginRun for a root call,
		// BeginStepCall for a step completion); skip BeginSubcall and adopt that trace. Use its
		// pre-locked amount as gross — for a step that is step.price, the snapshot taken at step
		// creation, not the action's possibly-changed current price. preReadExisting was read in
		// section 1.5, so no second read is needed.
		trace.ID = req.ExistingTraceID
		lockPrice = applyPrefundedSnapshot(trace, preReadExisting)
	default:
		// Lock the parent trace so a subcall's fund-move cannot interleave with that trace's
		// settlement taxable-read→commit (§9 capability composition fence).
		pmu := k.traceLock(req.ParentTraceID)
		pmu.Lock()
		err := k.store.BeginSubcall(ctx, req.ParentTraceID, trace, lockPrice)
		pmu.Unlock()
		if err != nil {
			if errors.Is(err, ErrInsufficientFunds) {
				return nil, err
			}
			return nil, ErrInternal.Wrap("could not begin subcall")
		}
	}

	txID := uuid.New().String()
	ctx = log.WithProcessID(ctx, processID)
	ctx = log.WithCallerUserID(ctx, req.CallerID)
	ctx = log.WithCallerHandle(ctx, k.callerHandle(ctx, req.CallerID))
	ctx = log.WithTraceID(ctx, trace.ID)
	ctx = log.WithActionID(ctx, action.ID)
	ctx = log.WithTxID(ctx, txID)
	logger = k.log.With(ctx)
	logger.Info("call.start", "action", action.Name, "price", lockPrice)

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
			receipt, sErr := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, 0, cfgErr)
			if sErr != nil {
				return nil, sErr
			}
			return &CallReply{TxID: ktx.ID, TraceID: trace.ID, ReceiptID: receipt.ID}, cfgErr
		}
		ikey := ""
		if trace.IdempotencyKey != nil {
			ikey = *trace.IdempotencyKey
		}
		fr, _ := fe.ExecuteFederation(ctx, target.PublicKey, action.Source, ikey, req.Args)
		latency := time.Since(started).Seconds()
		ktx.EndedAt = time.Now().UTC()
		if fr.NotDispatched {
			// First dispatch, provably never sent (§13 never-dispatched): settle as an ordinary
			// local failure with a full refund now, rather than parking the allocation for a retry
			// that would repeat a request the peer never received. Only here — retryRemoteTrace
			// never fail-fasts, since a parked request may already have executed. Mirrors the
			// executor-not-configured settlement above (a funded trace must never be stranded).
			unreach := ErrPeerUnreachable.Wrapf("peer @%s is unreachable; the call was not sent and has been refunded", target.Handle).WithMeta("peer", target.Handle)
			ktx.Status = TxFailure
			ktx.Reason = unreach.Error()
			receipt, sErr := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, unreach)
			if sErr != nil {
				return nil, sErr
			}
			logger.Warn("remote.unreachable", "action", action.Name, "peer", target.Handle)
			return &CallReply{TxID: ktx.ID, TraceID: trace.ID, ReceiptID: receipt.ID}, unreach
		}
		return k.settleRemoteCall(ctx, logger, action, ktx, trace, callerWalletID, callerWalletKind, req, target, mp, fr, latency)
	}

	reply, _, execErr := k.execute(ctx, action, req.Args, trace, action.OwnerUserID, req.CallerID, process.OwnerUserID)
	latency := time.Since(started).Seconds()
	ktx.EndedAt = time.Now().UTC()

	if execErr != nil {
		ktx.Status = TxFailure
		ktx.Reason = execErr.Error()
		receipt, sErr := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, execErr)
		if sErr != nil {
			return nil, sErr
		}
		logger.Warn("call.failed", "action", action.Name, "error", execErr)
		return &CallReply{TxID: ktx.ID, TraceID: trace.ID, ReceiptID: receipt.ID}, execErr
	}

	// 10. Validate output schema.
	if schemaErr := ValidateInput(action.OutputSchema, any(reply)); schemaErr != nil {
		ktx.Status = TxFailure
		ktx.Reason = "output schema violation: " + schemaErr.Error()
		receipt, sErr := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, schemaErr)
		if sErr != nil {
			return nil, sErr
		}
		return &CallReply{TxID: ktx.ID, TraceID: trace.ID, ReceiptID: receipt.ID}, schemaErr
	}

	// 11. Read trace.available post-execution — this is the taxable amount.
	// trace.available decreases with each subcall (BeginSubcall) and step park (CreateStep).
	// The taxable read and the commit that zeroes it must be atomic against a concurrent
	// capability spend (§9), so both run under this trace's lock; error paths release it
	// before delegating to settleFailedCall (which re-acquires it).
	mu := k.traceLock(trace.ID)
	mu.Lock()
	postTrace, readErr := k.store.ReadTrace(ctx, trace.ID)
	if readErr != nil {
		mu.Unlock()
		// If we can't read the trace, settle as failure to avoid fund loss.
		ktx.Status = TxFailure
		ktx.Reason = "could not read trace post-execution"
		_, _ = k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, readErr)
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
		mu.Unlock()
		ktx.Status = TxFailure
		ktx.Reason = "could not build receipt"
		_, _ = k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, latency, receiptErr)
		return nil, ErrInternal.Wrap("could not build receipt")
	}
	// Detach settlement from execution-scoped cancellation so the success commit
	// (payout + lock release + audit record) is never aborted mid-flight (§5).
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	commitErr := k.store.CommitCall(sctx, ktx, receipt, trace.ID, callerWalletID, callerWalletKind, target.ID, k.cfg.FeeRecipientID, net, fee, stats, req.IdempotencyRecordID, req.StepID)
	mu.Unlock()
	if commitErr != nil {
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
// Root calls (ExistingTraceID) have no parent trace, so they resolve to CallerProcess.
func (k *Kernel) callerWallet(req CallRequest, process *Process, parentTrace *Trace) (id, kind string) {
	var parentTraceID *string
	if parentTrace != nil {
		parentTraceID = &parentTrace.ID
	}
	return callerWalletFor(req.StepID, process.ID, parentTraceID)
}

// applyPrefundedSnapshot copies the pre-funded state from a persisted trace onto the in-memory
// trace and returns the locked amount (= dbTrace.Available) to use as gross. Shared by both
// pre-created-trace dispatch paths: root calls (BeginRun) and step completions (BeginStepCall).
// ParentTraceID is copied from the persisted trace — null for a root trace, the step's parent
// for a completion trace — so the recorded causality is correct without a special case.
func applyPrefundedSnapshot(trace, dbTrace *Trace) int64 {
	trace.Available = dbTrace.Available
	trace.ParentTraceID = dbTrace.ParentTraceID
	trace.IdempotencyKey = dbTrace.IdempotencyKey
	trace.DispatchJSON = dbTrace.DispatchJSON
	return dbTrace.Available
}

// canCall returns true iff the action is callable by the immediate caller (§4). Visibility is
// scoped to the caller, not the process owner, so a provider's public action may subcall the
// provider's own private helpers in anyone's process, while foreign code funded by a process owner
// cannot reach that owner's private actions.
// CanCall(C, a) := active(a) ∧ ¬suspended(a.owner) ∧
//
//	(public(a) ∨ (local(a) ∧ ¬IsPeer(C)) ∨ C = a.OwnerUserID)
func canCall(caller *User, action *Action) bool {
	if !action.Active || action.OwnerSuspended {
		return false
	}
	switch action.Visibility {
	case VisibilityPublic:
		return true
	case VisibilityLocal:
		return caller != nil && !caller.IsPeer()
	default: // private
		return caller != nil && caller.ID == action.OwnerUserID
	}
}

// checkCallPreconditions enforces the §4 semantic call-validity rules (steps 6 and 7) for a
// resolved action: CanCall by the immediate caller, and input against the action's schema. It is
// the single validity function — Call runs it unconditionally for every entry path, and
// beginRun runs it once before funding so an invalid root call never creates a funded process
// (a precondition rejection must create no transaction, §6). The grant check stays keyed on the
// process owner: delegated consent binds to the paying human, never the caller (§8).
func (k *Kernel) checkCallPreconditions(ctx context.Context, caller *User, processOwnerID string, action *Action, args map[string]any) error {
	if !canCall(caller, action) {
		if !action.Active {
			return ErrInvalidState.Wrap("action is inactive")
		}
		if action.OwnerSuspended {
			return ErrInvalidState.Wrap("action owner is suspended")
		}
		return ErrUnauthorized.Wrap("call permission denied")
	}
	if err := ValidateInput(action.InputSchema, args); err != nil {
		return err
	}
	return k.checkGrantRequired(ctx, processOwnerID, action)
}

// checkGrantRequired implements §8 lazy consent: a call to a delegated http action (oauth_delegated
// or delegated_bearer) whose process owner holds no matching grant is rejected here — before any
// funds are locked and before any transaction exists (a precondition rejection creates no
// transaction, §6, and does not dent the provider's failure stats, §9). Non-delegated actions pass
// through untouched. An undecryptable or malformed auth payload is left for the executor's
// fail-closed path, not treated as consent.
func (k *Kernel) checkGrantRequired(ctx context.Context, ownerID string, action *Action) error {
	if action.Kind != KindHTTP || action.AuthJSON == "" || k.secretBox == nil {
		return nil
	}
	auth, err := k.openAuthInput(action)
	if err != nil || auth == nil || !isDelegatedScheme(auth.Scheme) {
		return nil
	}
	if _, gerr := k.store.ReadGrant(ctx, ownerID, action.ID); gerr != nil {
		if errors.Is(gerr, ErrNotFound) {
			return GrantRequiredError(k.actionRefOf(ctx, action))
		}
		return gerr
	}
	return nil
}

// execute dispatches to the correct execution backend for HTTP, WASM, and native actions.
// KindRemoteProxy is handled separately in Call() via ExecuteFederation.
func (k *Kernel) execute(ctx context.Context, action *Action, args map[string]any, trace *Trace, targetID, callerID, ownerUserID string) (map[string]any, string, error) {
	switch action.Kind {
	case KindHTTP:
		if k.http == nil {
			return nil, "", ErrInvalidState.Wrap("HTTP executor not configured")
		}
		// Mint a trace-scoped capability so the endpoint can compose within this call (§9).
		// Signing is ready by dispatch (requireReceiptSigningReady, checked in Call); on the
		// off chance it is not, an empty capability just disables composition for this call.
		capability, _ := k.IssueCapability(trace.ID)
		res, err := k.http.Execute(ctx, action, args, ownerUserID, capability)
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
	// The SDK reports a Handle error as the reserved sole-key object
	// {WasmErrorKey:"<message>"} rather than trapping, so the failure reason reaches
	// the caller. The sole-key guard keeps legitimate output that happens to contain
	// the key from being misclassified.
	if msg, ok := WasmHandleError(result); ok {
		return nil, ErrExecutionFailed.Wrap(msg)
	}
	return result, nil
}

// WasmErrorKey is the reserved sole key the WASM SDK uses to report a Handle error
// as JSON output (see script/sdk.tmpl) instead of trapping the module. Every consumer
// of raw SDK output must recognize it: kernel.executeWasm maps it to ErrExecutionFailed.
const WasmErrorKey = "__juice_error__"

// WasmHandleError reports whether a decoded WASM output object is the SDK's error
// envelope, returning the carried message. The sole-key guard prevents legitimate
// output that merely contains the key from being misclassified as an error.
func WasmHandleError(output map[string]any) (string, bool) {
	if len(output) != 1 {
		return "", false
	}
	msg, ok := output[WasmErrorKey].(string)
	return msg, ok
}

// kernelHostFunctions implements HostFunctions using the kernel itself.
// Scripts never receive the caller's JWT — they inherit trace authority.
type kernelHostFunctions struct {
	kernel   *Kernel
	traceID  string
	targetID string // action owner; used as CallerID for subcalls
}

func (h *kernelHostFunctions) Call(ctx context.Context, actionName string, argsJSON []byte) ([]byte, error) {
	var args map[string]any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return nil, ErrInvalidInput.Wrap("args must be a JSON object")
	}
	reply, err := h.kernel.Call(ctx, CallRequest{
		CallerID:      h.targetID,
		ParentTraceID: h.traceID,
		ActionRef:     actionName,
		Args:          args,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(reply.Result)
}

// StepCreate resolves the onward action and required caller through the canonical resolvers
// (§ ResolveAction/ResolveUser), so a script may name them by @owner/name and @handle — or id —
// exactly like juice.call. CreateStep itself stays an id-only primitive.
func (h *kernelHostFunctions) StepCreate(ctx context.Context, partialArgs []byte, requiredCaller, action string) (string, error) {
	act, err := h.kernel.ResolveAction(ctx, action)
	if err != nil {
		return "", err
	}
	caller, err := h.kernel.ResolveUser(ctx, requiredCaller)
	if err != nil {
		return "", err
	}
	step, err := h.kernel.CreateStep(ctx, h.traceID, act.ID,
		json.RawMessage(partialArgs), caller.ID)
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
// It returns the committed receipt so callers can surface the real charge (e.g. an inbound
// federation call that failed after settling descendants must return that receipt, not a
// zero-charge rejection).
func (k *Kernel) settleFailedCall(ctx context.Context, logger *log.Logger, tx *Transaction, traceID, callerWalletID, callerWalletKind string, req CallRequest, action *Action, latency float64, callErr error) (*Receipt, error) {
	// Settlement is a money transition + its audit record (§5); it must commit even
	// if the call timed out or the client disconnected. Detach from execution-scoped
	// cancellation so a cancelled/contended ctx can never strand the locked allocation.
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	ctx = sctx
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
	var committed *Receipt
	buildFn := func(refund int64) (*Receipt, error) {
		r, err := k.buildReceipt(tx, tx.Gross-refund)
		committed = r
		return r, err
	}
	if settlErr := k.store.CommitFailedCall(ctx, tx, buildFn, traceID, callerWalletID, callerWalletKind, tx.Gross, stats, req.IdempotencyRecordID, KernelErrorCode(callErr), req.StepID); settlErr != nil {
		logger.Error("call.settlement_failed", "action", action.Name, "error", callErr, "settlement_error", settlErr)
		return nil, ErrInternal.Wrap("could not record failure transaction")
	}
	return committed, nil
}

// settlementContext derives a context for committing a money transition and its
// audit record. It strips execution-scoped cancellation/deadline (so a timed-out
// or client-cancelled call still settles and never strands locked funds, §5) while
// preserving log/trace values, then bounds the write with its own timeout as a
// backstop against a wedged single-connection store. Callers must defer cancel().
func settlementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
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
