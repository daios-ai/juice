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

// callRequest is input to the central Call() operation. The dispatch mode is selected by which
// trace reference is set: ParentTraceID for a subcall (funded here by BeginSubcall), or
// ExistingTraceID for a pre-created, pre-funded trace — a root call (BeginRun) or a step
// completion (BeginStepCall). StepID is an orthogonal flag, not a third trace mode.
type callRequest struct {
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

// RunRequest is input to Run, the ordinary root-call entry point (§4): a struct so a new optional
// term is a field, not a signature break at every call site. Federation ingress keeps its own entry
// point (RunFederated), so its authority is not expressible here.
type RunRequest struct {
	CallerID  string
	ActionRef string // owner/name, owner@kernel/name, or a raw action id
	Args      map[string]any
	QuoteHash string // optional §4-precondition-7 pin; empty means the caller pinned nothing
}

// CallReply is the response from a successful Call().
type CallReply struct {
	Result    map[string]any `json:"result"`
	TxID      string         `json:"tx_id"`
	TraceID   string         `json:"trace_id"`
	ReceiptID string         `json:"receipt_id"`
}

// ActionRef is a parsed user[@kernel]/action reference (§13). Kernel is "" for a local action.
type ActionRef struct {
	Owner  string // bare owner handle
	Kernel string // local kernel alias or raw key; "" = local
	Name   string // action name (may itself contain "/")
}

// Local reports whether the reference names an action on this kernel.
func (r ActionRef) Local() bool { return r.Kernel == "" }

// String renders the canonical form owner[@kernel]/name.
func (r ActionRef) String() string {
	if r.Kernel == "" {
		return r.Owner + "/" + r.Name
	}
	return r.Owner + "@" + r.Kernel + "/" + r.Name
}

// ParseActionRef parses a "user[@kernel]/action" reference (§13, §14): `@` qualifies a kernel,
// `/` namespaces the action, handles are bare (no sigil). Returns ErrInvalidInput if the format is
// invalid — including a sigil-prefixed "@owner/name", which parses to an empty owner and is rejected.
func ParseActionRef(ref string) (ActionRef, error) {
	ref = strings.TrimSpace(ref)
	i := strings.Index(ref, "/")
	if i < 0 {
		return ActionRef{}, ErrInvalidInput.Wrap("action ref must be owner[@kernel]/name")
	}
	head, name := ref[:i], ref[i+1:]
	if head == "" || name == "" {
		return ActionRef{}, ErrInvalidInput.Wrap("action ref must be owner[@kernel]/name")
	}
	owner, kernel, hasKernel := strings.Cut(head, "@")
	if hasKernel && (owner == "" || kernel == "") {
		return ActionRef{}, ErrInvalidInput.Wrap("action ref must be owner[@kernel]/name")
	}
	return ActionRef{Owner: owner, Kernel: kernel, Name: name}, nil
}

// FormatActionRef renders an action's canonical user[@kernel]/action reference. For a remote proxy
// the stored Name is "remoteowner/rest" and OwnerHandle is the local mount alias, so it renders
// "remoteowner@mount/rest"; a local action renders "ownerHandle/name". Falls back to the bare name
// when OwnerHandle is not loaded. The single renderer shared by gossip, roster, and view enrichment.
func FormatActionRef(a *Action) string {
	if a.Kind == KindRemoteProxy && a.OwnerHandle != "" {
		if ro, rest, ok := strings.Cut(a.Name, "/"); ok {
			return ro + "@" + a.OwnerHandle + "/" + rest
		}
	}
	if a.OwnerHandle != "" {
		return a.OwnerHandle + "/" + a.Name
	}
	return a.Name
}

// quoteTerms is the advertised execution quote a buyer is shown, plus whether the action engages
// the transfer-value channel. Named a quote, not a contract: expected_contract_hash already means
// the full federation manifest If-Match (§8), and this is a smaller, buyer-facing fingerprint. It
// binds neither implementation (a same-terms source or artifact change is not caught) nor a
// transfer total, whose value fees are read from live config at funding (§13).
type quoteTerms struct {
	ActionID     string         `json:"action_id"` // stable identity: the remote id for a proxy
	Effect       string         `json:"effect"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
	Price        int64          `json:"price"`
}

// QuoteHash fingerprints the terms a caller was quoted for an action (§4 precondition 7). It keys
// on the STABLE id — a proxy's remote id, never its local cache UUID — so a discovered lookup hit
// and the local proxy it resolves to hash identically and a hash read from lookup binds a first
// cross-kernel call. Effect is included because it alone decides whether a call locks a value
// reserve (§13): a peer promoting effect under unchanged terms would otherwise pass.
func QuoteHash(a *Action) string { return quoteHashOf(quoteTermsOfAction(a)) }

// quoteTermsOfAction projects a stored action onto its quote, under the STABLE id.
func quoteTermsOfAction(a *Action) quoteTerms {
	id := a.RemoteActionID
	if id == "" {
		id = a.ID
	}
	return quoteTerms{
		ActionID: id, Effect: a.Effect, Description: a.Description,
		InputSchema: a.InputSchema, OutputSchema: a.OutputSchema, Price: a.Price,
	}
}

// quoteTermsOfDoc projects a gossiped discovery doc onto the same quote at the all-in price shown,
// so a discovered hit and the proxy it resolves to hash identically (§15).
func quoteTermsOfDoc(d *DiscoveryDoc, price int64) quoteTerms {
	return quoteTerms{
		ActionID: d.ActionID, Effect: d.Effect, Description: d.Description,
		InputSchema: d.InputSchema, OutputSchema: d.OutputSchema, Price: price,
	}
}

func quoteHashOf(t quoteTerms) string {
	payload, _ := CanonicalJSON(t)
	return sha256Hex(string(payload))
}

// IsPublicKey reports whether s has the syntactic form of a base64url Ed25519 public key. Exported
// for the service/CLI layer's shape-based peer resolution (§14 productions).
func IsPublicKey(s string) bool { return looksLikeKey(s) }

// looksLikeKey reports whether s is a base64url Ed25519 public key (43 chars → 32 bytes).
func looksLikeKey(s string) bool {
	if len(s) != 43 {
		return false
	}
	_, err := decodeRemotePublicKey(s)
	return err == nil
}

// looksLikeID reports whether s is a hex UUID (8-4-4-4-12) — a raw object id.
func looksLikeID(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}

// ResolveAction resolves an action reference to an Action. It accepts "@owner/name"
// (with or without a leading "@") or a raw action ID, disambiguated by the "/" that a
// UUID never contains. This is the single action-resolution entry point shared by Call,
// Run, the WASM host, and the service layer; do not re-inline the lookup elsewhere.
func (k *Kernel) ResolveAction(ctx context.Context, ref string) (*Action, error) {
	if strings.Contains(ref, "/") {
		r, err := ParseActionRef(ref)
		if err != nil {
			return nil, err
		}
		if r.Local() {
			owner, err := k.ResolveUser(ctx, r.Owner)
			if err != nil || owner == nil {
				return nil, ErrNotFound.Wrapf("action %s not found", ref)
			}
			a, err := k.store.ReadActionByOwnerName(ctx, owner.ID, r.Name)
			if err != nil || a == nil {
				return nil, ErrNotFound.Wrapf("action %s not found", ref)
			}
			// A proxy is stored under its mount user named "owner/name", so a bare
			// "mount/owner/name" would otherwise resolve here (the legacy mount form). Proxies are
			// addressable only kernel-qualified (owner@kernel/name), so reject it (§8, §13 grammar).
			if a.Kind == KindRemoteProxy {
				return nil, ErrNotFound.Wrapf("action %s not found", ref)
			}
			return a, nil
		}
		// Kernel-qualified: the local proxy cache row is owned by the peer's mount user and named
		// "owner/name" (§8). Resolve the mount by bound alias or raw key; a discovered label never
		// resolves (§13). A cached row is the fast path; a miss triggers on-demand resolve (§13).
		peerKey, mount, kerr := k.ResolveKernelKey(ctx, r.Kernel)
		if kerr != nil {
			return nil, ErrNotFound.Wrapf("action %s not found", ref)
		}
		if mount != nil {
			// An active cached proxy is the fast path; an absent OR inactive row is a cache miss that
			// re-resolves (§8 rule A) — reconcile preserves the id and re-enables, so an inactive proxy
			// (drift-deactivated by a prior refresh_proxy/quarantine, §13) is never permanently dead.
			// A row missing its seller price is a cache miss too: its total was frozen at import and
			// cannot be re-derived, so it re-resolves once to acquire one rather than have it
			// reverse-calculated from a rounded total (§16). Self-healing and one round-trip only.
			if a, aerr := k.store.ReadActionByOwnerName(ctx, mount.ID, r.Owner+"/"+r.Name); aerr == nil && a != nil && a.Active && a.BasePrice != nil {
				return a, nil
			}
		}
		a, lerr := k.lazyResolveRemote(ctx, peerKey, mount, r)
		if lerr != nil {
			if errors.Is(lerr, ErrNotFound) {
				return nil, ErrNotFound.Wrapf("action %s not found", ref)
			}
			return nil, lerr
		}
		return a, nil
	}
	a, err := k.store.ReadAction(ctx, ref)
	if err != nil || a == nil {
		return nil, ErrNotFound.Wrapf("action %s not found", ref)
	}
	return k.ensureBasePrice(ctx, a)
}

// ensureBasePrice heals a proxy imported before the seller's price was stored (§16). Such a row's
// total is frozen and its seller price unrecoverable — reversing the rounded total cannot recover it
// — so it re-resolves once from the signed manifest. Called wherever a row is about to be FUNDED,
// which includes CreateStep and a resolve by raw action id, not only a call by reference: dispatch
// records the seller's price, and a legacy row would otherwise record its local total there and
// quarantine the peer's perfectly valid receipt.
//
// It re-enters ResolveAction by the row's kernel-qualified reference, so healing reuses the one
// resolve path rather than opening a second import route. Anything other than a proxy, or a proxy
// that already has its price, is returned untouched.
func (k *Kernel) ensureBasePrice(ctx context.Context, a *Action) (*Action, error) {
	if a == nil || a.Kind != KindRemoteProxy || a.BasePrice != nil {
		return a, nil
	}
	owner, rest, ok := strings.Cut(a.Name, "/")
	mount, merr := k.store.ReadUser(ctx, a.OwnerUserID)
	if !ok || merr != nil || mount == nil || mount.KernelPublicKey == "" {
		return a, nil // not addressable as a remote reference; leave it as it is
	}
	healed, herr := k.ResolveAction(ctx, owner+"@"+mount.KernelPublicKey+"/"+rest)
	if herr != nil {
		return nil, herr // the peer must be reachable to fund a call on it anyway
	}
	return healed, nil
}

// lazyResolveRemote resolves a single remote action on demand and caches it as a local proxy row
// (§13 subscription-free calls). It is invoked from ResolveAction's kernel-qualified miss branch, so
// run, /v1/call, and WASM subcalls all reach unimported remote actions uniformly. Trust derives from
// the manifest signature, not an operator act; a nil resolver (no transport) yields ErrNotFound.
func (k *Kernel) lazyResolveRemote(ctx context.Context, peerKey string, mount *Account, r ActionRef) (*Action, error) {
	resolver := k.fedClient
	if resolver == nil {
		return nil, ErrNotFound.Wrapf("action %s not found", r.String())
	}
	m, err := resolver.ResolveRemoteAction(ctx, peerKey, r.Owner, r.Name)
	if err != nil {
		return nil, err // ErrPeerUnreachable / ErrNotFound already typed by the resolver
	}
	if m == nil {
		return nil, ErrNotFound.Wrapf("action %s not found", r.String())
	}
	if err := VerifyManifestSignature(peerKey, m); err != nil {
		return nil, ErrUnauthorized.Wrap("remote manifest signature is invalid")
	}
	// First meaningful use (§13): our own verified outbound act, so this is where a local petname
	// is bound — seeded from the kernel's cached nickname when one is known, else mechanically.
	// Naming turns on the petname being unbound, NOT on the account being absent: a peer that
	// called us first, or that we deposited to, already holds an account and would otherwise stay
	// nameless forever. A non-exact bind keeps any existing petname, so this is idempotent.
	// Best-effort and must never fail the call; the account must.
	if _, berr := k.BindPetname(ctx, peerKey, "", false); berr != nil {
		k.log.With(ctx).Warn("kernel.petname.bind_failed", "public_key", peerKey, "error", berr.Error())
	}
	if mount == nil {
		mount, err = k.EnsureKernelAccount(ctx, peerKey)
		if err != nil {
			return nil, err
		}
	}
	return k.ImportPeerAction(ctx, mount.ID, *m)
}

// ResolveUser resolves a user reference to a User. It accepts a bare handle, a base64url
// public key, or a raw user ID — the shapes are disjoint, so a single lookup disambiguates.
// This is the single user-resolution entry point shared by Call, Run, the WASM host, native
// actions, federation, and the service layer.
func (k *Kernel) ResolveUser(ctx context.Context, ident string) (*Account, error) {
	ident = strings.TrimSpace(ident)
	if looksLikeKey(ident) {
		if u, err := k.store.ReadAccountByKernelKey(ctx, ident); err == nil && u != nil {
			return u, nil
		}
	}
	if looksLikeID(ident) {
		if u, err := k.store.ReadUser(ctx, ident); err == nil && u != nil {
			return u, nil
		}
	}
	if u, err := k.store.ReadUserByHandle(ctx, ident); err == nil && u != nil {
		return u, nil
	}
	return nil, ErrNotFound.Wrapf("user %s not found", ident)
}

// Call executes the central kernel transition.
// Preconditions are checked in order per §5.1 of the requirements.
// For root calls (req.ExistingTraceID), the process and trace must already have been created by Run().
// For subcalls, the parent trace must have sufficient available funds.
// For step-completion calls, BeginStepCall must have been called before invoking Call.
// SubcallRequest is the only publicly constructible call: a subcall on an existing trace (§6), the
// HTTP twin of juice.call used by a capability callback (§9). It cannot express an orchestration
// mode — a pre-funded trace, a step completion, or an inbound idempotency record — so no client can
// assemble a combination the kernel does not itself create.
type SubcallRequest struct {
	CallerID      string // the executing action's owner, per the subcall law (§6)
	ParentTraceID string // the trace the subcall spends from
	ActionRef     string // owner/name, owner@kernel/name, or a raw action id
	Args          map[string]any
}

// Subcall executes a subcall on an existing trace. Root calls go through Run, step completions
// through CompleteStep, inbound federation through RunFederated — each supplying its own
// orchestration mode internally.
func (k *Kernel) Subcall(ctx context.Context, req SubcallRequest) (*CallReply, error) {
	return k.call(ctx, callRequest{
		CallerID:      req.CallerID,
		ParentTraceID: req.ParentTraceID,
		ActionRef:     req.ActionRef,
		Args:          req.Args,
	})
}

func (k *Kernel) call(ctx context.Context, req callRequest) (*CallReply, error) {
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
	process, err := k.readOpenProcess(ctx, processID)
	if err != nil {
		return nil, err
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
	var target *Account
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

	// 5 + 6. Liveness, visibility, and input-schema validation, enforced for every path (root,
	// step, subcall). The pre-resolved snapshot (req.Action) is validated, so root calls are
	// checked here too with no extra DB read and no TOCTOU window — Call is the single validity
	// function; no entry path bypasses it (beginRun runs the same check before funding). A step
	// completion (req.StepID != "") bound visibility at creation (§10), so it skips that check.
	if err := k.checkCallPreconditions(ctx, caller, process.OwnerUserID, action, req.Args, req.StepID == "", ""); err != nil {
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

	// For remote_proxy: action.Price = q = proxyPrice (mp + import duty), set at import (§8 resolve).
	// Derive the original remote manifest price (mp) from q for clamping and receipt audit.
	// lockPrice = q funds the EXECUTION channel from the parent trace; the value channel is a separate
	// TransferEffect reserve locked from the immediate caller C's own balance in BeginSubcall (§13), so
	// a composed transfer pays the value from the composing action owner, not the process budget. (For a
	// root/step call the ExistingTraceID branch below discards this and adopts beginRun's snapshot.)
	lockPrice := action.Price
	var mp int64 = action.Price // for non-remote-proxy: mp unused; for remote-proxy: corrected below
	eff, verr := k.prepareTransferEffect(ctx, false, action, req.Args)
	if verr != nil {
		return nil, verr
	}
	if eff != nil {
		trace.Value = eff.Amount
		trace.ValueTo = eff.Dest
		trace.ValueReserve = eff.Reserve
	}
	if action.Kind == KindRemoteProxy {
		// The seller's own price, kept on the row since it was resolved — never reverse-calculated
		// from the rounded local total, which cannot recover it exactly (§16).
		mp = actionBasePrice(action)
		var value int64
		if eff != nil {
			value = eff.Amount
		}
		key := uuid.New().String()
		trace.IdempotencyKey = &key
		trace.DispatchJSON = marshalDispatch(req.Args, req.StepID, mp, value, lockPrice, action.ArtifactHash, actionRemoteBPS(action), k.cfg.ImportBPS)
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

	// fail is the single settled-failure exit (§5): stamp the failure, commit the transaction +
	// receipt + refund through settleFailedCall, and report the committed transaction alongside the
	// cause — the caller was charged, so it must be able to find it. Every failure path below goes
	// through here, so the invariant has one implementation; only the cause and latency vary.
	// A settlement that itself fails returns its own error, since then nothing was committed.
	fail := func(cause error, latency float64) (*CallReply, error) {
		ktx.Status = TxFailure
		if ktx.EndedAt.IsZero() {
			ktx.EndedAt = time.Now().UTC()
		}
		receipt, sErr := k.settleFailedCall(ctx, logger, ktx, trace, callerWalletID, callerWalletKind, req, action, latency, cause)
		if sErr != nil || receipt == nil {
			if sErr == nil {
				sErr = cause
			}
			return nil, sErr
		}
		return &CallReply{TxID: ktx.ID, TraceID: trace.ID, ReceiptID: receipt.ID}, cause
	}

	// 9. Execute. Remote proxy calls use ExecuteFederation directly with the stored idempotency key,
	// dispatching the peer's stable action id — never the cached display name (§13).
	started := time.Now()

	if action.Kind == KindRemoteProxy {
		fe := k.fedClient
		if fe == nil {
			// The trace is already funded (BeginSubcall / BeginRun). Returning here without
			// settling would commit no transaction and strand the locked allocation. Route the
			// misconfiguration through the normal failure path so a failure tx + receipt commits
			// and the funds refund — the "no settlement" rule is only for a network timeout
			// awaiting a remote receipt, not a local adapter being absent.
			return fail(ErrInvalidState.Wrap("federation executor not configured"), 0)
		}
		ikey := ""
		if trace.IdempotencyKey != nil {
			ikey = *trace.IdempotencyKey
		}
		fr, _ := fe.ExecuteFederation(ctx, target.KernelPublicKey, action.RemoteActionID, action.ArtifactHash, ikey, req.Args)
		latency := time.Since(started).Seconds()
		ktx.EndedAt = time.Now().UTC()
		if fr.NotDispatched {
			// First dispatch, provably never sent (§13 never-dispatched): settle as an ordinary
			// local failure with a full refund now, rather than parking the allocation for a retry
			// that would repeat a request the peer never received. Only here — retryRemoteTrace
			// never fail-fasts, since a parked request may already have executed. Mirrors the
			// executor-not-configured settlement above (a funded trace must never be stranded).
			pn := k.KernelName(ctx, target.KernelPublicKey)
			logger.Warn("remote.unreachable", "action", action.Name, "peer", target.Handle)
			return fail(ErrPeerUnreachable.Wrapf("peer %s is unreachable; the call was not sent and has been refunded", pn).WithMeta("peer", pn), latency)
		}
		return k.settleRemoteCall(ctx, logger, action, ktx, trace, callerWalletID, callerWalletKind, req, target, mp, fr, latency)
	}

	reply, _, execErr := k.execute(ctx, action, req.Args, trace, action.OwnerUserID, req.CallerID, process.OwnerUserID)
	latency := time.Since(started).Seconds()
	ktx.EndedAt = time.Now().UTC()

	if execErr != nil {
		logger.Warn("call.failed", "action", action.Name, "error", execErr)
		return fail(execErr, latency)
	}

	// 10. Validate output schema.
	if schemaErr := ValidateInput(action.OutputSchema, any(reply)); schemaErr != nil {
		return fail(schemaErr, latency)
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
		// If we can't read the trace, settle as failure to avoid fund loss. The settlement commits a
		// transaction and charges for it, so its receipt is reported like every other settled failure:
		// a nil reply would tell the caller nothing happened while it has in fact been charged, and
		// the idempotency record already completed.
		reply, _ := fail(ErrInternal.Wrap("could not read trace"), latency)
		return reply, ErrInternal.Wrap("could not read trace")
	}
	taxable := postTrace.Available
	net, fee := ComputeFee(taxable, k.cfg.FeeBPS)

	replyJSON, _ := json.Marshal(reply)
	ktx.ReplyJSON = json.RawMessage(replyJSON)
	ktx.Status = TxSuccess
	ktx.Net = net
	ktx.Fee = fee
	stats := k.computeStats(ctx, action.ID, ktx, latency)
	// Two independent channels (§13). The EXECUTION premium is levied on the charge (= gross) at the
	// rate snapshotted on the trace, and released from premium_parked at settlement. The VALUE premium
	// is the serving markup baked into the value reserve at admission — value_reserve − value — so the
	// receipt's value_premium is exactly what commitTraceTransferEffect settles to sys, for every
	// caller (a local caller reserves exactly value ⇒ 0; a peer/step completer reserves value+markup).
	// Both computed separately; never on charge+value (the rounding-merge is the bug).
	premium := ceilDiv(ktx.Gross*trace.PremiumBPS, 10000)
	var valuePremium int64
	if trace.ValueTo != "" && trace.ValueReserve > trace.Value {
		valuePremium = trace.ValueReserve - trace.Value
	}
	receipt, receiptErr := k.buildReceipt(ktx, ktx.Gross, premium, trace.Value, valuePremium, trace.ValueTo) // success: charge = gross, value delivered
	if receiptErr != nil {
		mu.Unlock()
		// Same as the post-execution read failure above: the settlement committed, so its receipt is
		// reported rather than discarded.
		reply, _ := fail(ErrInternal.Wrap("could not build receipt"), latency)
		return reply, ErrInternal.Wrap("could not build receipt")
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
func (k *Kernel) callerWallet(req callRequest, process *Process, parentTrace *Trace) (id, kind string) {
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
	// The serving-markup and value-transfer snapshots (§13) ride on the funded root trace; carry them
	// into the adopted trace so the receipt levies the correct premium/value and the reserve is
	// released to the beneficiary and sys at settlement.
	trace.PremiumBPS = dbTrace.PremiumBPS
	trace.PremiumParked = dbTrace.PremiumParked
	trace.Value = dbTrace.Value
	trace.ValueTo = dbTrace.ValueTo
	trace.ValueReserve = dbTrace.ValueReserve
	return dbTrace.Available
}

// canCall returns true iff the action is callable by the immediate caller (§4). Visibility is
// scoped to the caller, not the process owner, so a provider's public action may subcall the
// provider's own private helpers in anyone's process, while foreign code funded by a process owner
// cannot reach that owner's private actions.
// CanCall(C, a) := active(a) ∧ ¬suspended(a.owner) ∧
//
//	(public(a) ∨ (local(a) ∧ ¬IsPeer(C)) ∨ C = a.OwnerUserID)
func canCall(caller *Account, action *Action) bool {
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

// checkCallPreconditions enforces the §4 semantic call-validity rules (steps 6 to 8) for a
// resolved action: liveness, visibility by the immediate caller, the optional quote pin, and input
// against the action's schema. It is the single validity function — Call runs it unconditionally
// for every entry path, and beginRun runs it once before funding so an invalid root call never
// creates a funded process (§6). checkVisibility is false only for a step completion, which bound
// visibility at creation (§10): a liveness failure still resets it to waiting, a later visibility
// change does not. The grant check stays keyed on the process owner: delegated consent binds to the
// paying human (§8). quoteHash is empty on every path but a pinned root run.
func (k *Kernel) checkCallPreconditions(ctx context.Context, caller *Account, processOwnerID string, action *Action, args map[string]any, checkVisibility bool, quoteHash string) error {
	if !action.Active {
		return ErrInvalidState.Wrap("action is inactive")
	}
	if action.OwnerSuspended {
		return ErrInvalidState.Wrap("action owner is suspended")
	}
	if checkVisibility && !canCall(caller, action) {
		return ErrUnauthorized.Wrap("call permission denied")
	}
	// After visibility, so a mismatch never discloses a private action's terms; before input
	// validation, so terms that changed enough to invalidate the args still report as changed terms
	// rather than a schema violation (§4 precondition 7). Guarded rather than computed in the `if`
	// initializer: this runs on every call, subcall and step completion, and hashing two schemas
	// for a pin nobody supplied is pure waste.
	if quoteHash != "" {
		if cur := QuoteHash(action); quoteHash != cur {
			return TermsChangedError(cur, action.Price)
		}
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
	reply, err := h.kernel.call(ctx, callRequest{
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
	callerID, remoteID, err := h.kernel.ResolveRequiredCaller(ctx, requiredCaller)
	if err != nil {
		return "", err
	}
	step, err := h.kernel.CreateStep(ctx, h.traceID, act.ID,
		json.RawMessage(partialArgs), callerID, remoteID)
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
func (k *Kernel) settleFailedCall(ctx context.Context, logger *log.Logger, tx *Transaction, trace *Trace, callerWalletID, callerWalletKind string, req callRequest, action *Action, latency float64, callErr error) (*Receipt, error) {
	traceID := trace.ID
	// Settlement is a money transition + its audit record (§5); it must commit even
	// if the call timed out or the client disconnected. Detach from execution-scoped
	// cancellation so a cancelled/contended ctx can never strand the locked allocation.
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	ctx = sctx
	if len(tx.ReplyJSON) == 0 {
		tx.ReplyJSON = json.RawMessage("null")
	}
	// The reason is the failure CLASS (§6): a code, never an adapter's prose, because it is signed
	// into the receipt and crosses the kernel boundary. A mandated literal (`interrupted`, §5) is
	// assigned first. Set before CommitFailedCall so the receipt matches the transaction.
	if tx.Reason == "" {
		tx.Reason = KernelErrorCode(callErr)
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
	// Serving-markup premium (§13): the rate is snapshotted on the trace, so premium — levied on the
	// actual failed charge (gross−refund), computed inside the receipt closure so the signed number and
	// the committed legs cannot diverge — is available on EVERY failure path (execution, recovery,
	// forced closure, max-age expiry), not only those with the in-memory request. The parked reserve is
	// released inside CommitFailedCall from the trace snapshot. Both 0 for local calls.
	var committed *Receipt
	buildFn := func(refund int64) (*Receipt, error) {
		charge := tx.Gross - refund
		// value delivery is all-or-nothing (§13): a failed transfer delivers nothing, so value/value_premium
		// are 0 and refundTransferEffect returns the whole value reserve to the caller C.
		r, err := k.buildReceipt(tx, charge, ceilDiv(charge*trace.PremiumBPS, 10000), 0, 0, "")
		committed = r
		return r, err
	}
	if settlErr := k.store.CommitFailedCall(ctx, tx, buildFn, traceID, callerWalletID, callerWalletKind, k.cfg.FeeRecipientID, tx.Gross, stats, req.IdempotencyRecordID, KernelErrorCode(callErr), req.StepID); settlErr != nil {
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
