// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"encoding/json"
	"time"
)

// ActionKind describes how an action is executed.
type ActionKind string

const (
	KindHTTP        ActionKind = "http"
	KindWasm        ActionKind = "wasm"
	KindNative      ActionKind = "native"
	KindRemoteProxy ActionKind = "remote_proxy"
)

// ProcessStatus is the lifecycle state of a process.
type ProcessStatus string

const (
	ProcessOpen   ProcessStatus = "open"
	ProcessClosed ProcessStatus = "closed"
)

// TxStatus is the outcome of a call attempt.
type TxStatus string

const (
	TxSuccess TxStatus = "success"
	TxFailure TxStatus = "failure"
)

// Account is the local financial, authentication, and moderation principal: balances, credentials,
// and the ledger identity every transaction party is captured under (§3). A local user holds a
// Handle and password/recovery credentials; a remote kernel's account instead holds
// KernelPublicKey and no credentials at all, so the two entities stay distinct while sharing one
// wallet model. RecoveryPublicKey is a recovery credential (§12), never a federation identity.
type Account struct {
	ID           string     `json:"id"`
	Handle       string     `json:"handle"`      // empty on a kernel account — a kernel is named by its petname (§13)
	Description  string     `json:"description"` // free-text "about"; sys's is the kernel's about (§13)
	PasswordHash string     `json:"-"`
	Available    int64      `json:"available"`
	Locked       int64      `json:"locked"`
	SuspendedAt  *time.Time `json:"suspended_at,omitempty"`
	// KernelPublicKey links this account to the remote kernel it settles for — the sole
	// account↔kernel relationship (§3), and the peer discriminator. Empty on a local user.
	KernelPublicKey string `json:"kernel_public_key,omitempty"`
	// RecoveryPublicKey is the account's own Ed25519 recovery key (base64url), enrolled at
	// creation from a client-held seed phrase; the server stores only the public half and never
	// the mnemonic (§12).
	RecoveryPublicKey string `json:"-"`
	// BlockchainAddress is where this account is paid on the rail, proven and canonical (D23).
	BlockchainAddress string    `json:"blockchain_address,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// IsPeer reports whether a is a remote kernel's account (§13): it authenticates by federation
// signature and is denied local-visibility actions (§4).
func (a *Account) IsPeer() bool { return a != nil && a.KernelPublicKey != "" }

// IsLiveUser reports whether a is a usable local user: an account holding a handle. The three
// states are exhaustive — a handle means a live user, a kernel key means a kernel account, and
// neither means a purged peer's tombstone, which anchors the ledger (§13 Retention) but is history.
// "Not a peer" therefore does not imply "live user", and mutating operations must test this.
func (a *Account) IsLiveUser() bool { return a != nil && a.Handle != "" }

// IsLive reports whether a names something that still exists to act or be acted upon. A tombstone
// stays readable for historical enrichment but is refused by every live and mutating operation.
func (a *Account) IsLive() bool { return a.IsLiveUser() || a.IsPeer() }

// ActionVisibility is the callability scope of an action (§4). It replaces the earlier boolean
// public flag with three levels, controlling the direct dependency surface, not reachability.
type ActionVisibility string

const (
	// VisibilityPrivate: callable only when the caller is the action owner.
	VisibilityPrivate ActionVisibility = "private"
	// VisibilityLocal: callable by any local (non-peer) caller; never served in manifests or gossip.
	VisibilityLocal ActionVisibility = "local"
	// VisibilityPublic: callable by anyone including peers; served in manifests/gossip (§13).
	VisibilityPublic ActionVisibility = "public"
)

// ValidActionVisibility reports whether v is one of the three defined visibility levels.
func ValidActionVisibility(v ActionVisibility) bool {
	return v == VisibilityPrivate || v == VisibilityLocal || v == VisibilityPublic
}

// Action is a callable capability.
type Action struct {
	ID             string           `json:"id"`
	OwnerUserID    string           `json:"owner_user_id"`
	OwnerHandle    string           `json:"owner_handle,omitempty"`    // populated via JOIN; empty if not loaded
	OwnerSuspended bool             `json:"owner_suspended,omitempty"` // populated via JOIN; true when the owner is suspended (§12)
	Name           string           `json:"name"`
	Kind           ActionKind       `json:"kind"`
	Active         bool             `json:"active"`
	Visibility     ActionVisibility `json:"visibility"`
	Price          int64            `json:"price"`
	Description    string           `json:"description"`
	InputSchema    map[string]any   `json:"input_schema"`
	OutputSchema   map[string]any   `json:"output_schema"`
	Source         string           `json:"source,omitempty"`           // structured HTTPSource JSON for http; TinyGo source for wasm; federation URL for remote_proxy
	ArtifactHash   string           `json:"artifact_hash,omitempty"`    // content-addressed compiled WASM artifact
	WasmArtifact   string           `json:"wasm_artifact,omitempty"`    // base64-encoded compiled WASM bytes (wasm only); Source holds the TinyGo text
	RemoteActionID string           `json:"remote_action_id,omitempty"` // ID of the action on the remote kernel (remote_proxy only)
	RemoteOwnerID  string           `json:"remote_owner_id,omitempty"`  // stable owner user_id on the remote kernel (with peer key = PrincipalID, §13)
	RemoteBPS      *int64           `json:"remote_bps,omitempty"`       // provider premium snapshot from the signed manifest; nil = pre-v0.12 proxy row
	// BasePrice is the seller's manifest price (mp) on a remote_proxy row. Price is DERIVED from it
	// and the current import_bps at read (§8, §16 Price Snapshot Pattern), so local policy reprices
	// the catalog with no re-resolve. nil = imported before 041: the row re-resolves before it is
	// next funded rather than having mp reverse-calculated from its rounded total.
	BasePrice *int64     `json:"base_price,omitempty"`
	Effect    string     `json:"effect,omitempty"` // signed manifest contract: a privileged execution effect ("transfer", §13); empty = ordinary action
	AuthJSON  string     `json:"-"`                // AES-256-GCM encrypted upstream auth credentials; never serialized
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Upstream auth schemes (§8). Owner-held schemes carry their secret in auth_json; the delegated
// schemes carry no per-caller secret there and bind the credential to a Grant row: oauth_delegated
// via an OAuth refresh token obtained by browser consent, delegated_bearer via a static token the
// caller supplies once (a personal access token / per-user API key). An unknown scheme is rejected
// at create/update and fails closed at dispatch.
const (
	AuthSchemeHeader           = "header"
	AuthSchemeQuery            = "query"
	AuthSchemeBearer           = "bearer"
	AuthSchemeBasic            = "basic"
	AuthSchemeOAuthClientCreds = "oauth_client_credentials"
	AuthSchemeOAuthJWTBearer   = "oauth_jwt_bearer"
	AuthSchemeOAuthDelegated   = "oauth_delegated"
	AuthSchemeDelegatedBearer  = "delegated_bearer"
)

// AuthInput is a write-only upstream auth payload for action create/update.
// It is marshaled to JSON and stored encrypted in auth_json. Never returned by any API.
// For oauth_delegated, Config holds provider endpoints (auth_url, token_url, optional
// device_auth_url, client_id, scopes) and Secrets holds only an optional client_secret —
// the per-user refresh token lives in a Grant (§3), never here.
type AuthInput struct {
	Scheme  string         `json:"scheme"`
	Config  map[string]any `json:"config,omitempty"` // scheme-specific config (e.g. header name, token_url)
	Secrets map[string]any `json:"secrets"`          // credentials (never logged or returned)
}

// Grant is a user's per-action delegated consent (§8): a pointer binding one action to the
// Connection (ConnectionID) whose credential it may wield. It holds no token of its own.
type Grant struct {
	ID            string    `json:"id"`
	GrantorUserID string    `json:"grantor_user_id"`
	ActionID      string    `json:"action_id"`
	ConnectionID  string    `json:"-"` // FK to Connection (empty on unbackfilled legacy rows)
	CreatedAt     time.Time `json:"created_at"`
}

// Connection is a user's upstream account credential, stored once per (user, provider_key)
// and shared by every Grant that points at it (§8). SealedSecret (the OAuth refresh token or
// static bearer token) is AES-256-GCM sealed with AAD user_id|connection_id and write-only:
// never serialized by any read path. ScopesJSON is the requested-scope union consented so far.
type Connection struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	ProviderKey  string    `json:"provider_key"`
	SealedSecret string    `json:"-"` // sealed; never returned
	ScopesJSON   string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// GrantView is the token-free read shape for GET /v1/me: the action reference, the scopes the
// action requests (from its auth config), and when the grant was created.
type GrantView struct {
	Action      string    `json:"action"` // owner@kernel/name
	Scopes      any       `json:"scopes,omitempty"`
	ProviderKey string    `json:"provider_key,omitempty"` // key of the backing Connection (§8); empty on unbackfilled legacy grants
	CreatedAt   time.Time `json:"created_at"`
}

// ConnectionView is the token-free read shape for GET /v1/me: the provider label, how many of
// the caller's actions are consented against it, whether it is currently unused (§8), and when
// it was created.
type ConnectionView struct {
	Provider    string    `json:"provider"`
	Actions     int       `json:"actions"`
	Unused      bool      `json:"unused"`
	ProviderKey string    `json:"provider_key,omitempty"` // stable account key (§8); the value DELETE /v1/grants?account= accepts
	CreatedAt   time.Time `json:"created_at"`
}

// Process is a budgeted execution context.
type Process struct {
	ID          string        `json:"id"`
	OwnerUserID string        `json:"owner_user_id"`
	Available   int64         `json:"available"`
	Locked      int64         `json:"locked"`
	Status      ProcessStatus `json:"status"`
	CreatedAt   time.Time     `json:"created_at"`
	EndedAt     *time.Time    `json:"ended_at,omitempty"`
}

// StepStatus is the lifecycle state of a step.
type StepStatus string

const (
	StepWaiting   StepStatus = "waiting"
	StepRunning   StepStatus = "running"
	StepDone      StepStatus = "done"
	StepCancelled StepStatus = "cancelled"
)

// Step is a partially applied future Call — a suspended computation boundary that
// records enough context to resume when a caller later supplies the remaining input.
// Core invariant: CompleteStep(caller, id, input) = Call(caller, trace, action_id, partial_args ⊕ input)
// The allowed completion input is derived live as action.input_schema \ keys(partial_args).
type Step struct {
	ID                     string  `json:"id"`
	ParentTraceID          *string `json:"parent_trace_id,omitempty"`
	RequiredCallerUserID   string  `json:"required_caller_user_id"`
	RequiredCallerRemoteID *string `json:"required_caller_remote_id,omitempty"` // stable remote user_id on the peer kernel (§13); nil = local required caller
	// RequiredCallerHandle is what that remote principal was called when the step was made. Display
	// only, exactly like a proxy's owner_handle (P6): the id above stays the identity, so a rename
	// on the peer leaves the step addressed correctly and only this line goes stale.
	RequiredCallerHandle string          `json:"required_caller_handle,omitempty"`
	ActionID             string          `json:"action_id"`
	PartialArgs          json.RawMessage `json:"partial_args"`
	Price                int64           `json:"price"`
	// ImportBPS freezes the origin fee this Step was funded under: CreateStep parks Price and the
	// Step may settle long after import_bps changes (§16 Price Snapshot Pattern). Remote-proxy steps
	// only; nil = parked before 041, settling from live config as before.
	ImportBPS         *int64     `json:"import_bps,omitempty"`
	Status            StepStatus `json:"status"`
	TxID              *string    `json:"tx_id,omitempty"`
	CompletionTraceID *string    `json:"completion_trace_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// OrphanRunningStep is one result row from Store.ListOrphanRunningSteps.
// HasSettled is true when the completion trace has committed subcall transactions or locked funds,
// meaning the trace cannot safely be re-parked and must instead be settled as failed.
type OrphanRunningStep struct {
	StepID            string
	CompletionTraceID string
	Price             int64
	ParentTraceID     *string
	TraceAvailable    int64
	TraceLocked       int64
	HasSettled        bool
}

// StepReply is the response from a successful CompleteStep.
type StepReply struct {
	*CallReply
	StepID string `json:"step_id"`
}

// Trace records causal structure and wallet state for one call in a call tree.
// Root traces have ParentTraceID == nil.
// Step-completion traces may have a ParentTraceID that crosses process boundaries.
// Available tracks funds remaining after subcalls and step parks; zeroed at settlement.
type Trace struct {
	ID            string  `json:"id"`
	ProcessID     string  `json:"process_id"`
	ParentTraceID *string `json:"parent_trace_id,omitempty"`
	ActionOwnerID string  `json:"action_owner_id"`
	ActionID      string  `json:"action_id"`
	CallerUserID  string  `json:"caller_user_id"`
	// CallerRemoteID/CallerHandle and TargetRemoteID/TargetHandle complete the caller's and the
	// target's principal (D4) when the account stands for a user on a peer: the caller a buying
	// kernel attested in its signed request, or the step completer it attested; the target a proxy
	// row names as its remote owner. Set once, where the call enters the kernel, and copied onto
	// the transaction by every settlement path. Empty for a local user or the peer kernel itself.
	CallerRemoteID string  `json:"-"`
	CallerHandle   string  `json:"-"`
	TargetRemoteID string  `json:"-"`
	TargetHandle   string  `json:"-"`
	Available      int64   `json:"available"`
	Locked         int64   `json:"locked"`
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
	// DispatchJSON is what this trace sent across a kernel boundary, when it did (D19): outbound
	// terms only. What it was admitted under lives on the admission record it answers.
	DispatchJSON *string `json:"dispatch_json,omitempty"`
	// IdempotencyRecordID is the inbound cross-kernel record this trace serves (§13), set only on a
	// root call made on a peer's behalf. Whichever settlement resolves the trace completes that
	// record, so a crashed or parked federated call never strands its requester.
	IdempotencyRecordID *string `json:"idempotency_record_id,omitempty"`
	// OutcomeJSON is the outcome a call reached while a trace beneath it was still in flight: a
	// trace settles only after every trace beneath it (D3), so the outcome waits here and the last
	// child's settlement commits it. Nil while executing, and once settled.
	OutcomeJSON *string `json:"-"`
	// Ticket is the lottery stake a cross-kernel call holds from its immediate caller C's own
	// balance (P10): the face value, locked at dispatch so a winning draw is funded when it lands,
	// and released by whichever settlement resolves the trace. 0 on every other call.
	Ticket int64 `json:"ticket,omitempty"`
	// OwedBlockchainAddress is where a foreign buyer proved it pays from, frozen when its call was admitted
	// (P4): the payment closing this call's obligation must come from here, and a payment from here
	// is never anyone else's while the obligation is unresolved. Empty on a local call.
	OwedBlockchainAddress string `json:"-"`
	// Value and ValueTo snapshot a TransferEffect on a call whose caller C funds a transfer (§13): the
	// amount locked from C.available at admission and the beneficiary it is delivered to at settlement
	// (refunded to C on failure). Sourced from C, not the trace budget, and untaxed, so locked and
	// delivered are one number. Both 0/"" on every non-transfer call. Riding on the trace is what lets
	// every settlement path — commit, failure, recovery, forced closure — release the lock without the
	// in-memory request.
	Value     int64     `json:"value,omitempty"`
	ValueTo   string    `json:"value_to,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Transaction records one attempted call. Immutable after creation.
type Transaction struct {
	ID            string `json:"id"`
	ProcessID     string `json:"process_id"`
	TraceID       string `json:"trace_id"`
	ParentTraceID string `json:"parent_trace_id"`
	OwnerUserID   string `json:"owner_user_id"`
	CallerUserID  string `json:"caller_user_id"`
	TargetUserID  string `json:"target_user_id"`
	// The remote halves of the caller's and target's principal, copied from the trace at
	// settlement (D4): the transaction outlives its trace, so it keeps its own.
	CallerRemoteID string `json:"-"`
	CallerHandle   string `json:"-"`
	TargetRemoteID string `json:"-"`
	TargetHandle   string `json:"-"`
	ActionID       string `json:"action_id"`
	// ActionName is the action's stored name at the time — for a proxy the folded form
	// (JoinProxyName) — never rewritten (G3); readers render it through Names.
	ActionName        string          `json:"action_name"`
	RemoteActionID    string          `json:"remote_action_id,omitempty"` // remote action ID on the far kernel; empty for local calls
	ArgsJSON          json.RawMessage `json:"args"`
	ReplyJSON         json.RawMessage `json:"result"`
	Status            TxStatus        `json:"status"`
	Gross             int64           `json:"gross"`
	Net               int64           `json:"net"`
	Fee               int64           `json:"fee"`
	Refund            int64           `json:"refund"`
	Reason            string          `json:"reason"`
	RemoteReceiptHash string          `json:"remote_receipt_hash,omitempty"` // SHA-256 of the remote receipt JSON; empty for local calls
	RemoteReceiptJSON string          `json:"remote_receipt_json,omitempty"` // full receipt JSON from the remote kernel; empty for local calls
	RemoteSignerKey   string          `json:"remote_signer_key,omitempty"`   // the key the receipt verified under at settlement, stored with it so verification outlives the peer (G7, U36)
	// EvidenceEligible records, once, whether this call's receipt is public evidence: decided when
	// the call settles, from what was true then. An action made public later does not publish what
	// it did in private, and one made private later does not erase what it did in public (P9).
	EvidenceEligible bool      `json:"-"`
	StartedAt        time.Time `json:"started_at"`
	EndedAt          time.Time `json:"ended_at"`
}

// Caller and Target are the transaction's parties as principals (D4).
func (t *Transaction) Caller() Principal {
	return Principal{AccountID: t.CallerUserID, RemoteID: t.CallerRemoteID, Handle: t.CallerHandle}
}
func (t *Transaction) Target() Principal {
	return Principal{AccountID: t.TargetUserID, RemoteID: t.TargetRemoteID, Handle: t.TargetHandle}
}

// Caller and Target are the trace's parties as principals; a transaction copies them at settlement.
func (t *Trace) Caller() Principal {
	return Principal{AccountID: t.CallerUserID, RemoteID: t.CallerRemoteID, Handle: t.CallerHandle}
}
func (t *Trace) Target() Principal {
	return Principal{AccountID: t.ActionOwnerID, RemoteID: t.TargetRemoteID, Handle: t.TargetHandle}
}

// RequiredCaller is who the step is parked for, as a principal.
func (s *Step) RequiredCaller() Principal {
	p := Principal{AccountID: s.RequiredCallerUserID, Handle: s.RequiredCallerHandle}
	if s.RequiredCallerRemoteID != nil {
		p.RemoteID = *s.RequiredCallerRemoteID
	}
	return p
}

// Stats tracks fixed performance and usage statistics for an action.
type Stats struct {
	ActionID        string    `json:"action_id"`
	Uses            int64     `json:"uses"`
	Successes       int64     `json:"successes"`
	Failures        int64     `json:"failures"`
	RatingCount     int64     `json:"rating_count"`
	LatencyEstimate float64   `json:"latency_estimate"`
	RatingEstimate  float64   `json:"rating_estimate"`
	LastUsedAt      time.Time `json:"last_used_at"`
}

// LedgerEntry is an immutable audit record of one direct balance movement, with a
// nullable source and destination: a deposit credits (FromUserID empty, ToUserID set),
// a withdrawal debits (FromUserID set, ToUserID empty), and a user transfer moves
// between two local users (both set). OperatorUserID is the authorizer — @sys for a
// deposit/withdrawal, the sender for a transfer. ExternalKey is an optional opaque
// idempotency token; when present it is globally unique and a replay re-applies no
// balance change.
type LedgerEntry struct {
	ID             string    `json:"id"`
	OperatorUserID string    `json:"operator_user_id"`
	FromUserID     string    `json:"from_user_id,omitempty"`
	ToUserID       string    `json:"to_user_id,omitempty"`
	Amount         int64     `json:"amount"`
	Reason         string    `json:"reason"`
	ExternalKey    string    `json:"external_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// RemoteKernel is a known remote kernel: one row per public key, whether or not it holds an
// account here (§3). It owns Stiegler naming state — the self-certifying key, the Nickname the
// remote asserts about itself (never resolves a reference), and the Petname assigned locally
// (resolves). GossipCursor is the persisted evidence high-watermark (§13 peer sync), advanced only
// after a page is verified and committed, so it is never written by ordinary observation.
// LastSeen and LastContactFailedAt are the contact display cache: the latest successful and latest
// failed contact, each only ever moving forward.
type RemoteKernel struct {
	PublicKey string `json:"public_key"`
	Petname   string `json:"petname,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	// BlockchainAddress is where this peer is paid, with BlockchainProof its signature proving control of it.
	// A merely declared address could name a stranger's and claim their payment (D23).
	BlockchainAddress   string     `json:"blockchain_address,omitempty"`
	BlockchainProof     string     `json:"-"`
	About               string     `json:"about,omitempty"`
	GossipCursor        string     `json:"gossip_cursor,omitempty"`
	LastSeen            *time.Time `json:"last_seen,omitempty"`
	LastContactFailedAt *time.Time `json:"last_contact_failed_at,omitempty"`
	FirstSeen           time.Time  `json:"first_seen"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// AuthCode is a short-lived PKCE authorization code.
type AuthCode struct {
	Code          string
	UserID        string
	CodeChallenge string // S256: base64url(sha256(code_verifier))
	RedirectURI   string
	ExpiresAt     time.Time
	Used          bool
}

// RefreshToken is a rotatable long-lived token that can exchange for access tokens.
type RefreshToken struct {
	Token     string
	UserID    string
	ExpiresAt time.Time
	Revoked   bool
	CreatedAt time.Time
}

// Receipt is an immutable signed record of a committed call.
// Created atomically with the transaction in CommitCall or CommitFailedCall.
type Receipt struct {
	ID           string   `json:"id"`
	IssuerUserID string   `json:"issuer_user_id"`
	TxID         string   `json:"tx_id"`
	TraceID      string   `json:"trace_id"`
	ActionID     string   `json:"action_id"`
	CallerUserID string   `json:"caller_user_id"`
	ProcessID    string   `json:"process_id"`
	ArgsHash     string   `json:"args_hash"`
	ReplyHash    string   `json:"reply_hash"`
	Status       TxStatus `json:"status"`
	Gross        int64    `json:"gross"`
	Net          int64    `json:"net"`
	Fee          int64    `json:"fee"`
	Charge       int64    `json:"charge"`
	// Premium is the serving kernel's markup (execution tax + risk premium) on this charge, credited
	// to the serving kernel's sys and owed by the origin peer on top of Charge (§13). omitempty keeps
	// it out of the JCS signature for all local and pre-v0.12 receipts (Premium=0), so those verify
	// unchanged; nonzero only on a receipt the serving kernel issues for an inbound federated call.
	Premium int64 `json:"premium,omitempty"`
	// Nonce is the serving kernel's half of the settlement draw (P10), minted after the work is done
	// and before the buyer's secret is known, so neither side can select the outcome. Hex, 32 bytes.
	// omitempty keeps it out of the JCS signature on every local and free receipt, which carry no
	// obligation to draw for and so verify unchanged.
	Nonce string `json:"nonce,omitempty"`
	// Value and ValueTo are the transfer channel, kept distinct from the execution channel
	// (Charge/Premium) so the two never mix (§13): the delivered amount — all-or-nothing, so a
	// partial-charge failure never dilutes delivery — and the beneficiary. The channel is local to a
	// kernel and untaxed, so no premium rides it. omitempty keeps both out of the JCS signature when
	// unset, so non-transfer receipts (and every receipt predating the channel) verify unchanged.
	Value   int64  `json:"value,omitempty"`
	ValueTo string `json:"value_to,omitempty"`
	// RefreshProxy signals the origin to invalidate its cached proxy for this action (§8/§13): set only
	// on a zero-charge pre-execution rejection whose fault is the cache's (contract-hash mismatch, a
	// non-executable action). omitempty keeps it out of the JCS signature for every other receipt, so
	// those verify unchanged; a receipt setting it must be a valid zero-charge rejection or it quarantines.
	RefreshProxy bool `json:"refresh_proxy,omitempty"`
	// IdempotencyKey and Counterparty bind this receipt to the one request it answers: the call's
	// own name and the buyer kernel that sent it (P4). The request signs (counterparty, recipient,
	// key); the receipt signs the same triple from the other side — the seller by its signature,
	// the buyer and the call by these two — so a receipt cannot answer a call it was not issued for
	// and two identical calls cannot share one. Set on every receipt a kernel signs for an inbound
	// federated call, executed or rejected; omitempty keeps both out of the JCS signature of every
	// local receipt, which answers no request but its caller's own.
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Counterparty   string    `json:"counterparty,omitempty"`
	Reason         string    `json:"reason"`
	StartedAt      time.Time `json:"started_at"`
	CreatedAt      time.Time `json:"created_at"`
	Signature      string    `json:"signature"`
}

// Rating is an immutable human-submitted rating for a transaction.
// Stored in a separate ratings table; the transaction row is never modified after creation.
type Rating struct {
	ID             string  `json:"id"`
	RatedTxID      string  `json:"rated_tx_id"`
	RatedReceiptID *string `json:"rated_receipt_id"` // nil for transactions predating the receipt requirement
	RaterUserID    string  `json:"rater_user_id"`
	Rating         float64 `json:"rating"`
	Note           *string `json:"note"` // optional human-readable justification
	// RatedReceiptHash is SHA-256(CanonicalJSON(rated receipt)) — the portable link a v0.13
	// evidence bundle carries so a receiver can join this rating to its receipt (§13). omitempty
	// is load-bearing: a pre-v0.13 rating was signed without this field, so keeping it out of the
	// canonical payload lets legacy ratings still verify unchanged. Wire-ingress evidence requires
	// it non-empty; legacy ratings feed local Stats only and are never gossip-eligible.
	RatedReceiptHash string    `json:"rated_receipt_hash,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	Signature        string    `json:"signature"`
}

// EmbeddedRating is the rating summary embedded in TransactionView responses.
type EmbeddedRating struct {
	Value float64 `json:"value"`
	Note  *string `json:"note"`
}

// TransactionView is a Transaction with its associated rating and, for a cross-kernel call, the name
// of the obligation that settles it. Rating is null when the transaction has not been rated.
type TransactionView struct {
	*Transaction
	Rating *EmbeddedRating `json:"rating"`
	// TicketID names the obligation this call settles, so a party can follow it and an operator
	// recording its payment can name it. The obligation itself lives on the side that is owed.
	TicketID string `json:"ticket_id,omitempty"`
}

// IdempotencyRecord prevents duplicate cross-kernel calls.
type IdempotencyRecord struct {
	ID                 string
	IdempotencyKey     string
	CounterpartyUserID string
	ArgsJSON           string // the request's exact argument bytes, so recovery can sign over them
	CreatedAt          time.Time
}

// ImportResult summarises the outcome of an import operation.
// Used by both OpenAPI import and per-call remote manifest resolution.
type ImportResult struct {
	Created     []*Action
	Unchanged   []*Action
	Updated     []*Action // rewritten: a contract change also deactivates; an auth-only change does not
	Deactivated []*Action // deactivated: removed from the source
	Rejected    []ImportRejection
}

// ImportRejection records one operation that could not be imported.
type ImportRejection struct {
	Key    string // operation_key or action name
	Reason string
}

// HTTPParam records where one input field is sent in the HTTP request.
type HTTPParam struct {
	Name string `json:"name"`
	In   string `json:"in"` // "path", "query", or "body"
}

// HTTPSource is the structured request shape stored in Action.Source for every
// kind=http action — both manually created actions and OpenAPI imports. Type is
// "http" for manual actions and "openapi" for imports; the OpenAPI provenance
// fields (SpecURL, OperationKey, PriceDeclared) are empty for manual actions, and
// import reconciliation is scoped to Type=="openapi" rows. PriceDeclared records
// whether the document set x-juice-price for this operation: an absent price
// leaves the price to the owner, so it must be distinguishable from a declared
// zero (§8).
type HTTPSource struct {
	Type          string      `json:"type"`
	SpecURL       string      `json:"spec_url,omitempty"`
	BaseURL       string      `json:"base_url"`
	Method        string      `json:"method"`
	Path          string      `json:"path"`
	OperationKey  string      `json:"operation_key,omitempty"`
	PriceDeclared bool        `json:"price_declared,omitempty"`
	Params        []HTTPParam `json:"params,omitempty"`
}

// ActionManifest is a signed, exportable description of a public active action.
type ActionManifest struct {
	ActionID     string         `json:"action_id"`
	OwnerID      string         `json:"owner_id"`     // stable owner user_id on the serving kernel (identity half of PrincipalID)
	OwnerHandle  string         `json:"owner_handle"` // owner's current display handle (mutable metadata, not identity/contract)
	Name         string         `json:"name"`
	RemoteBPS    int64          `json:"remote_bps"` // provider premium (bps) on inbound remote calls (§13)
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
	Price        int64          `json:"price"`
	Kind         ActionKind     `json:"kind"`
	ArtifactHash string         `json:"artifact_hash"`
	UpdatedAt    time.Time      `json:"updated_at"`
	Signature    string         `json:"signature"` // base64url Ed25519 signature
}

// ResolvedAction is what a resolve reply carries: the signed manifest, and where the serving kernel
// is paid with its own proof of that address. The two are separate because they answer separate
// questions — what the action is, and how its kernel is paid — and only the first is the contract a
// buyer pins. A world without payment addresses carries neither.
type ResolvedAction struct {
	Manifest          *ActionManifest `json:"manifest"`
	BlockchainAddress string          `json:"blockchain_address,omitempty"`
	BlockchainProof   string          `json:"blockchain_proof,omitempty"`
}

// PublicRating is the market-facing projection of one rating (§11, U39): value, note, when — and
// where it was given, since a rating a remote payer gave on the kernel that paid is admitted here
// only as trade-backed evidence (D16), and a reader may weigh the two differently.
type PublicRating struct {
	Value     int       `json:"value"`
	Note      *string   `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	Source    string    `json:"source"` // "local" or "peer"
}

// TraceOutcome is what a call reached, recorded on its trace by the settlement commit that refused
// because a child was still in flight (D3). The sweep that settles the trace later commits exactly
// this; nothing is re-derived from execution.
type TraceOutcome struct {
	Status  TxStatus        `json:"status"`
	Gross   int64           `json:"gross"` // the call's allocation, which what is left on the trace no longer shows
	Reason  string          `json:"reason,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Reply   json.RawMessage `json:"reply,omitempty"`
	EndedAt time.Time       `json:"ended_at"`
	StepID  string          `json:"step_id,omitempty"` // the step this trace completes, if any
}

// ReceiptVerification is the result of VerifyReceipt.
type ReceiptVerification struct {
	TransactionID         string        `json:"transaction_id"`
	Valid                 bool          `json:"valid"`
	RemoteKernelHandle    string        `json:"remote_kernel_handle,omitempty"`
	RemoteKernelPublicKey string        `json:"remote_kernel_public_key,omitempty"`
	Checks                ReceiptChecks `json:"checks"`
	Receipt               *Receipt      `json:"receipt"`
	// Unbound marks a receipt signed before receipts named the request they answer: its binding
	// cannot be audited, which is a fact about its age, not a failed check (P5, G3).
	Unbound bool `json:"unbound,omitempty"`
}

// ReceiptChecks names each audit a receipt verification ran and whether it held (§11). A local and
// a remote receipt are answerable against different facts — a cross-kernel one adds the peer's key,
// the rates its dispatch froze and the draw (P7, P10) — so a check that does not apply is absent
// rather than reported as passing.
type ReceiptChecks map[string]bool

// allHeld reports whether every check that ran held. A verification that ran none is not one.
func (c ReceiptChecks) allHeld() bool {
	for _, held := range c {
		if !held {
			return false
		}
	}
	return len(c) > 0
}

// CallerWalletKind identifies the funding source for CommitCall/CommitFailedCall.
const (
	CallerProcess = "process" // root call — lock is in process.locked
	CallerTrace   = "trace"   // subcall — lock is in parent trace.locked
	// CallerStep means the call was a step-completion: BeginStepCall already released
	// the parent trace lock. On failure the refund returns to the process.
	CallerStep = "step"
)

// RemoteKernelView is one row of the `admin peers` roster (§14): every known kernel, served by a
// single kernels LEFT JOIN accounts. Petname resolves a reference and Nickname never does, so the
// two are surfaced as separate columns. It carries no internal account id — a kernel is addressed
// by key or petname, never by one.
type RemoteKernelView struct {
	PublicKey string `json:"public_key"`
	Petname   string `json:"petname,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	About     string `json:"about,omitempty"`
	// HasAccount is true iff this kernel has ever been a counterparty here — it has called us or we
	// have called it. False for a kernel known only from discovery.
	HasAccount bool `json:"has_account"`
	// Actions is the kernel's cached public-action count (from discovery docs), 0 when unknown.
	Actions     int        `json:"actions"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
	// LastSeen and LastContactFailedAt are the contact cache (§13): when we last reached the peer,
	// and when a contact last failed. Display-only — a consumer compares the two and applies its own
	// freshness policy; the kernel judges neither.
	LastSeen            *time.Time `json:"last_seen,omitempty"`
	LastContactFailedAt *time.Time `json:"last_contact_failed_at,omitempty"`
}

// EvidenceReceipt is a wire-only signed projection of a committed call, gossiped as trade
// evidence (§13). It is never an authoritative table — the issuer builds it on demand from its
// own records at gossip-serve time. It exposes only what a third party needs to derive
// uses/failures/latency and to verify a rating link, and NOTHING that would leak a counterparty's
// business: no transaction/trace/process/caller/payer identity, no args_hash/reply_hash, no amounts,
// no value_to.
//
// ReceiptHash is SHA-256(CanonicalJSON(the stored Receipt)) — the same definition on both sides of
// the remote-receipt join, so an origin kernel's RemoteReceiptHash equals the serving kernel's
// ReceiptHash byte-for-byte. Subject{Kernel,Action} is the PrincipalID-style stable identity of the
// executed action (own key for a first-party action, the peer's key + RemoteActionID for a proxy
// call). CounterpartyKernelPublicKey is set ONLY when the execution caller was a peer kernel (an
// inbound federated call), never for a local caller and never a user identity: it is the two-kernel
// trade proof that lets a receiver confirm a remote rating's issuer actually traded here.
// RemoteReceiptHash links an origin's rating evidence to the serving kernel's execution receipt.
// Signed under the evidence_receipt domain (§12).
type EvidenceReceipt struct {
	ReceiptHash                 string    `json:"receipt_hash"`
	SubjectKernelPublicKey      string    `json:"subject_kernel_public_key"`
	SubjectActionID             string    `json:"subject_action_id"`
	CounterpartyKernelPublicKey string    `json:"counterparty_kernel_public_key,omitempty"`
	Status                      TxStatus  `json:"status"`
	StartedAt                   time.Time `json:"started_at"`
	CreatedAt                   time.Time `json:"created_at"`
	RemoteReceiptHash           string    `json:"remote_receipt_hash,omitempty"`
	Signature                   string    `json:"signature"`
}

// GossipRequest is the (cursored) request payload for /juice/fed/gossip/1. An empty cursor
// starts the evidence stream at the oldest retained item.
type GossipRequest struct {
	Cursor string `json:"cursor,omitempty"`
	// CatalogCursor is the last action id of the catalogue page this requester received; empty
	// opens a new scan (P9).
	CatalogCursor string `json:"catalog_cursor,omitempty"`
}

// RatingEvidence is the wire projection of a Rating (§13): the public reputation signal only —
// value, note, timestamp, and the receipt link — with no rater or transaction identity. The
// gossiping kernel signs it under sigDomainRating; the full Rating never crosses the wire.
type RatingEvidence struct {
	Rating           float64   `json:"rating"`
	Note             *string   `json:"note"`
	RatedReceiptHash string    `json:"rated_receipt_hash"`
	CreatedAt        time.Time `json:"created_at"`
	Signature        string    `json:"signature"`
}

// EvidenceBundle is one retained receipt, optionally with the rating projection referencing it (§13).
type EvidenceBundle struct {
	EvidenceReceipt *EvidenceReceipt `json:"evidence_receipt"`
	Rating          *RatingEvidence  `json:"rating,omitempty"`
}

// GossipResponse is the v0.13 gossip payload. It carries the full first-party catalog snapshot
// (identity, own signed manifests) on every response, plus one page of evidence bundles
// ordered by effective time (a rating's created_at when rated, else the receipt's) so a late
// rating re-surfaces its bundle. NextCursor is the exclusive high-watermark to send on the next pull.
type GossipResponse struct {
	PublicKey string `json:"public_key"`
	Handle    string `json:"handle"`
	// Network and NetworkFingerprint name the world this kernel serves. A reply from another network is
	// not accumulated: its artifacts could never verify here anyway (P9, D23).
	Network            string `json:"network,omitempty"`
	NetworkFingerprint string `json:"network_fingerprint,omitempty"`
	// BlockchainAddress is where this kernel is paid, with BlockchainProof its own rail key's signature over it.
	BlockchainAddress string            `json:"blockchain_address,omitempty"`
	BlockchainProof   string            `json:"blockchain_proof,omitempty"`
	About             string            `json:"about,omitempty"` // @sys's description: the kernel's self-description (§13)
	ActionManifests   []*ActionManifest `json:"action_manifests,omitempty"`
	Evidence          []EvidenceBundle  `json:"evidence,omitempty"`
	NextCursor        string            `json:"next_cursor,omitempty"`
	// NextCatalogCursor is the last action id of this page, empty when the page ends the catalogue
	// — which is what completes the requester's scan (P9).
	NextCatalogCursor string `json:"next_catalog_cursor,omitempty"`
}

// GossipReceiptRow is one gossip-eligible trade as the evidence sender needs it (§13) — the
// projection's own fields and nothing else: the receipt's stored canonical hash, what it says
// happened and when, whom it was with, the remote receipt a proxy call settled on, and the rating
// that names it. It is deliberately not a receipt: everything a receipt holds besides this is
// private to the two parties (P9), and reading it to hash what the row already stores would
// compute one fact twice. Ordered by EffectiveAt, so a late rating re-surfaces its bundle.
type GossipReceiptRow struct {
	ReceiptHash                 string
	Status                      TxStatus
	StartedAt                   time.Time
	CreatedAt                   time.Time
	SubjectKernelPublicKey      string
	SubjectActionID             string
	CounterpartyKernelPublicKey string
	RemoteReceiptJSON           string
	// Rating is the wire projection the kernel will sign, unsigned as it comes from the store.
	Rating      *RatingEvidence
	EffectiveAt time.Time
	Cursor      string // the (effective_at, receipt id) high-watermark AFTER this row
}

// observe widens the row's retained window to include one more receipt.
func (r *SubjectEvidenceRow) observe(at time.Time) {
	if r.Since.IsZero() || at.Before(r.Since) {
		r.Since = at
	}
	if at.After(r.Until) {
		r.Until = at
	}
}

// EvidenceRow is one persisted, verified evidence record in the regenerable evidence cache (§13).
// It is keyed by (IssuerPublicKey, ReceiptHash) and truncatable with zero semantic effect.
type EvidenceRow struct {
	IssuerPublicKey             string
	ReceiptHash                 string
	SubjectKernelPublicKey      string
	SubjectActionID             string
	CounterpartyKernelPublicKey string
	EvidenceReceiptJSON         string
	RatingJSON                  string
	RemoteReceiptHash           string
	ReceiptCreatedAt            time.Time
	EffectiveAt                 time.Time
	ObservedAt                  time.Time
	Equivocated                 bool
}

// DiscoveryDoc is one regenerable, searchable discovery record (§13): a remote action summary
// learned first-party from gossip and indexed by the lookup machinery. It carries no execution
// semantics and is truncatable with zero effect (a lookup selection still resolves and verifies
// from the home kernel). ActionID is the remote action's stable id and the schemas mirror the
// signed manifest; Handle is the owner's, for rendering owner@kernel/name.
type DiscoveryDoc struct {
	KernelPublicKey string         `json:"kernel_public_key"`
	Handle          string         `json:"handle,omitempty"`
	Description     string         `json:"description,omitempty"`
	ActionID        string         `json:"action_id,omitempty"` // remote action id
	Name            string         `json:"name,omitempty"`
	InputSchema     map[string]any `json:"input_schema,omitempty"`
	OutputSchema    map[string]any `json:"output_schema,omitempty"`
	// ServingPrice is the manifest's price plus the peer's signed serving markup,
	// mp + ceil(mp·remote_bps/10000) (§13). The local all-in price adds import_bps at read time,
	// so a policy change reprices the catalog with no re-pull. Untagged: admin inspect serializes
	// these docs directly and its shape is not part of this record.
	ServingPrice int64     `json:"-"`
	Embedding    []float32 `json:"-"`
	// Generation is the catalogue scan that last mentioned this doc. A completed scan sweeps
	// everything it did not mention, which is how a withdrawn action leaves the index without a
	// replace-all that an empty page could turn into an erasure.
	Generation int64     `json:"-"`
	ObservedAt time.Time `json:"observed_at"`
}

// SubjectEvidenceRow is one issuer's derived retained-evidence metrics about a subject action (§13),
// as surfaced by `admin inspect`. RatingCount/RatingMean cover only trade-backed ratings (the
// counterparty and hashes join); UnverifiedRatings counts issuer-attested ratings whose two-kernel
// link could not be confirmed locally. Uses/Successes/Failures/latency are this issuer's own
// interactions with the subject: the execution summary when issuer==subject, else the issuer's
// counterparty-experience row — the two views are shown separately and never summed (§13).
// CorroboratedUses counts, for a counterparty-experience row (issuer!=subject), the interactions
// whose two-kernel link holds — the subject's own execution evidence names this issuer as
// counterparty and the receipt hashes join — so an issuer's claim is shown verified vs unverified,
// not taken on faith. It is 0 for the self-reported execution summary.
type SubjectEvidenceRow struct {
	IssuerPublicKey   string  `json:"issuer_public_key"`
	SubjectActionID   string  `json:"subject_action_id"`
	Uses              int64   `json:"uses"`
	Successes         int64   `json:"successes"`
	Failures          int64   `json:"failures"`
	CorroboratedUses  int64   `json:"corroborated_uses"`
	AvgLatencyMs      float64 `json:"avg_latency_ms"`
	RatingCount       int64   `json:"rating_count"`
	RatingMean        float64 `json:"rating_mean"`
	UnverifiedRatings int64   `json:"unverified_ratings"`
	// Contradictions counts trades where this issuer's row links to the subject's own row — same
	// receipt, same action, this issuer named as counterparty — and the two report different
	// outcomes. A linked pair that disagrees is not corroboration: it is two signed statements that
	// cannot both be true, so it is counted here and never in CorroboratedUses (§13).
	Contradictions int64 `json:"contradictions"`
	// Equivocations counts trades this issuer told two stories about: two different signed
	// statements under one receipt. Such a trade counts for nothing else — not a use, not an
	// outcome, not a rating — so without this number a reader could not tell a party that
	// contradicts itself from one that never traded [→U39].
	Equivocations int64 `json:"equivocations"`
	// Since and Until bound the retained sample: the oldest and newest receipt counted here. The
	// retention cap means a row may describe a window rather than a lifetime, so a reader is given
	// the window rather than left to assume there is none.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	Notes []string  `json:"notes,omitempty"`
}

// FederationResult is the return value of ExecuteFederation.
// ReceiptJSON is non-empty when the remote kernel included a receipt in its response
// (at any HTTP status — rejection and failure receipts arrive on non-200).
// A zero FederationResult (empty ReceiptJSON) means the call is pending: the caller
// should retain the trace for retry.
type FederationResult struct {
	Result      map[string]any
	ReceiptJSON string
	HTTPStatus  int
	// NotDispatched is true when the transport provably never sent the request (resolve/connect
	// failed before any byte was written). Only the first dispatch may act on it (§13 never-
	// dispatched); the retry path ignores it. Set by the executor, so kernel never imports fed.
	NotDispatched bool
}
