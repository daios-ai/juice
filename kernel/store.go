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
// grant.grantor_user_id == ownerUserID. kernel/ defines this interface; cmd/juice implements it.
type HTTPExecutor interface {
	Execute(ctx context.Context, action *Action, args map[string]any, ownerUserID string) (map[string]any, error)
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
// transport (§13), addressing the peer by its Ed25519 public key. actionRef is the remote
// action reference (@owner/name); the transport signs the request as this kernel and resolves
// peerPublicKey to a live path (direct / hole-punched / relayed). HTTPExecutor implementations
// may optionally implement this interface; kernel checks via type assertion.
type FederationExecutor interface {
	ExecuteFederation(ctx context.Context, peerPublicKey, actionRef, idempotencyKey string, args map[string]any) (FederationResult, error)
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
	// ---- Users ----

	CreateUser(ctx context.Context, u *User) error
	ReadUser(ctx context.Context, id string) (*User, error)
	ReadUserByHandle(ctx context.Context, handle string) (*User, error)
	ReadUserByPublicKey(ctx context.Context, publicKey string) (*User, error)
	ListUsers(ctx context.Context, limit, offset int) ([]*User, error)
	SuspendUser(ctx context.Context, id string) error
	UnsuspendUser(ctx context.Context, id string) error
	UpdateUser(ctx context.Context, u *User) error
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
	ListPublicActions(ctx context.Context, limit, offset int) ([]*Action, error)
	// ListActionsByOwner returns all non-deleted actions owned by ownerID, including
	// inactive and private ones. Used to give an owner their full private view.
	ListActionsByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*Action, error)
	ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error)

	// ---- Processes ----

	// BeginRun atomically debits price from owner.available→locked, creates the process
	// with available=0/locked=price, and creates the root trace with available=price.
	// All precondition checks must happen in Go before calling BeginRun.
	// Returns ErrInsufficientFunds if owner.available < price.
	BeginRun(ctx context.Context, p *Process, t *Trace, ownerID string, price int64) error

	ReadProcess(ctx context.Context, id string) (*Process, error)
	ListProcesses(ctx context.Context, ownerID string, limit, offset int) ([]*Process, error)
	ListAllProcesses(ctx context.Context, limit, offset int) ([]*Process, error)

	// BeginSubcall atomically deducts price from the parent trace's available into its locked
	// and creates the child trace with available=price.
	BeginSubcall(ctx context.Context, parentTraceID string, t *Trace, price int64) error

	// BeginStepCall atomically moves step.price from the step's parent_trace.locked back into
	// parent_trace.available (the step is being consumed), creates the new trace with
	// available=step.price, and transitions the step waiting→running.
	BeginStepCall(ctx context.Context, stepID string, t *Trace) error

	// CommitCall atomically records a successful transaction, creates its receipt,
	// settles funds (trace.available→target/sys; caller wallet locked released;
	// owner.locked decremented by taxable), updates trace latency, upserts action stats,
	// completes the idempotency record (if non-empty), and marks the step done (if non-empty).
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
	// signed charge (gross − refund) is guaranteed to match what is committed.
	CommitFailedCall(ctx context.Context, tx *Transaction, buildReceipt func(refund int64) (*Receipt, error), traceID, callerWalletID, callerWalletKind string, gross int64, stats *Stats, idempotencyRecordID, errorCode, stepID string) error

	// EndProcess cancels all waiting steps (returning parked prices to the process owner's
	// available balance), then returns process.available to the owner, and closes the process.
	EndProcess(ctx context.Context, processID string) error

	// ---- Traces ----

	ReadTrace(ctx context.Context, id string) (*Trace, error)
	// ReadRootTrace returns the root trace (ParentTraceID IS NULL) for the given process.
	ReadRootTrace(ctx context.Context, processID string) (*Trace, error)

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

	// CommitRemoteSettlement atomically settles a remote-proxy call:
	// releases the gross lock from the caller wallet, pays charge→proxyUserID and duty→feeRecipientID,
	// returns the refund (gross−charge−duty) to the caller wallet, decrements owner.locked by taxable,
	// records the transaction+receipt, updates stats, marks step done (if stepID non-empty),
	// completes the idempotency record (if idempotencyRecordID non-empty), and closes the process if quiescent.
	CommitRemoteSettlement(ctx context.Context, tx *Transaction, receipt *Receipt, traceID, callerWalletID, callerWalletKind, proxyUserID, feeRecipientID string, charge, duty int64, stats *Stats, idempotencyRecordID, stepID string) error

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
	InitFirstBoot(ctx context.Context, u *User, configs map[string]string) error

	// ---- Receipts (by ID) ----

	// ReadReceipt returns the receipt with the given ID.
	ReadReceipt(ctx context.Context, id string) (*Receipt, error)

	// ---- Adjustments ----

	// CreateAdjustment atomically applies a.Direction to target.available and records the
	// adjustment. A credit adds amount; a debit subtracts it and returns ErrInsufficientFunds
	// if target.available < amount. When a.ExternalKey is set and already present, the existing
	// record is returned (loaded into a) and no balance change is applied — the idempotency
	// check runs before the debit's available-balance guard.
	CreateAdjustment(ctx context.Context, a *Adjustment) error

	// ---- Embeddings ----

	// UpsertEmbedding stores a pre-computed embedding vector for an action.
	UpsertEmbedding(ctx context.Context, actionID string, vec []float32) error
	// ListEmbeddings returns stored embedding vectors keyed by action ID,
	// filtered to active, public, non-deleted actions only.
	ListEmbeddings(ctx context.Context) (map[string][]float32, error)
	// UpsertLookupText replaces an action's lexical-index text (§9 hybrid lookup).
	UpsertLookupText(ctx context.Context, actionID, text string) error
	// SearchActionsLexical returns up to limit active action IDs matching query, BM25-ranked best-first.
	SearchActionsLexical(ctx context.Context, query string, limit int) ([]string, error)

	// ---- Users (extended) ----

	// DenyUser sets denied_at on the user (unfriend operation).
	DenyUser(ctx context.Context, id string) error
	// UndenyUser clears denied_at on the user.
	UndenyUser(ctx context.Context, id string) error
	// UpdatePeerSync records a successful friend gossip pull (§13 peer sync): peer_last_seen=now
	// and, when the peer reported one, peer_credit=credit (nil leaves the prior value). Display-only
	// cache; never a money path.
	UpdatePeerSync(ctx context.Context, id string, lastSeen time.Time, credit *int64) error

	// ---- Gossip / Federation ----

	// CreateOrUpdateDiscoveredKernel upserts a DiscoveredKernel row keyed by (public_key, introduced_by).
	CreateOrUpdateDiscoveredKernel(ctx context.Context, k *DiscoveredKernel) error
	// ListDiscoveredKernels returns all discovered kernel rows.
	ListDiscoveredKernels(ctx context.Context) ([]*DiscoveredKernel, error)

	// ---- Peer lifecycle ----

	// DenyPeerCascade atomically: sets denied_at on the user, deactivates all their proxy
	// actions, and cancels+refunds all waiting steps addressed to them as caller.
	DenyPeerCascade(ctx context.Context, userID string) error
	// ListPurgeablePeers returns the IDs of peer users (public_key set) that are idle past
	// cutoff at zero balance (§13 Retention): available=0, locked=0, last activity (max of
	// created_at, latest transaction naming them, latest deposit/withdrawal, latest gossip
	// mention) before cutoff, and no waiting/running step addressed to them or to their actions.
	ListPurgeablePeers(ctx context.Context, cutoff time.Time) ([]string, error)
	// PurgePeerCascade atomically deletes a purged peer's derived data — its proxy actions,
	// their stats and stat_tags, its steps, and its discovered_kernels rows — and forgets the
	// peer identity by clearing public_key and denied_at on the user row. The immutable
	// transaction/receipt ledger is preserved (party ids carry no FK), keeping local
	// counterparties' credits reconstructible (§11); the anonymized user row stays as a ledger
	// anchor so old history remains legible.
	PurgePeerCascade(ctx context.Context, userID string) error
	// DeactivateActionsOwnedBy sets active=false for all non-deleted actions owned by ownerUserID.
	DeactivateActionsOwnedBy(ctx context.Context, ownerUserID string) error
	// CancelAndRefundStepsForCaller cancels all waiting steps where required_caller_user_id=callerUserID,
	// atomically refunding each step's parked price to its parent trace (available+=price, locked-=price).
	CancelAndRefundStepsForCaller(ctx context.Context, callerUserID string) error
	// ListStatsByOwner returns Stats rows for actions owned by ownerUserID that have uses > 0.
	// Used by GetGossip to identify transacted friends.
	ListStatsByOwner(ctx context.Context, ownerUserID string) ([]*Stats, error)

	// ---- StatTags (gossip endorsements) ----

	// UpsertStatTag creates or updates a stat_tag row keyed by (action_id, key, source).
	UpsertStatTag(ctx context.Context, tag *StatTag) error
	// ListStatTagsByAction returns all stat_tag rows for an action.
	ListStatTagsByAction(ctx context.Context, actionID string) ([]*StatTag, error)
}

// SecretBox provides authenticated encryption for upstream action credentials.
// Seal encrypts plaintext authenticated with aad (the action ID).
// Open decrypts ciphertext, returning an error if the AAD or ciphertext are invalid.
type SecretBox interface {
	Seal(aad, plaintext string) (string, error)
	Open(aad, ciphertext string) (string, error)
}
