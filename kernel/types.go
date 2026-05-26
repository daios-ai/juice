package kernel

import "time"

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
type User struct {
	ID           string
	Handle       string
	Email        string
	PasswordHash string
	Available    int64
	Locked       int64
	SuspendedAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
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

// Transaction records one attempted call.
type Transaction struct {
	ID            string
	ProcessID     string
	TraceID       string
	ParentTraceID string
	OwnerUserID   string
	SubjectUserID string
	TargetUserID  string
	ActionID      string
	ArgsJSON      string
	ReplyJSON     string
	Status        TxStatus
	Gross         int64
	Net           int64
	Fee           int64
	Reason        string
	StartedAt     time.Time
	EndedAt       time.Time
	Rating        *float64
}

// Stats tracks fixed performance and usage statistics for an action.
type Stats struct {
	ActionID    string
	Uses        int64
	Successes   int64
	Failures    int64
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
	ProcessID      string
	TraceID        string
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

// TraceFeedback holds recursive cost and latency for a trace node.
// Deprecated: use Trace.Cost and Trace.LatencyMS instead.
type TraceFeedback struct {
	TraceID          string
	RecursiveCost    int64
	RecursiveLatency float64 // seconds
}

