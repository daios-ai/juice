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
	RecoveryPublicKey string    `json:"-"`
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
// RefreshToken is populated only on legacy rows awaiting the backfill (§8) and is otherwise
// empty; it is AES-256-GCM sealed and write-only, never serialized by any read path.
type Grant struct {
	ID            string    `json:"id"`
	GrantorUserID string    `json:"grantor_user_id"`
	ActionID      string    `json:"action_id"`
	ConnectionID  string    `json:"-"` // FK to Connection (empty on unbackfilled legacy rows)
	RefreshToken  string    `json:"-"` // legacy sealed token, backfill-only; cleared once linked
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
	Action      string    `json:"action"` // @owner/name
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
	ID                     string          `json:"id"`
	ParentTraceID          *string         `json:"parent_trace_id,omitempty"`
	RequiredCallerUserID   string          `json:"required_caller_user_id"`
	RequiredCallerRemoteID *string         `json:"required_caller_remote_id,omitempty"` // stable remote user_id on the peer kernel (§13); nil = local required caller
	ActionID               string          `json:"action_id"`
	PartialArgs            json.RawMessage `json:"partial_args"`
	Price                  int64           `json:"price"`
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
	ID             string  `json:"id"`
	ProcessID      string  `json:"process_id"`
	ParentTraceID  *string `json:"parent_trace_id,omitempty"`
	ActionOwnerID  string  `json:"action_owner_id"`
	ActionID       string  `json:"action_id"`
	CallerUserID   string  `json:"caller_user_id"`
	Available      int64   `json:"available"`
	Locked         int64   `json:"locked"`
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
	DispatchJSON   *string `json:"dispatch_json,omitempty"`
	// IdempotencyRecordID is the inbound cross-kernel record this trace serves (§13), set only on a
	// root call made on a peer's behalf. Whichever settlement resolves the trace completes that
	// record, so a crashed or parked federated call never strands its requester.
	IdempotencyRecordID *string `json:"idempotency_record_id,omitempty"`
	// PremiumBPS and PremiumParked snapshot the serving-markup admitted for an inbound federated root
	// call (§13): the rate the receipt levies on the actual charge, and the reserve parked in the
	// owner's locked at admission. Persisting them on the trace lets EVERY settlement path — commit,
	// failure, crash recovery, forced closure — release the reserve without the in-memory request,
	// and pins the rate against a mid-call config change. 0 on local calls and subcalls.
	PremiumBPS    int64 `json:"premium_bps,omitempty"`
	PremiumParked int64 `json:"premium_parked,omitempty"`
	// Value, ValueTo, ValueReserve snapshot a TransferEffect on a call whose caller C funds a transfer
	// (§13): the delivered amount, the resolved local-beneficiary user id (empty for a remote/outbound
	// destination), and the total reserve locked from C.available at admission (value + value fees).
	// Released to the beneficiary/peer + sys + refund at settlement, or refunded on failure — sourced
	// from C, not the trace budget. All 0/"" on every non-transfer call.
	Value        int64     `json:"value,omitempty"`
	ValueTo      string    `json:"value_to,omitempty"`
	ValueReserve int64     `json:"value_reserve,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ValueSettlement describes how an outbound remote settlement disposes of the value-transfer reserve
// locked on the caller C (§13). It is computed by settleRemoteCall from the peer's signed receipt and
// applied atomically inside CommitRemoteSettlement. A zero Reserve means the call carried no transfer.
// Exactly one disposition applies: Quarantine leaves the reserve LOCKED (an invalid/inconsistent
// receipt after a possibly-executed dispatch — never auto-refund); Refund returns the whole reserve to
// C (a valid failure/rejection); otherwise the reserve settles — Credit (value+value_premium) to the
// peer proxy row, SysCredit (value_import) to origin sys, remainder refunded to C.
type ValueSettlement struct {
	Reserve    int64
	Quarantine bool
	Refund     bool
	Credit     int64
	SysCredit  int64
}

// PendingTransfer is the buyer-side reserve holder for a remote payment Step (§13): the buyer funds a
// TransferEffect attached to a Step hosted on another kernel, so the reserve lives here rather than on a
// fabricated local trace. Status ∈ {pending, settled, refunded, quarantined}. IdempotencyKey is unique
// and payment-bound, so a retry presents the same key and never double-funds.
type PendingTransfer struct {
	ID             string
	BuyerID        string
	PeerKey        string
	StepID         string
	InputHash      string
	Input          json.RawMessage // raw completion input bytes, so a retry rebuilds the SAME signed request
	IdempotencyKey string
	Beneficiary    string // the beneficiary user_id the serving kernel binds in its receipt
	Amount         int64
	RemoteMax      int64 // descriptor obligation (amount + value_premium); settlement re-validates against it
	Reserve        int64 // buyer's max_total: amount + value_premium + value_import
	Status         string
	LastError      string // disposition reason (e.g. why quarantined), for the operator
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Transaction records one attempted call. Immutable after creation.
type Transaction struct {
	ID                string          `json:"id"`
	ProcessID         string          `json:"process_id"`
	TraceID           string          `json:"trace_id"`
	ParentTraceID     string          `json:"parent_trace_id"`
	OwnerUserID       string          `json:"owner_user_id"`
	CallerUserID      string          `json:"caller_user_id"`
	TargetUserID      string          `json:"target_user_id"`
	ActionID          string          `json:"action_id"`
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
	StartedAt         time.Time       `json:"started_at"`
	EndedAt           time.Time       `json:"ended_at"`
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

// SettlementRecord is the creditor-signed evidence of one residual settlement (§13). An *open*
// record carries only the commitment H(s); a *final* record adds the revealed secret, the debtor's
// nonce, and the outcome. It is JCS-signed by the creditor over all fields with Signature="", and its
// key-set (creditor+debtor+quantum+commitment) is disjoint from every other signed payload (§12).
type SettlementRecord struct {
	SettlementID string    `json:"settlement_id"`
	Creditor     string    `json:"creditor"`          // creditor kernel public key (base64url)
	Debtor       string    `json:"debtor"`            // debtor kernel public key (base64url)
	Amount       int64     `json:"amount"`            // d, the residual debt being settled
	Quantum      int64     `json:"quantum"`           // Q, the creditor's fee-rational quantum
	Mode         string    `json:"mode"`              // "probabilistic"
	Commitment   string    `json:"commitment"`        // SHA-256(secret) hex — binds the creditor before the nonce
	Nonce        string    `json:"nonce,omitempty"`   // final only: the debtor's committed nonce
	Secret       string    `json:"secret,omitempty"`  // final only: revealed secret s (hex)
	Outcome      string    `json:"outcome,omitempty"` // final only: "pay" | "clear"
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
	Signature    string    `json:"signature"`
}

// RemoteKernel is a known remote kernel: one row per public key, whether or not it holds an
// account here (§3). It owns Stiegler naming state — the self-certifying key, the Nickname the
// remote asserts about itself (never resolves a reference), and the Petname assigned locally
// (resolves). GossipCursor is the persisted evidence high-watermark (§13 peer sync), advanced only
// after a page is verified and committed, so it is never written by ordinary observation.
// LastSeen and PeerCredit are the peer-sync display cache, written only after a successful
// authenticated sync.
type RemoteKernel struct {
	PublicKey    string     `json:"public_key"`
	Petname      string     `json:"petname,omitempty"`
	Nickname     string     `json:"nickname,omitempty"`
	About        string     `json:"about,omitempty"`
	GossipCursor string     `json:"gossip_cursor,omitempty"`
	LastSeen     *time.Time `json:"last_seen,omitempty"`
	PeerCredit   *int64     `json:"peer_credit,omitempty"`
	FirstSeen    time.Time  `json:"first_seen"`
	UpdatedAt    time.Time  `json:"updated_at"`
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
	// Value / ValuePremium / ValueTo are the transfer channel, kept distinct from the execution channel
	// (Charge/Premium) so the two never mix (§13). Value is the delivered amount — all-or-nothing (0 or
	// the requested amount, so a partial-charge failure never dilutes delivery); ValuePremium is the
	// serving markup on the value (= ceil(value·remote_bps), NOT folded into execution Premium); ValueTo
	// is the resolved beneficiary the origin binds. omitempty keeps all three out of the JCS signature
	// for every non-transfer receipt (0/""), so those verify unchanged.
	Value        int64  `json:"value,omitempty"`
	ValuePremium int64  `json:"value_premium,omitempty"`
	ValueTo      string `json:"value_to,omitempty"`
	// RefreshProxy signals the origin to invalidate its cached proxy for this action (§8/§13): set only
	// on a zero-charge pre-execution rejection whose fault is the cache's (contract-hash mismatch, a
	// non-executable action). omitempty keeps it out of the JCS signature for every other receipt, so
	// those verify unchanged; a receipt setting it must be a valid zero-charge rejection or it quarantines.
	RefreshProxy bool      `json:"refresh_proxy,omitempty"`
	Reason       string    `json:"reason"`
	StartedAt    time.Time `json:"started_at"`
	CreatedAt    time.Time `json:"created_at"`
	Signature    string    `json:"signature"`
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

// TransactionView is a Transaction with its associated rating embedded.
// Rating is null when the transaction has not been rated.
type TransactionView struct {
	*Transaction
	Rating *EmbeddedRating `json:"rating"`
}

// IdempotencyRecord prevents duplicate cross-kernel calls.
// Status transitions: "pending" (inserted before execution) → "complete" (set after success or failure).
type IdempotencyRecord struct {
	ID                 string
	IdempotencyKey     string
	CounterpartyUserID string
	ReceiptID          *string
	Status             string // "pending" | "complete"
	ResultJSON         string // JSON-encoded result, set on completion
	ReceiptJSON        string // JSON of the receipt, stored for idempotent replay
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// ImportResult summarises the outcome of an import operation.
// Used by both OpenAPI import and per-call remote manifest resolution.
type ImportResult struct {
	Created     []*Action
	Unchanged   []*Action
	Updated     []*Action // deactivated: contract changed
	Deactivated []*Action // deactivated: removed from spec
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
// fields (SpecURL, OperationKey, OperationHash, OwnershipVerified) are empty for
// manual actions, and import reconciliation is scoped to Type=="openapi" rows.
type HTTPSource struct {
	Type              string      `json:"type"`
	SpecURL           string      `json:"spec_url,omitempty"`
	BaseURL           string      `json:"base_url"`
	Method            string      `json:"method"`
	Path              string      `json:"path"`
	OperationKey      string      `json:"operation_key,omitempty"`
	OperationHash     string      `json:"operation_hash,omitempty"`
	Params            []HTTPParam `json:"params,omitempty"`
	OwnershipVerified bool        `json:"ownership_verified,omitempty"`
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
	Effect       string         `json:"effect,omitempty"` // privileged execution effect ("transfer"); signed so the origin decides value-bearing from the contract, not a name (§13)
	ArtifactHash string         `json:"artifact_hash"`
	UpdatedAt    time.Time      `json:"updated_at"`
	Stats        *Stats         `json:"stats"`
	Signature    string         `json:"signature"` // base64url Ed25519 signature
}

// ReceiptVerification is the result of VerifyRemoteReceipt.
type ReceiptVerification struct {
	TransactionID         string `json:"transaction_id"`
	Valid                 bool   `json:"valid"`
	RemoteKernelHandle    string `json:"remote_kernel_handle"`
	RemoteKernelPublicKey string `json:"remote_kernel_public_key"`
	// SignatureVersion is which signing scheme verified the stored receipt: 2 = v0.13
	// domain-prefixed, 1 = legacy undomained (a pre-v0.13 audit record, still authentic), 0 = none.
	SignatureVersion int           `json:"signature_version"`
	Checks           ReceiptChecks `json:"checks"`
	Receipt          *Receipt      `json:"receipt"`
}

// ReceiptChecks holds the per-field results of a remote receipt verification.
type ReceiptChecks struct {
	ReceiptHash        bool `json:"receipt_hash"`
	Signature          bool `json:"signature"`
	ActionID           bool `json:"action_id"`
	Status             bool `json:"status"`
	Charge             bool `json:"charge"`              // execution obligation (tx.net) == receipt.charge + receipt.premium
	Premium            bool `json:"premium"`             // receipt.premium == ceil(receipt.charge * remote_bps / 10000)
	ValuePremium       bool `json:"value_premium"`       // receipt.value_premium == ceil(receipt.value * remote_bps / 10000) (§13)
	SettlementArith    bool `json:"settlement_arith"`    // tx.fee == ceil(tx.net * import_bps / 10000) on success, 0 on failure
	ChargeCeiling      bool `json:"charge_ceiling"`      // receipt.charge + receipt.premium <= tx.gross, the authenticated ceiling (§13)
	RefundConservation bool `json:"refund_conservation"` // tx.Refund == tx.Gross - tx.Net - tx.Fee (exact equality)
	ArgsHash           bool `json:"args_hash"`
	ReplyHash          bool `json:"reply_hash"`
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
	// HasAccount is true iff a bilateral financial account exists here. It is false for a
	// discovery-only kernel, whose Available/Locked/PeerCredit are meaningless.
	HasAccount bool `json:"has_account"`
	// Actions is the kernel's cached public-action count (from discovery docs), 0 when unknown.
	Actions     int        `json:"actions"`
	Available   int64      `json:"available"`
	Locked      int64      `json:"locked"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
	// PeerCredit and LastSeen are the peer-sync cache (§13 peer sync): our credit on the peer
	// and when we last reached it. Display-only.
	PeerCredit *int64     `json:"peer_credit,omitempty"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	// SettlementDue flags a debtor peer when this kernel's global gross receivables have reached the
	// settlement trigger Y (§13): information for the operator, never authority — computed live.
	SettlementDue bool `json:"settlement_due,omitempty"`
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
}

// GossipUser is a first-party user summary in a gossip response: @sys and the owners of active
// public actions, the searchable identities a peer indexes into its discovery docs (§13).
type GossipUser struct {
	UserID      string `json:"user_id"`
	Handle      string `json:"handle"`
	Description string `json:"description,omitempty"`
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
// (identity, users, own signed manifests) on every response, plus one page of evidence bundles
// ordered by effective time (a rating's created_at when rated, else the receipt's) so a late
// rating re-surfaces its bundle. NextCursor is the exclusive high-watermark to send on the next pull.
type GossipResponse struct {
	PublicKey       string            `json:"public_key"`
	Handle          string            `json:"handle"`
	About           string            `json:"about,omitempty"` // @sys's description: the kernel's self-description (§13)
	Users           []GossipUser      `json:"users,omitempty"`
	ActionManifests []*ActionManifest `json:"action_manifests,omitempty"`
	Evidence        []EvidenceBundle  `json:"evidence,omitempty"`
	NextCursor      string            `json:"next_cursor,omitempty"`
	// CounterpartyBalance is the requesting peer's credit on this kernel (§13 peer sync),
	// set only for a known non-suspended requester; nil otherwise. Information, never authority.
	CounterpartyBalance *int64 `json:"counterparty_balance,omitempty"`
}

// GossipReceiptRow is one gossip-eligible receipt row assembled by the evidence sender (§13): the
// stored receipt, its transaction facts needed to build the projection (subject, counterparty,
// timestamps), the joined rating (if any), and the stored remote receipt JSON (for a proxy call).
// Ordered by EffectiveAt so a late rating re-surfaces its bundle.
type GossipReceiptRow struct {
	Receipt                     *Receipt
	SubjectKernelPublicKey      string
	SubjectActionID             string
	CounterpartyKernelPublicKey string
	RemoteReceiptJSON           string
	// IdempotencyKey is the outbound proxy trace's key (empty for own-execution leg-(a) rows). A
	// signed rejection receipt sets its tx_id to this key, so leg (b) distinguishes a genuine
	// admitted execution from a rejection at any price, including 0 (§13 gossip-eligibility).
	IdempotencyKey string
	Rating         *Rating
	EffectiveAt    time.Time
	Cursor         string // the (effective_at, receipt_hash) high-watermark AFTER this row
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

// DiscoveryDoc is one regenerable, searchable discovery record (§13): a user or action summary
// learned first-party from gossip and indexed by the lookup machinery. It carries no execution
// semantics and is truncatable with zero effect (a lookup selection still resolves and verifies
// from the home kernel). For Kind=="action", ActionID is the remote action's stable id and the
// schemas mirror the signed manifest; for Kind=="user", UserID/Handle/Description summarize the
// remote principal.
type DiscoveryDoc struct {
	KernelPublicKey string         `json:"kernel_public_key"`
	Kind            string         `json:"kind"` // "user" | "action"
	UserID          string         `json:"user_id,omitempty"`
	Handle          string         `json:"handle,omitempty"`
	Description     string         `json:"description,omitempty"`
	ActionID        string         `json:"action_id,omitempty"` // remote action id (Kind=="action")
	Name            string         `json:"name,omitempty"`
	InputSchema     map[string]any `json:"input_schema,omitempty"`
	OutputSchema    map[string]any `json:"output_schema,omitempty"`
	// ServingPrice is the manifest's price plus the peer's signed serving markup,
	// mp + ceil(mp·remote_bps/10000) (§13). The local all-in price adds import_bps at read time,
	// so a policy change reprices the catalog with no re-pull. Untagged: admin inspect serializes
	// these docs directly and its shape is not part of this record.
	ServingPrice int64     `json:"-"`
	Embedding    []float32 `json:"-"`
	ObservedAt   time.Time `json:"observed_at"`
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
	IssuerPublicKey   string   `json:"issuer_public_key"`
	SubjectActionID   string   `json:"subject_action_id"`
	Uses              int64    `json:"uses"`
	Successes         int64    `json:"successes"`
	Failures          int64    `json:"failures"`
	CorroboratedUses  int64    `json:"corroborated_uses"`
	AvgLatencyMs      float64  `json:"avg_latency_ms"`
	RatingCount       int64    `json:"rating_count"`
	RatingMean        float64  `json:"rating_mean"`
	UnverifiedRatings int64    `json:"unverified_ratings"`
	Notes             []string `json:"notes,omitempty"`
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
