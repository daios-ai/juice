package kernel

import (
	"crypto/ed25519"
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

// User is an authenticated subject with balances.
// A user with PublicKey and RemoteBaseURL set represents a remote kernel peer.
type User struct {
	ID            string     `json:"id"`
	Handle        string     `json:"handle"`
	Email         string     `json:"email"`
	PasswordHash  string     `json:"-"`
	Available     int64      `json:"available"`
	Locked        int64      `json:"locked"`
	SuspendedAt   *time.Time `json:"suspended_at,omitempty"`
	DeniedAt      *time.Time `json:"denied_at,omitempty"`
	PublicKey     string     `json:"public_key,omitempty"`      // Ed25519 public key, base64url; empty for local users
	RemoteBaseURL string     `json:"remote_base_url,omitempty"` // HTTP API base URL of the remote kernel; empty for local users
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Action is a callable capability.
type Action struct {
	ID             string         `json:"id"`
	OwnerUserID    string         `json:"owner_user_id"`
	OwnerHandle    string         `json:"owner_handle,omitempty"` // populated via JOIN; empty if not loaded
	Name           string         `json:"name"`
	Kind           ActionKind     `json:"kind"`
	Active         bool           `json:"active"`
	Public         bool           `json:"public"`
	Price          int64          `json:"price"`
	Description    string         `json:"description"`
	InputSchema    map[string]any `json:"input_schema"`
	OutputSchema   map[string]any `json:"output_schema"`
	Source         string         `json:"source,omitempty"`           // URL for http; TinyGo source for wasm; federation URL for remote_proxy
	ArtifactHash   string         `json:"artifact_hash,omitempty"`    // content-addressed compiled WASM artifact
	WasmArtifact   string         `json:"wasm_artifact,omitempty"`    // base64-encoded compiled WASM bytes (wasm only); Source holds the TinyGo text
	RemoteActionID string         `json:"remote_action_id,omitempty"` // ID of the action on the remote kernel (remote_proxy only)
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      *time.Time     `json:"deleted_at,omitempty"`
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
// Core invariant: CompleteStep(caller, id, input) = Call(caller, process_id, next_action_id, partial_args ⊕ input)
type Step struct {
	ID                   string          `json:"id"`
	ProcessID            string          `json:"process_id"`
	ParentTraceID        *string         `json:"parent_trace_id,omitempty"`
	RequiredCallerUserID string          `json:"required_caller_user_id"`
	NextActionID         string          `json:"next_action_id"`
	PartialArgs          json.RawMessage `json:"partial_args"`
	InputSchema          json.RawMessage `json:"input_schema"`
	Price                int64           `json:"price"`
	Status               StepStatus      `json:"status"`
	TxID                 *string         `json:"tx_id,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
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
	ID            string    `json:"id"`
	ProcessID     string    `json:"process_id"`
	ParentTraceID *string   `json:"parent_trace_id,omitempty"`
	ActionOwnerID string    `json:"action_owner_id"`
	Available     int64     `json:"available"`
	Locked        int64     `json:"locked"`
	LatencyMS     int64     `json:"latency_ms"`
	CreatedAt     time.Time `json:"created_at"`
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

// Deposit is an admin credit grant to a user's available balance.
type Deposit struct {
	ID             string    `json:"id"`
	OperatorUserID string    `json:"operator_user_id"`
	TargetUserID   string    `json:"target_user_id"`
	Amount         int64     `json:"amount"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}

// Withdrawal is an admin debit from a user's available balance.
type Withdrawal struct {
	ID             string    `json:"id"`
	OperatorUserID string    `json:"operator_user_id"`
	TargetUserID   string    `json:"target_user_id"`
	Amount         int64     `json:"amount"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}

// DiscoveredKernel is a remote kernel learned via gossip.
type DiscoveredKernel struct {
	PublicKey    string          `json:"public_key"`
	IntroducedBy string          `json:"introduced_by"`
	Handle       string          `json:"handle"`
	BaseURL      string          `json:"base_url"`
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
	Reason       string    `json:"reason"`
	StartedAt    time.Time `json:"started_at"`
	CreatedAt    time.Time `json:"created_at"`
	Signature    string    `json:"signature"`
}

// Rating is an immutable human-submitted rating for a transaction.
// Stored in a separate ratings table; the transaction row is never modified after creation.
type Rating struct {
	ID             string    `json:"id"`
	RatedTxID      string    `json:"rated_tx_id"`
	RatedReceiptID *string   `json:"rated_receipt_id"` // nil for transactions predating the receipt requirement
	RaterUserID    string    `json:"rater_user_id"`
	Rating         float64   `json:"rating"`
	Note           *string   `json:"note,omitempty"` // optional human-readable justification
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
	Updated     []*Action   // deactivated: contract changed
	Deactivated []*Action   // deactivated: removed from spec
	Rejected    []ImportRejection
}

// ImportRejection records one operation that could not be imported.
type ImportRejection struct {
	Key    string // operation_key or action name
	Reason string
}

// OpenAPIParam records where one input field is sent in the HTTP request.
type OpenAPIParam struct {
	Name string `json:"name"`
	In   string `json:"in"` // "path", "query", or "body"
}

// OpenAPISource is the provenance stored in Action.Source for OpenAPI-imported actions.
type OpenAPISource struct {
	Type              string         `json:"type"`
	SpecURL           string         `json:"spec_url"`
	BaseURL           string         `json:"base_url"`
	Method            string         `json:"method"`
	Path              string         `json:"path"`
	OperationKey      string         `json:"operation_key"`
	OperationHash     string         `json:"operation_hash"`
	Params            []OpenAPIParam `json:"params,omitempty"`
	OwnershipVerified bool           `json:"ownership_verified,omitempty"`
}

// ActionManifest is a signed, exportable description of a public active action.
type ActionManifest struct {
	ActionID     string         `json:"action_id"`
	OwnerHandle  string         `json:"owner_handle"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
	Price        int64          `json:"price"`
	Kind         ActionKind     `json:"kind"`
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
	Charge             bool `json:"charge"`              // amount paid to proxy == receipt.gross
	SettlementArith    bool `json:"settlement_arith"`    // net + fee == gross (internal receipt math)
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

// PeerView is a peer kernel in the friendship list.
type PeerView struct {
	Handle    string     `json:"handle"`
	BaseURL   string     `json:"base_url"`
	PublicKey string     `json:"public_key"`
	DeniedAt  *time.Time `json:"denied_at,omitempty"`
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
	BaseURL   string         `json:"base_url"`
	PublicKey string         `json:"public_key"`
	Actions   []GossipAction `json:"actions"`
}

// GossipResponse is the payload returned by GET /v1/gossip.
type GossipResponse struct {
	PublicKey string             `json:"public_key"`
	Handle    string             `json:"handle"`
	BaseURL   string             `json:"base_url"`
	Actions   []GossipAction     `json:"actions"`
	Friends   []GossipFriendView `json:"friends"`
}

// Ed25519PrivateKey is a type alias for clarity at call sites.
type Ed25519PrivateKey = ed25519.PrivateKey

