package kernel

import (
	"context"
	"time"
)

// ---- Script execution interfaces ----

// ScriptExecutor compiles and runs WebAssembly scripts.
// kernel/ defines this interface; script/ provides the wazero implementation.
type ScriptExecutor interface {
	Compile(ctx context.Context, source []byte) (artifact []byte, hash string, err error)
	Execute(ctx context.Context, artifact []byte, input []byte, host HostFunctions) ([]byte, error)
}

// HostFunctions are the callbacks available to a running script.
type HostFunctions interface {
	Call(ctx context.Context, actionName string, args []byte) ([]byte, error)
	Emit(ctx context.Context, event string, args []byte) error
	Log(ctx context.Context, level, msg string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, value []byte) error
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

// ---- Persistence interface ----

// TxFilter narrows a ListTransactions query.
type TxFilter struct {
	OwnerUserID   string
	SubjectUserID string
	TargetUserID  string
	ProcessID     string
	Limit         int
	Offset        int
}

// Store is the persistence interface for all kernel objects.
// All monetary transitions are executed atomically inside the store.
// kernel/ must not import any concrete store implementation.
type Store interface {
	// ---- Users ----

	CreateUser(ctx context.Context, u *User) error
	ReadUser(ctx context.Context, id string) (*User, error)
	ReadUserByHandle(ctx context.Context, handle string) (*User, error)
	ListUsers(ctx context.Context, limit, offset int) ([]*User, error)
	SuspendUser(ctx context.Context, id string) error
	UnsuspendUser(ctx context.Context, id string) error

	// ---- Actions ----

	CreateAction(ctx context.Context, a *Action) error
	ReadAction(ctx context.Context, id string) (*Action, error)
	ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*Action, error)
	UpdateAction(ctx context.Context, a *Action) error
	DeleteAction(ctx context.Context, id string) error
	ListActions(ctx context.Context, activeOnly bool, limit, offset int) ([]*Action, error)
	ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error)

	// ---- ACL ----

	GrantACL(ctx context.Context, e *ACLEntry) error
	RevokeACL(ctx context.Context, subjectID, actionID string, perm Permission) error
	CheckACL(ctx context.Context, subjectID, actionID string, perm Permission) (bool, error)
	GrantAll(ctx context.Context, actionID string) error
	RevokeAll(ctx context.Context, actionID string) error
	CheckGrantAll(ctx context.Context, actionID string) (bool, error)

	// ---- Processes ----

	CreateProcess(ctx context.Context, p *Process) error
	ReadProcess(ctx context.Context, id string) (*Process, error)
	ListAllProcesses(ctx context.Context, limit, offset int) ([]*Process, error)

	// LockFunds moves `amount` from process.available to process.locked.
	// Fails atomically if process.available < amount.
	LockFunds(ctx context.Context, processID string, amount int64) error

	// RefundFunds moves `amount` back from process.locked to process.available.
	RefundFunds(ctx context.Context, processID string, amount int64) error

	// FundProcess moves `amount` from user.available to process.available.
	// Fails atomically if user.available < amount.
	FundProcess(ctx context.Context, userID, processID string, amount int64) error

	// SettleCall debits gross from process.locked and owner.locked,
	// credits net to targetUser.available and fee to feeRecipient.available.
	SettleCall(ctx context.Context, processID, targetUserID, feeRecipientID string, net, fee int64) error

	// EndProcess closes the process and returns all remaining funds to the owner.
	EndProcess(ctx context.Context, processID string) error

	// ---- Traces ----

	CreateTrace(ctx context.Context, t *Trace) error
	ReadTrace(ctx context.Context, id string) (*Trace, error)

	// ---- Transactions ----

	CreateTransaction(ctx context.Context, tx *Transaction) error
	UpdateTransaction(ctx context.Context, tx *Transaction) error
	ReadTransaction(ctx context.Context, id string) (*Transaction, error)
	ListTransactions(ctx context.Context, filter TxFilter) ([]*Transaction, error)
	ListAllTransactions(ctx context.Context, limit, offset int) ([]*Transaction, error)
	UpdateTraceCostLatency(ctx context.Context, traceID string, grossDelta int64, endedAt time.Time) error
	CascadeRating(ctx context.Context, traceID string, rating float64) error

	// ---- Stats ----

	ReadStats(ctx context.Context, actionID string) (*Stats, error)
	UpsertStats(ctx context.Context, s *Stats) error
	UpsertStatTag(ctx context.Context, tag *StatTag) error

	// ---- Listeners & Events ----

	CreateListener(ctx context.Context, l *Listener) error
	ReadListener(ctx context.Context, id string) (*Listener, error)
	UpdateListener(ctx context.Context, l *Listener) error
	ListListeners(ctx context.Context, sourceUserID, eventName string) ([]*Listener, error)
	AppendEvent(ctx context.Context, listenerID, txID string) error
	ReadEvents(ctx context.Context, listenerID string) ([]string, error)

	// ---- Traces (by process) ----

	ListTraces(ctx context.Context, processID string) ([]*Trace, error)

	// ---- Auth codes (PKCE flow) ----

	CreateAuthCode(ctx context.Context, c *AuthCode) error
	ConsumeAuthCode(ctx context.Context, code string) (*AuthCode, error)

	// ---- Refresh tokens ----

	CreateRefreshToken(ctx context.Context, t *RefreshToken) error
	RotateRefreshToken(ctx context.Context, oldToken string) (*RefreshToken, error)

	// ---- Config ----

	GetConfig(ctx context.Context, key string) (string, error)
	SetConfig(ctx context.Context, key, value string) error
}
