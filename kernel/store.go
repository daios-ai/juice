// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"context"
	"encoding/json"
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

// FederationExecutor sends a cross-kernel call to a remote proxy target over the federation
// transport (§13), addressing the peer by its Ed25519 public key. actionID is the action's stable
// id on that peer; expectedContractHash is the cached contract hash the call
// binds as the §8 If-Match precondition. The transport signs the request as this kernel and
// resolves peerPublicKey to a live path (direct / hole-punched / relayed).
type FederationExecutor interface {
	ExecuteFederation(ctx context.Context, peerPublicKey, actionID, expectedContractHash, idempotencyKey, commitment string, lottery int64, args map[string]any) (FederationResult, error)
}

// TicketRevealer carries one signed reveal to a peer over /juice/fed/settle/1 (P10): how a draw came
// out, and where the money for a winning one comes from. The kernel owns the payload and its
// signature; the adapter owns the wire shape and its deadline, and never imports fed.
type TicketRevealer interface {
	Reveal(ctx context.Context, peerPublicKey string, payload RevealPayload, signature string) error
}

// RemoteResolver resolves a single remote action or user on demand over /juice/fed/resolve/1
// (§13 subscription-free calls). ResolveRemoteAction returns the peer's signed manifest for one
// action; ResolveRemoteUser maps a user reference to its stable id and handle on the peer. A nil
// federation client disables lazy resolution (a cold cross-kernel ref is then a plain ErrNotFound).
type RemoteResolver interface {
	ResolveRemoteAction(ctx context.Context, peerPublicKey, owner, name string) (*ResolvedAction, error)
	ResolveRemoteUser(ctx context.Context, peerPublicKey, ref string) (userID, handle string, err error)
}

// StepCaller carries one /juice/fed/step/1 request to a peer (§13): listing the steps parked for
// this kernel, or completing one. Like TicketRevealer, it takes the signed scalars the kernel
// produced and returns the peer's raw status/body — the kernel owns the protocol (key derivation,
// signing, settlement disposition), the adapter owns the wire shape and its transport deadline.
// notDispatched reports the §13 never-dispatched proof: the request provably never left this host.
type StepCaller interface {
	CompletePeerStep(ctx context.Context, peerKey, timestamp, signature, stepID, idempotencyKey string,
		input []byte, forUserID, userAttestation, userTimestamp string, userSuperuser bool) (status int, body []byte, notDispatched bool, err error)
	ListPeerSteps(ctx context.Context, peerKey, timestamp, signature, forUserID string) (status int, body []byte, notDispatched bool, err error)
}

// FederationClient is the outbound federation adapter: everything the kernel needs to reach a peer
// (§13) — dispatch a call, reveal a draw, resolve one action or principal, carry a step.
// cmd/juice supplies one object implementing all of them; the kernel holds it as a single named
// dependency rather than type-asserting capabilities out of the HTTP executor, so a federation
// change never touches the HTTP adapter. A nil client means federation is unconfigured, reported
// per call site.
type FederationClient interface {
	FederationExecutor
	TicketRevealer
	RemoteResolver
	StepCaller
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
	UpdateAction(ctx context.Context, a *Action) error
	// UpdateActionLifecycle atomically updates the action record and, as flagged, zeros its stats
	// row and deletes its delegated grants. One update can need both effects at once — an active
	// source change on a delegated action changes the contract and invalidates standing consent —
	// so the two are flags on one commit rather than separate operations (§5).
	UpdateActionLifecycle(ctx context.Context, a *Action, resetStats, revokeGrants bool) error
	DeleteAction(ctx context.Context, id string) error
	// DeleteActionAndGrants atomically soft-deletes the action and deletes its delegated grants:
	// a deleted action can never be called again, so no consent may outlive it (§5, §8).
	DeleteActionAndGrants(ctx context.Context, id string) error
	// ListVisibleActions returns active non-deleted actions with a non-suspended owner, network-wide
	// (visibility=public) and, when includeLocal is set, also kernel-local ones (§4/§14).
	ListVisibleActions(ctx context.Context, includeLocal bool, limit, offset int) ([]*Action, error)
	// ListExportableActionsAfter returns one page of public actions in id order, for a catalogue scan.
	ListExportableActionsAfter(ctx context.Context, afterActionID string, limit int) ([]*Action, error)
	// ListActionsByOwner returns all non-deleted actions owned by ownerID, including
	// inactive and private ones. Used to give an owner their full private view.
	ListActionsByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*Action, error)
	ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error)
	// ListNativeActions returns all non-deleted kind=native actions (the platform stdlib rows).
	// Used at startup to prune natives whose handler the build no longer registers.
	ListNativeActions(ctx context.Context) ([]*Action, error)

	// ---- Processes ----

	// BeginRun is D3's run write set. A positive reserve marks an inbound foreign call, admitted
	// against the credit limit in the SAME statement — checking it in Go first would race two
	// concurrent admissions past one limit (D14).
	BeginRun(ctx context.Context, p *Process, t *Trace, ownerID string, price, reserve, limit int64) error

	ReadProcess(ctx context.Context, id string) (*Process, error)
	ListProcesses(ctx context.Context, ownerID string, limit, offset int) ([]*Process, error)
	ListAllProcesses(ctx context.Context, limit, offset int) ([]*Process, error)

	// BeginSubcall is D3's call-entry write set.
	BeginSubcall(ctx context.Context, parentTraceID string, t *Trace, price int64) error

	// BeginStepCall is D3's step-call write set.
	BeginStepCall(ctx context.Context, stepID string, t *Trace) error

	// CommitCall is D3's success write set.
	CommitCall(ctx context.Context, tx *Transaction, receipt *Receipt, traceID, callerWalletID, callerWalletKind, targetUserID, feeRecipientID string, net, fee int64, stats *Stats, idempotencyRecordID, stepID string) error

	// CommitFailedCall is D3's failure write set. Two traps for an implementation: user.locked is
	// decremented at process closure, never here, or the two double-count; and buildReceipt runs
	// INSIDE the transaction, with the computed refund, so the signed charge cannot disagree with
	// what is committed.
	CommitFailedCall(ctx context.Context, tx *Transaction, buildReceipt func(refund int64) (*Receipt, error), traceID, callerWalletID, callerWalletKind, feeRecipientID string, gross int64, stats *Stats, idempotencyRecordID, stepID string) error

	// EndProcess is D3's closure write set.
	EndProcess(ctx context.Context, processID string) error

	// ---- Traces ----

	ReadTrace(ctx context.Context, id string) (*Trace, error)
	// ReadRootTrace returns the root trace (ParentTraceID IS NULL) for the given process.
	ReadRootTrace(ctx context.Context, processID string) (*Trace, error)
	// ListTraces returns a process's traces: listing them yields its execution tree (D11).
	ListTraces(ctx context.Context, processID string) ([]*Trace, error)
	// TraceHasTransaction reports whether the trace has settled, i.e. a transaction row exists
	// for it (the settled-once predicate; §9 capability validity, §11 unique trace transaction).
	TraceHasTransaction(ctx context.Context, traceID string) (bool, error)

	// ---- Transactions ----

	ReadTransaction(ctx context.Context, id string) (*Transaction, error)
	ListTransactions(ctx context.Context, filter TxFilter) ([]*Transaction, error)

	// ---- Receipts ----

	ReadReceiptByTxID(ctx context.Context, txID string) (*Receipt, error)

	// ---- Ratings ----

	// ListPublicRatings is the action's public projection, local and trade-backed peer ratings
	// together, paged in the query (D11, D16).
	ListPublicRatings(ctx context.Context, actionID, subjectKernel, subjectAction, selfKey string, limit, offset int) ([]PublicRating, error)
	ReadRatingByTxID(ctx context.Context, txID string) (*Rating, error)
	// CreateRatingAndUpdateStats atomically inserts a rating and updates rating_count/rating_estimate.
	CreateRatingAndUpdateStats(ctx context.Context, r *Rating, actionID string, rating float64) error
	// ListRatings returns ratings for a given action ordered by created_at DESC.
	ListRatings(ctx context.Context, actionID string, limit, offset int) ([]*Rating, error)

	// ---- Idempotency ----

	// ReadIdempotencyRecord returns the lock held for key + counterparty, or ErrNotFound.
	ReadIdempotencyRecord(ctx context.Context, key, counterpartyUserID string) (*IdempotencyRecord, error)
	// ReadIdempotencyRecordByID reads the lock a trace was admitted under, for recovery.
	ReadIdempotencyRecordByID(ctx context.Context, id string) (*IdempotencyRecord, error)
	// InsertPendingIdempotencyRecord takes the lock for one request, or returns the record already
	// holding it — the caller's signal that this request is in flight.
	InsertPendingIdempotencyRecord(ctx context.Context, r *IdempotencyRecord) (*IdempotencyRecord, error)
	// DeleteIdempotencyRecord releases the lock, unless a trace still depends on it.
	DeleteIdempotencyRecord(ctx context.Context, id string) error
	// ReadFederatedOutcome returns the receipt this kernel signed for one request and the reply its
	// transaction recorded, or ErrNotFound. The permanent answer to a repeated request (P4).
	ReadFederatedOutcome(ctx context.Context, counterparty, key string) (*Receipt, json.RawMessage, error)

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
	ListStepsAwaitingCaller(ctx context.Context, requiredCallerUserID, remoteUserID string, limit int) ([]*Step, error)
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

	// ListReadyTraces are the traces a recorded outcome can settle now: outcome recorded, no
	// transaction, nothing unsettled beneath (D3). Every settlement commit refuses with
	// ErrSettlementDeferred and records the outcome when a child is still in flight.
	ListReadyTraces(ctx context.Context) ([]*Trace, error)
	// ConsumedByChildren is what the trace's settled children kept: the sum of their gross less
	// refund, the part of the trace's allocation that left it for good.
	ConsumedByChildren(ctx context.Context, traceID string) (int64, error)
	// ListUnsettledTracesForProcess returns all traces for a process that have no committed
	// transaction, ordered deepest-first. Includes both orphan and pending-remote traces.
	// Used by EndProcess to fail in-flight calls before closure.
	ListUnsettledTracesForProcess(ctx context.Context, processID string) ([]*Trace, error)

	// CommitRemoteSettlement atomically settles an outbound remote-proxy call (P7, P10).
	//
	// The call's budget pays the obligation and the import fee exactly, whatever the draw said: the
	// refund (q − obligation − importFee) returns to the caller wallet and importFee to
	// feeRecipientID. The obligation itself does not go to the peer's row — a peer row holds no money
	// — it returns to the caller C, whose own stake then carries the draw: on a losing ticket C keeps
	// it, and on a winning one `payout` reserves the face value from C into the operator's hold for
	// the rail to send. releaseStake is what C locked at dispatch.
	//
	// The stake and the caller are read from the trace, so every settlement path releases exactly
	// what was locked whether or not anything was owed.
	CommitRemoteSettlement(ctx context.Context, tx *Transaction, receipt *Receipt, traceID, callerWalletID, callerWalletKind, feeRecipientID string, obligation, importFee int64, payout *RailTransfer, stats *Stats, idempotencyRecordID, stepID string) error

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
	DeleteGrantsForAction(ctx context.Context, actionID string) error
	// DeleteGrantsForAction removes every grant on an action (deactivating update / delete).

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

	// ---- Rail (D23) ----
	// Every external movement is one row keyed by the fact that caused it, so booking a payment
	// twice is impossible however it was found, and the money in transit is one query.

	// CreateRailDeposit records a payment in: the crossing credits sys and holds it, and when
	// toUserID is set the same commit delivers row.Credit to that account and keeps the rest as the
	// operator's. Replaying the same fact returns what was written and moves nothing; the same fact
	// on other terms is refused, since the stateless manual rail cannot refuse it itself.
	CreateRailDeposit(ctx context.Context, sys string, row *RailTransfer, toUserID string) (*LedgerEntry, error)

	// ReserveRailTransfer opens an outgoing payment: it debits the party, moves what the operator
	// adds, holds the whole amount on sys, records the ledger leg, and writes the row with its
	// destination. From this instant the money is unavailable to everyone.
	ReserveRailTransfer(ctx context.Context, sys string, row *RailTransfer) error

	// RecordRailOutcome stores what presenting the payment produced. A refill, when given, is
	// locked from sys in the same commit, so the authorization and its lock cannot diverge.
	RecordRailOutcome(ctx context.Context, sys, id, status, txHash, reason string, refill *RailTransfer) error

	// FinalizeRailTransfer closes a confirmed payment: the hold is released and the money crosses
	// out of the ledger under the transaction that carried it.
	FinalizeRailTransfer(ctx context.Context, sys, id, txHash string, at time.Time) error

	// RetryRailTransfer starts a fresh attempt at a payment the rail signed and then refused. The
	// money stays committed and the row keeps its identity; only the name it is presented under
	// changes, since a settled rail operation cannot be asked again.
	RetryRailTransfer(ctx context.Context, id string) error
	// CompensateRailTransfer undoes a reservation whose payment finalized without executing,
	// returning the credit to its owner. The original entries are never edited.
	CompensateRailTransfer(ctx context.Context, sys, id string, at time.Time) (*LedgerEntry, error)

	// BindRefill ties the lock taken before the rail signed to the purchase it made and settles the
	// lock at that purchase's maximum; idempotent for the same purchase. ReleaseRefill returns a lock
	// that bought nothing. ReadRailTransferByRefill finds the lock a purchase is bound to.
	BindRefill(ctx context.Context, sys, id, refillID string, max int64) error
	ReleaseRefill(ctx context.Context, sys, id string) error
	ReadRailTransferByRefill(ctx context.Context, refillID string) (*RailTransfer, error)

	// BookRefill closes a refill against the exact amount it consumed, releasing the rest of the
	// authorized maximum. The maximum is authority; only the cost is ever booked.
	BookRefill(ctx context.Context, sys, id string, cost int64, executed bool, at time.Time) error

	// ReadRailTransfer returns one row, or nil when the fact is unknown here.
	ReadRailTransfer(ctx context.Context, id string) (*RailTransfer, error)

	// ListRailTransfers filters by kind, party and status — any of which may be empty for all —
	// oldest first, bounded by limit. It answers what happened, so finished rows are included.
	ListRailTransfers(ctx context.Context, kind, party, status string, limit, offset int) ([]*RailTransfer, error)

	// ListOpenRailTransfers returns the rows that still have work to do. Kept apart from the reading
	// question so a long history cannot crowd out the few rows the worker must drive.
	ListOpenRailTransfers(ctx context.Context, limit int) ([]*RailTransfer, error)

	// RailPosition sums the ledger into the operator's account of external money (D23).
	RailPosition(ctx context.Context, sys string) (*RailPosition, error)

	// SetRailAddress records where an account is paid. The address is unique across accounts.
	SetRailAddress(ctx context.Context, userID, address string, at time.Time) error

	// ListLedgerByUser returns ledger entries where userID is the source or the
	// destination, most recent first, bounded by limit/offset.
	ListLedgerByUser(ctx context.Context, userID string, limit, offset int) ([]*LedgerEntry, error)
	// ReadLedgerByExternalKey returns the entry recorded under an idempotency key, or nil.
	ReadLedgerByExternalKey(ctx context.Context, externalKey string) (*LedgerEntry, error)

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

	// ---- Peer contact ----

	// RecordKernelContact records one contact observation, keyed by public key: a success advances
	// last_seen, a failure advances last_contact_failed_at. Each timestamp only moves forward and
	// neither is cleared, so a slow observation cannot overwrite newer truth. An unknown key is a
	// no-op — observation binds nothing (§13). Display-only cache; never a money path, never
	// retention activity.
	RecordKernelContact(ctx context.Context, publicKey string, ok bool, at time.Time) error

	// ---- Obligations and exposure (P10) ----
	//
	// An obligation is a projection over the call's own records — the trace with its frozen terms
	// and reveal, the receipt with its charge, the idempotency record with the name both kernels
	// share — never a row of its own.

	// ReadOwed returns one obligation by its call's idempotency key, scoped to the peer it was
	// agreed with, or nil when there is none.
	ReadOwed(ctx context.Context, id, peerUserID string) (*Owed, error)
	// ApplyReveal records on the trace how a draw came out. An amount waits for the payment that
	// carries it, from the payer frozen at admission; nothing owed closes the obligation outright,
	// and the exposure it added stays either way, since only cash reduces exposure. Where the reveal
	// is itself the payment (D23) the caller passes that payment, booked in the same statement.
	ApplyReveal(ctx context.Context, sys, traceID string, amount int64, txHash string, payment *RailTransfer) error
	// ReconcileDeposits is the one path every observed payment takes: obligations whose money has
	// arrived are closed first — the join is the rule, so no caller can credit a payment from the
	// wrong sender, amount or transaction — and whatever no obligation claimed is then attributed to
	// the account that registered the address it came from. Returns the credits it wrote.
	ReconcileDeposits(ctx context.Context, sysID string, limit int) ([]*LedgerEntry, error)
	// ListOwed is every obligation this kernel is still waiting to be paid for, oldest first —
	// including one whose buyer has not yet said how the draw came out.
	ListOwed(ctx context.Context, limit int) ([]*Owed, error)
	// PeersWithUnresolvedMoney is every peer some money is waiting on, in either direction, as keys
	// alone — what the discovery order reads to know whom to talk to first.
	PeersWithUnresolvedMoney(ctx context.Context) ([]string, error)
	// Exposure returns what this kernel has delivered to foreign buyers and not been paid for. It may
	// be negative: premium income accumulates there.
	Exposure(ctx context.Context) (int64, error)
	// ListPendingReveals returns the calls whose seller has still to be told how the draw came out,
	// assembled from the trace, its transaction and the payment row — only those actionable now, so
	// a payment in flight never blocks the reveals behind it.
	ListPendingReveals(ctx context.Context, limit int) ([]*PendingReveal, error)
	// MarkRevealed records that the seller has acknowledged one, closing the payment's row with it.
	MarkRevealed(ctx context.Context, traceID string) error
	// MarkRevealFailed moves an undeliverable reveal behind those not yet tried.
	MarkRevealFailed(ctx context.Context, traceID string, at time.Time) error

	// ---- Kernels (identity, naming, discovery) ----

	// UpsertKernel records an observation: nickname, about, timestamps. It never writes the petname
	// (assigned locally, only on our own outbound act) nor gossip_cursor/last_seen, each of which
	// advances only after its own work is verified and committed (§13).
	UpsertKernel(ctx context.Context, publicKey, nickname, about, railAddress, railProof string, now time.Time) error
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

	// ApplyCatalogPage commits one page of a peer's catalogue: its documents stamped with the scan
	// that carried them, the sweep of what a completed scan never mentioned, and where the scan now
	// stands — one transition, one commit.
	ApplyCatalogPage(ctx context.Context, kernelPublicKey string, docs []*DiscoveryDoc, nextCursor string, generation int64) error
	// DiscoveryEmbedding returns a stored embedding when the text behind it has not changed.
	DiscoveryEmbedding(ctx context.Context, kernelPublicKey, actionID, text string) ([]float32, bool)
	// CatalogScan reads where a peer's catalogue scan stands.
	CatalogScan(ctx context.Context, kernelPublicKey string) (string, int64, error)
	// ListDiscoveryDocs returns all discovery docs (with embeddings) for the lookup dense leg.
	ListDiscoveryDocs(ctx context.Context) ([]*DiscoveryDoc, error)
	// SearchDiscoveryLexical returns doc_keys ranked by BM25 for query, most relevant first.
	SearchDiscoveryLexical(ctx context.Context, query string, limit int) ([]string, error)

	// ---- Evidence cache (regenerable reputation cache, §13) ----

	// UpsertEvidence stores or merges one verified evidence row, enforcing the late-rating
	// transitions and the E-per-(issuer,subject_kernel,subject_action) cap (§13).
	UpsertEvidence(ctx context.Context, e *EvidenceRow) error
	// ListEvidenceBySubject returns evidence rows about a subject kernel, for inspect display.
	ListEvidenceBySubject(ctx context.Context, subjectKernelPublicKey, subjectActionID string) ([]*EvidenceRow, error)
	// ListReceiptsForGossip returns one ordered page of this kernel's own gossip-eligible receipts
	// (with any joined rating and remote receipt) after the cursor, for the evidence sender (§13).
	ListReceiptsForGossip(ctx context.Context, cursor string, limit int) ([]*GossipReceiptRow, error)

	// ---- Peer lifecycle ----

	// ListPurgeablePeers returns the IDs of peer users (kernel_public_key set) that are idle past
	// cutoff at zero balance (§13 Retention): available=0, locked=0, last activity (max of
	// created_at, latest transaction naming them, latest deposit/withdrawal, latest gossip
	// mention) before cutoff, and no waiting/running step addressed to them or to their actions.
	ListPurgeablePeers(ctx context.Context, cutoff time.Time) ([]string, error)
	// PurgePeerCascade atomically deletes a purged peer's derived data — its proxy actions,
	// their stats, its steps, its discovered_kernels row, its discovery_docs, and its evidence
	// rows (as issuer and as subject) — and forgets the peer identity by clearing kernel_public_key on the
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
