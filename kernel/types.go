package kernel

import (
	"crypto/ed25519"
	"time"
)

// ActionKind describes how an action is executed.
type ActionKind string

const (
	KindHTTP   ActionKind = "http"
	KindWasm   ActionKind = "wasm"
	KindNative ActionKind = "native"
)

// Permission names an ACL right.
type Permission string

const (
	PermRead  Permission = "read"
	PermCall  Permission = "call"
	PermAdmin Permission = "admin"
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
	ID            string
	Handle        string
	Email         string
	PasswordHash  string
	Available     int64
	Locked        int64
	SuspendedAt   *time.Time
	PublicKey     string // Ed25519 public key, base64url; empty for local users
	RemoteBaseURL string // HTTP API base URL of the remote kernel; empty for local users
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Action is a callable capability.
type Action struct {
	ID           string
	OwnerUserID  string
	Name         string
	Kind         ActionKind
	Active       bool
	Public       bool
	Price        int64
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Source       string // URL for http; WAT/WASM source for wasm
	ArtifactHash string // content-addressed compiled WASM artifact
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time // nil unless soft-deleted
}

// ACLEntry grants a permission to a subject over an action.
type ACLEntry struct {
	SubjectUserID string
	ActionID      string
	Permission    Permission
	CreatedAt     time.Time
}

// Process is a budgeted execution context.
type Process struct {
	ID          string
	OwnerUserID string
	Available   int64
	Locked      int64
	Status      ProcessStatus
	CreatedAt   time.Time
	EndedAt     *time.Time
}

// Trace records causal structure for one step in a call tree.
// Root traces have ParentTraceID == ID.
// CausedByTraceID is a FOLLOWS_FROM reference set for event-triggered calls;
// it references the emitting action's trace and may cross process boundaries.
type Trace struct {
	ID              string
	ProcessID       string
	ParentTraceID   string
	CausedByTraceID *string
	Cost            int64
	LatencyMS       int64
	CreatedAt       time.Time
}

// Transaction records one attempted call. Immutable after creation.
type Transaction struct {
	ID                 string
	ProcessID          string
	TraceID            string
	ParentTraceID      string
	OwnerUserID        string
	SubjectUserID      string
	TargetUserID       string
	ActionID           string
	ArgsJSON           string
	ReplyJSON          string
	Status             TxStatus
	Gross              int64
	Net                int64
	Fee                int64
	Reason             string
	RemoteReceiptHash  string // SHA-256 of the remote receipt JSON for cross-kernel calls; empty for local
	StartedAt          time.Time
	EndedAt            time.Time
}

// Stats tracks fixed performance and usage statistics for an action.
type Stats struct {
	ActionID    string
	Uses        int64
	Successes   int64
	Failures    int64
	RatingCount int64 // number of rated observations (may be less than Uses)
	PriceMean   float64
	LatencyMean float64
	RatingMean  float64
	LastUsedAt  time.Time
}

// StatTag is an extensible key/value annotation on an action's stats.
type StatTag struct {
	ActionID  string
	Key       string
	Value     string
	Source    string
	UpdatedAt time.Time
}

// Listener binds a named event to a kernel call.
type Listener struct {
	ID             string
	OwnerUserID    string
	SourceUserID   string
	EventName      string
	TargetActionID string
	Active         bool
	CreatedAt      time.Time
}

// Event is a queued occurrence of a named event for a specific listener.
// States: pending (ConsumedAt=nil, TxID=nil), in-flight (ConsumedAt set, TxID=nil),
// consumed (both set). In-flight events are reset to pending on bootstrap restart.
type Event struct {
	ID              string
	ListenerID      string
	ArgsJSON        string
	CausingTraceID  string
	ConsumedAt      *time.Time
	TxID            *string
	CreatedAt       time.Time
}

// Deposit is an admin credit grant to a user's available balance.
type Deposit struct {
	ID             string
	OperatorUserID string
	TargetUserID   string
	Amount         int64
	Reason         string
	CreatedAt      time.Time
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
	ID           string
	IssuerUserID string   // @sys user of this kernel
	TxID         string
	TraceID      string
	ActionID     string
	ArgsHash     string   // hex SHA-256 of args JSON
	ReplyHash    string   // hex SHA-256 of reply JSON
	Status       TxStatus
	Gross        int64
	Net          int64
	Fee          int64
	Reason       string
	CreatedAt    time.Time
	Signature    string   // base64url Ed25519 signature over canonical payload
}

// Rating is an immutable human-submitted rating for a transaction.
// Stored in a separate ratings table; the transaction row is never modified after creation.
type Rating struct {
	ID             string
	RatedTxID      string
	RatedReceiptID *string  // nil for transactions predating the receipt requirement
	RaterUserID    string
	Rating         float64  // 0 or 1
	CreatedAt      time.Time
	Signature      string   // base64url Ed25519 signature
}

// IdempotencyRecord prevents duplicate cross-kernel calls.
type IdempotencyRecord struct {
	ID                 string
	IdempotencyKey     string
	CounterpartyUserID string
	ReceiptID          *string
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// ActionManifest is a signed, exportable description of a public active action.
type ActionManifest struct {
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

// Ed25519 key type aliases for clarity at call sites.
type (
	Ed25519PrivateKey = ed25519.PrivateKey
	Ed25519PublicKey  = ed25519.PublicKey
)

