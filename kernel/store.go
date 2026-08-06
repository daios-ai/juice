package kernel

import (
	"context"
	"time"
)

// ---- Script execution interfaces ----

// SourceCompiler compiles TinyGo source bytes to WASM.
// kernel/ defines this interface; script/ provides the TinyGo implementation.
// Returns ErrInvalidInput on compile failure; ErrInvalidState when tinygo binary absent.
type SourceCompiler interface {
	CompileSource(ctx context.Context, source []byte) (artifact []byte, hash string, err error)
}

// WASMImport describes one function import inside a WASM binary.
type WASMImport struct {
	Module string
	Name   string
}

// WASMInspector enumerates a WASM binary's imports and exports without executing it.
// ScriptExecutor implementations may optionally implement this; kernel checks via type assertion.
type WASMInspector interface {
	InspectWASM(artifact []byte) (imports []WASMImport, exports []string, err error)
}

// ScriptExecutor compiles and runs WebAssembly scripts.
// kernel/ defines this interface; script/ provides the wazero implementation.
type ScriptExecutor interface {
	Compile(ctx context.Context, source []byte) (artifact []byte, hash string, err error)
	Execute(ctx context.Context, artifact []byte, input []byte, host HostFunctions) ([]byte, error)
}

// HTTPExecutor calls an external HTTP action endpoint. ownerUserID is the process owner (the
// payer): the executor needs it to resolve a delegated OAuth grant (§8), whose binding rule is
// grant.grantor_user_id == ownerUserID. capability is the trace-scoped composition token (§9)
// the executor delivers as a header so the endpoint can call back; empty disables composition.
// kernel/ defines this interface; cmd/juice implements it.
type HTTPExecutor interface {
	Execute(ctx context.Context, action *Action, args map[string]any, ownerUserID, capability string) (map[string]any, error)
}

// GrantStore is the executor-facing subset of Store for the delegated-token lifecycle (§8).
// The concrete store implements it; cmd/juice injects it into the HTTP executor so token
// exchange (which lives outside kernel/) can resolve a grant to its Connection, persist a
// rotated refresh token on that connection, and cascade-delete it on invalid_grant.
type GrantStore interface {
	ReadGrant(ctx context.Context, grantorUserID, actionID string) (*Grant, error)
	ReadConnection(ctx context.Context, id string) (*Connection, error)
	UpdateConnectionSecret(ctx context.Context, id, sealedSecret string) error
	DeleteConnectionCascade(ctx context.Context, id string) error
}

// URLFetcher retrieves the body of a URL. Used for OpenAPI ownership proof (well-known challenge).
// HTTPExecutor implementations may optionally implement this interface; kernel checks via type assertion.
type URLFetcher interface {
	FetchURL(ctx context.Context, rawURL string) ([]byte, error)
}

// FederationExecutor sends a cross-kernel call to a remote proxy target over the federation
// transport (§13), addressing the peer by its Ed25519 public key. actionID is the action's stable
// id on that peer; expectedContractHash is the cached contract hash the call
// binds as the §8 If-Match precondition. The transport signs the request as this kernel and
// resolves peerPublicKey to a live path (direct / hole-punched / relayed). HTTPExecutor
// implementations may optionally implement this interface; kernel checks via type assertion.
type FederationExecutor interface {
	ExecuteFederation(ctx context.Context, peerPublicKey, actionID, expectedContractHash, idempotencyKey string, args map[string]any) (FederationResult, error)
}

// FederationSettler runs one round of the /juice/fed/settle/1 residual-settlement exchange against a
// peer (§13), addressing it by Ed25519 public key. Like FederationExecutor it is an optional
// capability of the injected HTTPExecutor; the kernel checks via type assertion and never imports fed.
// It returns the peer's raw response body (a signed SettlementRecord) and status.
type FederationSettler interface {
	Settle(ctx context.Context, peerPublicKey, kind, timestamp, signature, settlementID string, amount int64, nonce string, record []byte) (status int, body []byte, err error)
}

// RemoteResolver resolves a single remote action or user on demand over /juice/fed/resolve/1
// (§13 subscription-free calls). Like FederationExecutor it is an optional capability the injected
// HTTPExecutor may implement; the kernel checks via type assertion and never imports fed.
// ResolveRemoteAction returns the peer's signed manifest for one action; ResolveRemoteUser maps a
// user reference to its stable id and handle on the peer. A nil/absent resolver disables lazy
// resolution (a cold cross-kernel ref is then a plain ErrNotFound).
type RemoteResolver interface {
	ResolveRemoteAction(ctx context.Context, peerPublicKey, owner, name string) (*ActionManifest, error)
	ResolveRemoteUser(ctx context.Context, peerPublicKey, ref string) (userID, handle string, err error)
}

// HostFunctions are the callbacks available to a running script.
type HostFunctions interface {
	Call(ctx context.Context, actionName string, args []byte) ([]byte, error)
	StepCreate(ctx context.Context, partialArgs []byte, requiredCallerUserID, actionID string) (string, error)
	StepComplete(ctx context.Context, stepID string, input []byte) ([]byte, error)
	Log(ctx context.Context, level, msg string) error
}

// ---- LLM interfaces ----

// ChatMessage is a single turn in a conversation.
type ChatMessage struct {
	Role    string
	Content string
}

// Embedder produces a dense vector representation of a text string.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Chatter performs a chat completion given a message history.
type Chatter interface {
	Chat(ctx context.Context, messages []ChatMessage) (ChatMessage, error)
}

// JSONChatter performs a chat completion constrained to a JSON schema.
type JSONChatter interface {
	ChatJSON(ctx context.Context, messages []ChatMessage, schema map[string]any) (any, error)
}

// ToolDefinition describes a Juice action offered to the LLM as a callable tool.
type ToolDefinition struct {
	Action       string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any // optional
	Price        *int64         // optional, informational
}

// ToolCall is a single tool invocation proposed by the LLM.
type ToolCall struct {
	Action string
	Args   map[string]any
}

// DecideTool holds the tool action and data for a single turn.
// On assistant turns it carries the proposed Action and Args.
// On tool turns it carries the Action and Result.
type DecideTool struct {
	Action string
	Args   map[string]any
	Result map[string]any
}

// DecideMessage is a single turn in a decide conversation.
// Role is "system", "user", "assistant", or "tool".
// Tool is non-nil on assistant proposal turns and tool result turns.
type DecideMessage struct {
	Role    string
	Content string
	Tool    *DecideTool
}

// DecideChatter asks the LLM to select a Juice action from a set of candidates.
type DecideChatter interface {
	ChatDecide(ctx context.Context, messages []DecideMessage, tools []ToolDefinition) (*ToolCall, *ChatMessage, error)
}

// ---- Persistence interface ----

// TxFilter narrows a ListTransactions query.
type TxFilter struct {
	OwnerUserID  string
	CallerUserID string
	TargetUserID string
	ProcessID    string
	// PartyUserID matches transactions where the user is any party:
	// process owner (owner_user_id), call caller (caller_user_id), or action owner (target_user_id).
	PartyUserID string
	Limit       int
	Offset      int
}

// Store is the persistence interface for all kernel objects.
// All monetary transitions are executed atomically inside the store.
// kernel/ must not import any concrete store implementation.
//
// The compound methods (StartProcess, BeginCall, CommitCall, CommitFailedCall) are the
// supported realization of the spec's atomic write sets; they enforce atomicity that
// individual primitive calls cannot. There is no parallel primitive-transaction API.
type Store interface {
	// ---- Accounts ----

	CreateUser(ctx context.Context, u *Account) error
	ReadUser(ctx context.Context, id string) (*Account, error)
	// ReadUserByHandle resolves the user namespace. A kernel account holds no handle, so it is
	// unreachable here by construction (§13).
	ReadUserByHandle(ctx context.Context, handle string) (*Account, error)
	// ReadAccountByKernelKey returns the account settling for a remote kernel, or ErrNotFound.
	ReadAccountByKernelKey(ctx context.Context, publicKey string) (*Account, error)
	// ListUsers returns local user accounts; kernel accounts are excluded (§14).
	ListUsers(ctx context.Context, limit, offset int) ([]*Account, error)
	SuspendUser(ctx context.Context, id string) error
	UnsuspendUser(ctx context.Context, id string) error
	UpdateUser(ctx context.Context, u *Account) error
	RenameUser(ctx context.Context, id, handle string) error

	// ---- Actions ----

	CreateAction(ctx context.Context, a *Action) error
	ReadAction(ctx context.Context, id string) (*Action, error)
	ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*Action, error)
	ReadActionByOwnerRemoteID(ctx context.Context, ownerID, remoteActionID string) (*Action, error)
	// ListActionsByOwnerOpenAPISpec returns all non-deleted actions with matching owner + OpenAPI spec_url.
	ListActionsByOwnerOpenAPISpec(ctx context.Context, ownerID, specURL string) ([]*Action, error)
	UpdateAction(ctx context.Context, a *Action) error
	// UpdateActionAndResetStats atomically updates the action record and zeros its stats row.
	// Used during import reconciliation to ensure contract changes and stat resets are coherent.
	UpdateActionAndResetStats(ctx context.Context, a *Action) error
	DeleteAction(ctx context.Context, id string) error
	// ListVisibleActions returns active non-deleted actions with a non-suspended owner, network-wide
	// (visibility=public) and, when includeLocal is set, also kernel-local ones (§4/§14).
	ListVisibleActions(ctx context.Context, includeLocal bool, limit, offset int) ([]*Action, error)
	// ListActionsByOwner returns all non-deleted actions owned by ownerID, including
	// inactive and private ones. Used to give an owner their full private view.
	ListActionsByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*Action, error)
	ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error)
	// ListNativeActions returns all non-deleted kind=native actions (the platform stdlib rows).
	// Used at startup to prune natives whose handler the build no longer registers.
	ListNativeActions(ctx context.Context) ([]*Action, error)

	// ---- Processes ----

	// BeginRun atomically debits W = price + premiumReserve from owner.available→locked, creates
	// the process with available=0/locked=price, and creates the root trace with available=price;
	// the extra premiumReserve stays parked in owner.locked for the serving markup, released at
	// settlement (§13). Admission (§13): an ordinary owner (no public_key) must have available ≥ W;
	// a peer owner is bounded not per-row but globally — the projected global gross receivables
	// G = Σ_peers max(0,−available), with this peer's debt replaced by its post-debit value, must
	// stay ≤ exposureMax (Sybil-proof: one cap across all peer identities). exposureMax is ignored
	// for ordinary owners and when W = 0 (a free call adds no exposure). All non-exposure
	// precondition checks must happen in Go before calling BeginRun.
	BeginRun(ctx context.Context, p *Process, t *Trace, ownerID string, price, premiumReserve, exposureMax int64) error

	ReadProcess(ctx context.Context, id string) (*Process, error)
	ListProcesses(ctx context.Context, ownerID string, limit, offset int) ([]*Process, error)
	ListAllProcesses(ctx context.Context, limit, offset int) ([]*Process, error)

	// BeginSubcall atomically deducts price from the parent trace's available into its locked
	// and creates the child trace with available=price.
	BeginSubcall(ctx context.Context, parentTraceID string, t *Trace, price int64) error

	// BeginStepCall atomically moves step.price from the step's parent_trace.locked back into
	// parent_trace.available (the step is being consumed), creates the new trace with
	// available=step.price, and transitions the step waiting→running.
	BeginStepCall(ctx context.Context, stepID string, t *Trace, exposureMax int64) error

	// CommitCall atomically records a successful transaction, creates its receipt,
	// settles funds (trace.available→target/sys; caller wallet locked released;
	// owner.locked decremented by taxable), updates trace latency, upserts action stats,
	// completes the idempotency record (if non-empty), and marks the step done (if non-empty).
	// The serving-markup reserve parked in the owner's locked at admission (§13) is released here from
	// the trace's premium_parked snapshot — receipt.Premium to feeRecipientID's sys, the remainder back
	// to the owner (0 for every local call/subcall, so the premium legs are a no-op).
	CommitCall(ctx context.Context, tx *Transaction, receipt *Receipt, traceID, callerWalletID, callerWalletKind, targetUserID, feeRecipientID string, net, fee int64, stats *Stats, idempotencyRecordID, stepID string) error

	// CommitFailedCall atomically cancels all outstanding steps in the trace's subtree
	// (collecting their parked prices), refunds trace.available + step prices to the caller
	// wallet, records a failure transaction, creates its receipt, updates trace latency,
	// upserts action stats, completes the idempotency record (if non-empty), and marks the
	// step done (if non-empty). The process owner's user.locked is decremented at process
	// closure — closeProcessTx runs inside the same DB transaction when the process becomes
	// quiescent. Implementations must NOT decrement user.locked directly here; doing so
	// would double-count with the closure step.
	// buildReceipt is called inside the transaction with the computed refund so that the
	// signed charge (gross − refund) is guaranteed to match what is committed. It also fixes
	// receipt.Premium, so the serving-markup legs (the trace's parked reserve released to
	// feeRecipientID's sys and the owner) cannot diverge from the signed number.
	CommitFailedCall(ctx context.Context, tx *Transaction, buildReceipt func(refund int64) (*Receipt, error), traceID, callerWalletID, callerWalletKind, feeRecipientID string, gross int64, stats *Stats, idempotencyRecordID, errorCode, stepID string) error

	// EndProcess cancels all waiting steps (returning parked prices to the process owner's
	// available balance), then returns process.available to the owner, and closes the process.
	EndProcess(ctx context.Context, processID string) error

	// ---- Traces ----

	ReadTrace(ctx context.Context, id string) (*Trace, error)
	// ReadRootTrace returns the root trace (ParentTraceID IS NULL) for the given process.
	ReadRootTrace(ctx context.Context, processID string) (*Trace, error)
	// TraceHasTransaction reports whether the trace has settled, i.e. a transaction row exists
	// for it (the settled-once predicate; §9 capability validity, §11 unique trace transaction).
	TraceHasTransaction(ctx context.Context, traceID string) (bool, error)

	// ---- Transactions ----

	ReadTransaction(ctx context.Context, id string) (*Transaction, error)
	ListTransactions(ctx context.Context, filter TxFilter) ([]*Transaction, error)
	ListAllTransactions(ctx context.Context, limit, offset int) ([]*Transaction, error)

	// ---- Receipts ----

	ReadReceiptByTxID(ctx context.Context, txID string) (*Receipt, error)

	// ---- Ratings ----

	ReadRatingByTxID(ctx context.Context, txID string) (*Rating, error)
	// CreateRatingAndUpdateStats atomically inserts a rating and updates rating_count/rating_estimate.
	CreateRatingAndUpdateStats(ctx context.Context, r *Rating, actionID string, rating float64) error
	// ListRatings returns ratings for a given action ordered by created_at DESC.
	ListRatings(ctx context.Context, actionID string, limit, offset int) ([]*Rating, error)

	// ---- Idempotency ----

	// ReadIdempotencyRecord returns an unexpired record matching key + counterparty, or ErrNotFound.
	ReadIdempotencyRecord(ctx context.Context, key, counterpartyUserID string) (*IdempotencyRecord, error)
	// InsertPendingIdempotencyRecord inserts a record with status="pending". Returns a unique-constraint
	// error (not ErrNotFound) if a record for the same key+counterparty already exists.
	InsertPendingIdempotencyRecord(ctx context.Context, r *IdempotencyRecord) error
	// DeleteIdempotencyRecord removes a record (used to allow retry after execution failure).
	DeleteIdempotencyRecord(ctx context.Context, id string) error
	// CompleteIdempotencyRecordIfPending atomically transitions a record from pending to
	// complete, storing resultJSON and receiptJSON. If the record is already complete
	// (e.g. CommitFailedCall already ran), this is a no-op.
	CompleteIdempotencyRecordIfPending(ctx context.Context, id, resultJSON, receiptJSON string) error

	// ---- Recovery challenges (§12) ----

	// CreateRecoveryChallenge stores a single-use, TTL-bound nonce for a password-recovery attempt.
	CreateRecoveryChallenge(ctx context.Context, nonce, userID string, expiresAt time.Time) error
	// ConsumeRecoveryChallenge atomically deletes an unexpired nonce and returns its user_id;
	// single-use, so a replay returns ErrNotFound.
	ConsumeRecoveryChallenge(ctx context.Context, nonce string) (string, error)

	// ---- Stats ----

	ReadStats(ctx context.Context, actionID string) (*Stats, error)
	UpsertStats(ctx context.Context, s *Stats) error

	// ---- Steps ----

	// CreateStep atomically inserts the step and parks step.price from the parent trace's
	// available into its locked. Returns ErrInsufficientFunds if parent_trace.available < price.
	CreateStep(ctx context.Context, s *Step) error
	ReadStep(ctx context.Context, id string) (*Step, error)
	// ListSteps returns steps visible to caller. processID and status are optional filters ("" = no filter).
	ListSteps(ctx context.Context, callerUserID, processID, status string, isSuperuser bool, limit, offset int) ([]*Step, error)
	ListStepsAwaitingCaller(ctx context.Context, requiredCallerUserID string, limit int) ([]*Step, error)
	// ResetStepAndRepark re-parks a step's price and resets to waiting. Used when the
	// completion trace is empty (crash during execution) to prevent double-completion minting.
	ResetStepAndRepark(ctx context.Context, stepID string) error
	// ListOrphanRunningSteps returns running steps that have a completion trace but no tx,
	// with enough detail to decide between re-parking (empty trace) or settling as failed.
	// HasSettled is true when the completion trace has locked funds or committed subcall transactions.
	ListOrphanRunningSteps(ctx context.Context) ([]OrphanRunningStep, error)
	// ListOrphanRunningStepsForProcess is ListOrphanRunningSteps scoped to one process.
	// Used by EndProcess to fail in-flight step completions as failed calls before closure.
	ListOrphanRunningStepsForProcess(ctx context.Context, processID string) ([]OrphanRunningStep, error)
	// ResetRunningSteps sets status=waiting where status=running AND tx_id IS NULL.
	ResetRunningSteps(ctx context.Context) error
	// ListOrphanTraces returns traces that have no associated transaction and no idempotency_key,
	// ordered deepest-first (longest parent chain first). Used by recovery to settle interrupted calls.
	// Traces with idempotency_key are pending remote dispatches handled by RetryPendingRemoteDispatches.
	ListOrphanTraces(ctx context.Context) ([]*Trace, error)

	// ListPendingRemoteTraces returns traces with an idempotency_key but no committed transaction.
	// These are in-flight remote proxy calls awaiting settlement by RetryPendingRemoteDispatches.
	ListPendingRemoteTraces(ctx context.Context) ([]*Trace, error)

	// ListDirectUnsettledChildren returns traces whose parent_trace_id equals parentTraceID
	// and that have no committed transaction. Used by settleFailedCall to pre-settle child
	// traces (e.g. timed-out remote subcalls) before committing the parent failure, so that
	// the parent's refund correctly includes the child's allocation.
	ListDirectUnsettledChildren(ctx context.Context, parentTraceID string) ([]*Trace, error)

	// ListUnsettledTracesForProcess returns all traces for a process that have no committed
	// transaction, ordered deepest-first. Includes both orphan and pending-remote traces.
	// Used by EndProcess to fail in-flight calls before closure.
	ListUnsettledTracesForProcess(ctx context.Context, processID string) ([]*Trace, error)

	// CommitRemoteSettlement atomically settles an outbound remote-proxy call (§13):
	// releases the gross lock q from the caller wallet, pays paid→proxyUserID (the bilateral payable
	// to the peer, = charge + the peer's serving premium) and importFee→feeRecipientID (the origin's
	// locally-retained import fee), returns the refund (q−paid−importFee) to the caller wallet,
	// decrements owner.locked by taxable (paid+importFee), records the transaction+receipt, updates
	// stats, marks step done (if stepID non-empty), completes the idempotency record (if non-empty),
	// and closes the process if quiescent.
	CommitRemoteSettlement(ctx context.Context, tx *Transaction, receipt *Receipt, traceID, callerWalletID, callerWalletKind, proxyUserID, feeRecipientID string, paid, importFee int64, vs ValueSettlement, stats *Stats, idempotencyRecordID, stepID, errorCode string) error

	// Remote payment-step reserve (buyer side, §13): the buyer funds a TransferEffect attached to a
	// Step hosted on another kernel via a dedicated pending_transfers record, not a fabricated trace.
	// InsertPendingTransfer locks max_total from the buyer reserve-first and records the row atomically.
	// CommitPendingTransfer settles it on the serving kernel's valid success receipt (value+value_premium
	// → peer proxy row, value_import → buyer sys, remainder refunded). RefundPendingTransfer returns the
	// whole reserve (valid failure/never-dispatched). SetPendingTransferStatus quarantines WITHOUT
	// touching balances (invalid receipt: reserve stays locked), recording a reason. ReadPendingTransferByKey
	// is the idempotency/retry lookup; ReadPendingTransfer/ListPendingTransfers back the operator surface
	// (an empty status lists only unresolved records — pending + quarantined).
	InsertPendingTransfer(ctx context.Context, pt *PendingTransfer) error
	ReadPendingTransferByKey(ctx context.Context, idempotencyKey string) (*PendingTransfer, error)
	ReadPendingTransfer(ctx context.Context, id string) (*PendingTransfer, error)
	ListPendingTransfers(ctx context.Context, status string, limit, offset int) ([]*PendingTransfer, error)
	CommitPendingTransfer(ctx context.Context, id, proxyRowID string, credit int64, sysID string, sysCredit int64) error
	RefundPendingTransfer(ctx context.Context, id string) error
	SetPendingTransferStatus(ctx context.Context, id, status, reason string) error

	// ---- Traces (by process) ----

	ListTraces(ctx context.Context, processID string) ([]*Trace, error)

	// ---- Auth codes (PKCE flow) ----

	CreateAuthCode(ctx context.Context, c *AuthCode) error
	ConsumeAuthCode(ctx context.Context, code string) (*AuthCode, error)

	// ---- Refresh tokens ----

	CreateRefreshToken(ctx context.Context, t *RefreshToken) error
	RotateRefreshToken(ctx context.Context, oldToken string) (*RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, token string) error

	// ---- Grants (delegated upstream OAuth, §8) ----

	// CreateOrReplaceGrant upserts on (grantor_user_id, action_id); re-consent overwrites.
	CreateOrReplaceGrant(ctx context.Context, g *Grant) error
	// ReadGrant returns the grant for (grantor, action), or ErrNotFound.
	ReadGrant(ctx context.Context, grantorUserID, actionID string) (*Grant, error)
	ListGrantsByUser(ctx context.Context, grantorUserID string) ([]*Grant, error)
	// DeleteGrant removes one grant (revoke / invalid_grant); ErrNotFound if absent.
	DeleteGrant(ctx context.Context, grantorUserID, actionID string) error
	// DeleteGrantsForAction removes every grant on an action (deactivating update / delete).
	DeleteGrantsForAction(ctx context.Context, actionID string) error

	// ---- Connections (shared upstream credential, §8) ----

	// CreateOrUpdateConnection upserts on (user_id, provider_key); a conflict keeps the
	// existing id and created_at and refreshes secret/scopes/updated_at.
	CreateOrUpdateConnection(ctx context.Context, c *Connection) error
	// ReadConnection returns the connection by id, or ErrNotFound.
	ReadConnection(ctx context.Context, id string) (*Connection, error)
	// ReadConnectionByUserProvider returns a user's connection for a provider_key, or ErrNotFound.
	ReadConnectionByUserProvider(ctx context.Context, userID, providerKey string) (*Connection, error)
	ListConnectionsByUser(ctx context.Context, userID string) ([]*Connection, error)
	// UpdateConnectionSecret replaces the sealed secret (provider rotation).
	UpdateConnectionSecret(ctx context.Context, id, sealedSecret string) error
	// DeleteConnectionCascade removes a connection and all its grants atomically.
	DeleteConnectionCascade(ctx context.Context, id string) error
	// ListLegacyTokenGrants returns grants still holding a legacy token, oldest first (backfill).
	ListLegacyTokenGrants(ctx context.Context) ([]*Grant, error)
	// LinkGrantConnection points a grant at a connection and clears its legacy token (backfill).
	LinkGrantConnection(ctx context.Context, grantID, connectionID string) error

	// ---- Config ----

	GetConfig(ctx context.Context, key string) (string, error)
	SetConfig(ctx context.Context, key, value string) error

	// InitFirstBoot atomically creates a user and sets all given config entries.
	// If the user handle already exists the user INSERT is skipped; config entries are always set.
	InitFirstBoot(ctx context.Context, u *Account, configs map[string]string) error

	// ---- Receipts (by ID) ----

	// ReadReceipt returns the receipt with the given ID.
	ReadReceipt(ctx context.Context, id string) (*Receipt, error)

	// ---- Ledger ----

	// CreateLedgerEntry atomically debits e.FromUserID (when set) and credits e.ToUserID
	// (when set), recording the entry. The debit subtracts amount and returns
	// ErrInsufficientFunds if that user's available < amount; the credit adds it. When
	// e.ExternalKey is set and already present, the existing record is returned (loaded
	// into e) and no balance change is applied — the idempotency check runs before the
	// debit's available-balance guard.
	CreateLedgerEntry(ctx context.Context, e *LedgerEntry) error

	// ListLedgerByUser returns ledger entries where userID is the source or the
	// destination, most recent first, bounded by limit/offset.
	ListLedgerByUser(ctx context.Context, userID string, limit, offset int) ([]*LedgerEntry, error)

	// ---- Embeddings ----

	// UpsertEmbedding stores a pre-computed embedding vector for an action.
	UpsertEmbedding(ctx context.Context, actionID string, vec []float32) error
	// ListEmbeddings returns stored embedding vectors keyed by action ID, filtered to active,
	// non-deleted actions; visibility is enforced by the caller-scoped canCall post-filter in Lookup.
	ListEmbeddings(ctx context.Context) (map[string][]float32, error)
	// UpsertLookupText replaces an action's lexical-index text (§9 hybrid lookup).
	UpsertLookupText(ctx context.Context, actionID, text string) error
	// SearchActionsLexical returns up to limit active action IDs matching query, BM25-ranked best-first.
	SearchActionsLexical(ctx context.Context, query string, limit int) ([]string, error)

	// ---- Peer sync ----

	// UpdatePeerSync records a successful, authenticated peer gossip pull (§13 peer sync), keyed by
	// public key: last_seen=now and, when the peer reported one, peer_credit=credit (nil leaves the
	// prior value). Display-only cache; never a money path.
	UpdatePeerSync(ctx context.Context, publicKey string, lastSeen time.Time, credit *int64) error

	// CommitSettlement records one finish outcome atomically, keyed idempotently by settlementID (§13,
	// the external_key read-first short-circuit — anti-grinding). A "clear" outcome passes dClear=±d,
	// variance=∓d (internally conservative) and extinguishes the debt; a "pay" outcome passes
	// dClear=variance=0, leaving the debt on the row until the cash record. `debt` (>0) is the ledger
	// row amount. A replay returns the stored record via storedRecord; "" on first application.
	CommitSettlement(ctx context.Context, settlementID, rowUserID, sysID string, dClear, variance, debt int64, recordJSON string) (storedRecord string, err error)

	// CommitSettlementCash finalizes a paid probabilistic outcome (§13), keyed idempotently by
	// settlementID.cash: it clears the debt d on rowUserID, books the variance ±(Q−d) on sysID, and
	// records the external cash Q — the sole non-conservative settlement move (this is where cash
	// crosses the rail). A debtor with insufficient sys reserve trips the users CHECK and the tx rolls
	// back, leaving the settlement pending with no partial writes.
	CommitSettlementCash(ctx context.Context, settlementID, rowUserID, sysID string, dClear, variance, q int64, recordJSON string) (storedRecord string, err error)

	// ReadSettlementRecord returns the stored record for a settlement (by settlementID), or "" if none
	// exists yet — the creditor's idempotency/anti-grinding lookup before a finish flip.
	ReadSettlementRecord(ctx context.Context, settlementID string) (string, error)

	// HasPendingSettlement reports whether peerID has a paid probabilistic outcome awaiting its rail
	// record (§13): a "pay" settlement with no companion .cash finalization.
	HasPendingSettlement(ctx context.Context, peerID string) (bool, error)

	// GrossReceivables returns Σ over peer rows of max(0, −available): the kernel's total unsecured
	// receivables, compared against exposure_max/settlement_trigger for display (§13).
	GrossReceivables(ctx context.Context) (int64, error)

	// ---- Kernels (identity, naming, discovery) ----

	// UpsertKernel records an observation: nickname, about, timestamps. It never writes the petname
	// (assigned locally, only on our own outbound act) nor gossip_cursor/last_seen/peer_credit, each
	// of which advances only after its own work is verified and committed (§13).
	UpsertKernel(ctx context.Context, publicKey, nickname, about string, now time.Time) error
	// BindPetname assigns a kernel's local petname in one transaction, so concurrent first use
	// converges on one name (§13). exact=false preserves an existing petname and suffixes -2…-99 on
	// collision; exact=true is an operator bind and errors on an occupied name. Returns the binding.
	BindPetname(ctx context.Context, publicKey, desired string, exact bool) (string, error)
	// SuspendKernelAccount provisions (when absent) and suspends a kernel's account in one
	// transaction (§13), so an inbound signed call cannot slip between the two writes.
	SuspendKernelAccount(ctx context.Context, publicKey, newAccountID string, now time.Time) error
	// ListKernels returns the whole `admin peers` roster (§14) — every known kernel with its account
	// state when one exists — from one kernels LEFT JOIN accounts, excluding selfKey.
	ListKernels(ctx context.Context, selfKey string, includeSuspended bool, limit, offset int) ([]*RemoteKernelView, error)
	// ReadKernel returns the kernel row for a public key, or nil if unknown.
	ReadKernel(ctx context.Context, publicKey string) (*RemoteKernel, error)
	// ReadKernelByPetname resolves a bound petname to its kernel; nil when no kernel holds it.
	ReadKernelByPetname(ctx context.Context, petname string) (*RemoteKernel, error)
	// SetGossipCursor persists the evidence high-watermark, only after a page is verified and
	// committed (§13 peer sync).
	SetGossipCursor(ctx context.Context, publicKey, cursor string) error

	// ---- Discovery docs (regenerable lookup cache, §13) ----

	// ReplaceDiscoveryDocs replaces ALL discovery docs (and their FTS mirror rows) for one source
	// kernel with the supplied set, in one transaction. An empty set clears that kernel's docs.
	ReplaceDiscoveryDocs(ctx context.Context, kernelPublicKey string, docs []*DiscoveryDoc) error
	// ListDiscoveryDocs returns all discovery docs (with embeddings) for the lookup dense leg.
	ListDiscoveryDocs(ctx context.Context) ([]*DiscoveryDoc, error)
	// SearchDiscoveryLexical returns doc_keys ranked by BM25 for query, most relevant first.
	SearchDiscoveryLexical(ctx context.Context, query string, limit int) ([]string, error)

	// ---- Evidence cache (regenerable reputation cache, §13) ----

	// UpsertEvidence stores or merges one verified evidence row, enforcing the late-rating
	// transitions and the E-per-(issuer,subject_kernel,subject_action) cap (§13).
	UpsertEvidence(ctx context.Context, e *EvidenceRow) error
	// ListEvidenceBySubject returns evidence rows about a subject kernel, for inspect display.
	ListEvidenceBySubject(ctx context.Context, subjectKernelPublicKey string) ([]*EvidenceRow, error)
	// ListReceiptsForGossip returns one ordered page of this kernel's own gossip-eligible receipts
	// (with any joined rating and remote receipt) after the cursor, for the evidence sender (§13).
	ListReceiptsForGossip(ctx context.Context, cursor string, limit int) ([]*GossipReceiptRow, error)

	// ---- Peer lifecycle ----

	// ListPurgeablePeers returns the IDs of peer users (public_key set) that are idle past
	// cutoff at zero balance (§13 Retention): available=0, locked=0, last activity (max of
	// created_at, latest transaction naming them, latest deposit/withdrawal, latest gossip
	// mention) before cutoff, and no waiting/running step addressed to them or to their actions.
	ListPurgeablePeers(ctx context.Context, cutoff time.Time) ([]string, error)
	// PurgePeerCascade atomically deletes a purged peer's derived data — its proxy actions,
	// their stats, its steps, its discovered_kernels row, its discovery_docs, and its evidence
	// rows (as issuer and as subject) — and forgets the peer identity by clearing public_key on the
	// user row. The immutable transaction/receipt ledger is preserved (party ids carry no FK),
	// keeping local counterparties' credits reconstructible (§11); the anonymized user row stays as
	// a ledger anchor so old history remains legible.
	PurgePeerCascade(ctx context.Context, userID string) error
	// PurgeStaleDiscovery evicts the regenerable discovery cache of kernels learned only from the
	// directory — a discovered_kernels row (with its discovery_docs, FTS mirror, and evidence) whose
	// updated_at is at or before cutoff and which is NOT backed by a peer user row (§13 Retention).
	// Peer-backed kernels are governed by PurgePeerCascade instead, so this never touches a kernel
	// this one trades with. Returns the number of kernels evicted.
	PurgeStaleDiscovery(ctx context.Context, cutoff time.Time) (int, error)
	// DeactivateImportedIfHash deactivates a remote_proxy action only while its contract hash still
	// matches expectedHash (§13 rule C: hash-conditional so a stale dispatch's late rejection cannot
	// deactivate a re-resolved row). A no-op when the row is absent or its hash has changed.
	DeactivateImportedIfHash(ctx context.Context, actionID, expectedHash string, updatedAt time.Time) error
}

// SecretBox provides authenticated encryption for upstream action credentials.
// Seal encrypts plaintext authenticated with aad (the action ID).
// Open decrypts ciphertext, returning an error if the AAD or ciphertext are invalid.
type SecretBox interface {
	Seal(aad, plaintext string) (string, error)
	Open(aad, ciphertext string) (string, error)
}
