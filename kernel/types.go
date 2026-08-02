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

// User is an account with balances, distinguished only by the credentials it holds — a password
// (session) and/or a PublicKey (federation signature); there is no "kind". A key makes it a peer
// kernel here, its live path resolved from the key by the transport (§13). RecoveryPublicKey is a
// distinct recovery credential (§12): an account's own key for signing a password-reset challenge,
// never a federation identity, so it must not be confused with PublicKey (which sets IsPeer).
type User struct {
	ID           string     `json:"id"`
	Handle       string     `json:"handle"`
	Description  string     `json:"description"` // free-text "about"; @sys's is the kernel's about (§13)
	PasswordHash string     `json:"-"`
	Available    int64      `json:"available"`
	Locked       int64      `json:"locked"`
	SuspendedAt  *time.Time `json:"suspended_at,omitempty"`
	PublicKey    string     `json:"public_key,omitempty"` // Ed25519 public key, base64url; empty = no signature credential
	// RecoveryPublicKey is the account's own Ed25519 recovery key (base64url), enrolled at
	// creation from a client-held seed phrase; the server stores only the public half and never
	// the mnemonic (§12). Distinct from PublicKey: it does not make the account a peer.
	RecoveryPublicKey string `json:"-"`
	// PeerLastSeen and PeerCredit are the friend-sync cache (§13 peer sync): null except on peer
	// rows. Display-only — never callability, pricing, or settlement. PeerCredit is our cached
	// credit *on* the peer, valid as of PeerLastSeen.
	PeerLastSeen *time.Time `json:"peer_last_seen,omitempty"`
	PeerCredit   *int64     `json:"peer_credit,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// IsPeer reports whether u is a remote-kernel proxy user, identified by a set public_key (§13).
// A peer authenticates by federation signature and is denied local-visibility actions (§4).
func (u *User) IsPeer() bool { return u != nil && u.PublicKey != "" }

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
	Effect         string           `json:"effect,omitempty"`           // signed manifest contract: a privileged execution effect ("transfer", §13); empty = ordinary action
	AuthJSON       string           `json:"-"`                          // AES-256-GCM encrypted upstream auth credentials; never serialized
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	DeletedAt      *time.Time       `json:"deleted_at,omitempty"`
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
	ID                   string          `json:"id"`
	ParentTraceID        *string         `json:"parent_trace_id,omitempty"`
	RequiredCallerUserID string          `json:"required_caller_user_id"`
	RequiredCallerRemoteID *string       `json:"required_caller_remote_id,omitempty"` // stable remote user_id on the peer kernel (§13); nil = local required caller
	ActionID             string          `json:"action_id"`
	PartialArgs          json.RawMessage `json:"partial_args"`
	Price                int64           `json:"price"`
	Status               StepStatus      `json:"status"`
	TxID                 *string         `json:"tx_id,omitempty"`
	CompletionTraceID    *string         `json:"completion_trace_id,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
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
	IdempotencyKey string
	Beneficiary    string // the beneficiary user_id the serving kernel binds in its receipt
	Amount         int64
	Reserve        int64 // buyer's max_total: amount + value_premium + value_import
	Status         string
	CreatedAt      time.Time
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

// StatTag is an extensible key/value annotation on an action's stats.
type StatTag struct {
	ActionID  string    `json:"action_id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updated_at"`
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

// DiscoveredKernel is a remote kernel learned via gossip.
type DiscoveredKernel struct {
	PublicKey    string          `json:"public_key"`
	IntroducedBy string          `json:"introduced_by"`
	Handle       string          `json:"handle"`
	StatsJSON    json.RawMessage `json:"stats_json"`
	FirstSeen    time.Time       `json:"first_seen"`
	UpdatedAt    time.Time       `json:"updated_at"`
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
	ID           string    `json:"id"`
	IssuerUserID string    `json:"issuer_user_id"`
	TxID         string    `json:"tx_id"`
	TraceID      string    `json:"trace_id"`
	ActionID     string    `json:"action_id"`
	CallerUserID string    `json:"caller_user_id"`
	ProcessID    string    `json:"process_id"`
	ArgsHash     string    `json:"args_hash"`
	ReplyHash    string    `json:"reply_hash"`
	Status       TxStatus  `json:"status"`
	Gross        int64     `json:"gross"`
	Net          int64     `json:"net"`
	Fee          int64     `json:"fee"`
	Charge       int64     `json:"charge"`
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
	Value        int64     `json:"value,omitempty"`
	ValuePremium int64     `json:"value_premium,omitempty"`
	ValueTo      string    `json:"value_to,omitempty"`
	Reason       string    `json:"reason"`
	StartedAt time.Time `json:"started_at"`
	CreatedAt time.Time `json:"created_at"`
	Signature string    `json:"signature"`
}

// Rating is an immutable human-submitted rating for a transaction.
// Stored in a separate ratings table; the transaction row is never modified after creation.
type Rating struct {
	ID             string    `json:"id"`
	RatedTxID      string    `json:"rated_tx_id"`
	RatedReceiptID *string   `json:"rated_receipt_id"` // nil for transactions predating the receipt requirement
	RaterUserID    string    `json:"rater_user_id"`
	Rating         float64   `json:"rating"`
	Note           *string   `json:"note"` // optional human-readable justification
	CreatedAt      time.Time `json:"created_at"`
	Signature      string    `json:"signature"`
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
// Used by both OpenAPI import and federation (remote) import.
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
	TransactionID         string        `json:"transaction_id"`
	Valid                 bool          `json:"valid"`
	RemoteKernelHandle    string        `json:"remote_kernel_handle"`
	RemoteKernelPublicKey string        `json:"remote_kernel_public_key"`
	Checks                ReceiptChecks `json:"checks"`
	Receipt               *Receipt      `json:"receipt"`
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

// PeerView is a known peer kernel — identified by handle and public key (the global name), with
// its bilateral balance. It carries no internal user id: a peer is never addressed by one.
type PeerView struct {
	Handle      string     `json:"handle"`
	PublicKey   string     `json:"public_key"`
	Available   int64      `json:"available"`
	Locked      int64      `json:"locked"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
	// PeerCredit and LastSeen are the peer-sync cache (§13 peer sync): our credit on the peer
	// and when we last reached it. Display-only.
	PeerCredit *int64     `json:"peer_credit,omitempty"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	// SettlementDue flags a debtor peer when this kernel's global gross receivables have reached the
	// settlement trigger Y (§13): information for the operator, never authority — computed live, display
	// only. FIX 3: Y signals; the operator runs `admin settle`.
	SettlementDue bool `json:"settlement_due,omitempty"`
}

// GossipAction is an action entry in a gossip response.
type GossipAction struct {
	ActionID    string  `json:"action_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       int64   `json:"price"`
	Uses        int64   `json:"uses"`
	Rating      float64 `json:"rating"`
}

// GossipFriendView is a transacted friend in a gossip response: a peer kernel this kernel
// has settled calls with, including this kernel's locally earned stats for their actions.
type GossipFriendView struct {
	Handle    string         `json:"handle"`
	PublicKey string         `json:"public_key"`
	Actions   []GossipAction `json:"actions"`
}

// GossipResponse is the payload returned by GET /v1/gossip.
type GossipResponse struct {
	PublicKey string             `json:"public_key"`
	Handle    string             `json:"handle"`
	About     string             `json:"about,omitempty"` // @sys's description: the kernel's self-description (§13)
	Actions   []GossipAction     `json:"actions"`
	Friends   []GossipFriendView `json:"friends"`
	// CounterpartyBalance is the requesting peer's credit on this kernel (§13 peer sync),
	// set only for a friended, non-denied requester; nil otherwise. Information, never authority.
	CounterpartyBalance *int64 `json:"counterparty_balance,omitempty"`
}

// KernelRoster is one entry of the known-network directory (§13): a discovered kernel grouped with
// every introducer's report of it and, when we have friended and transacted with it, our own earned
// stats. Display only — the known network grants no callability, pricing, or settlement.
type KernelRoster struct {
	PublicKey string         `json:"public_key"`
	Handle    string         `json:"handle"`
	Own       []GossipAction `json:"own,omitempty"` // our own earned stats (ground truth), if transacted
	Sources   []RosterSource `json:"sources"`       // one per introducer, self-report or hearsay
}

// RosterSource is one introducer's gossiped view of a kernel's actions, namespaced by who told us.
type RosterSource struct {
	IntroducedBy string         `json:"introduced_by"`
	SelfReported bool           `json:"self_reported"` // IntroducedBy == the kernel's own key
	Actions      []GossipAction `json:"actions"`
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
