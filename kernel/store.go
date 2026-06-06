package kernel

import "context"

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

// HTTPExecutor calls an external HTTP action endpoint.
// kernel/ defines this interface; cmd/juice provides the concrete implementation.
type HTTPExecutor interface {
	Execute(ctx context.Context, source string, args map[string]any) (map[string]any, error)
}

// URLFetcher retrieves the body of a URL. Used for OpenAPI ownership proof (well-known challenge).
// HTTPExecutor implementations may optionally implement this interface; kernel checks via type assertion.
type URLFetcher interface {
	FetchURL(ctx context.Context, rawURL string) ([]byte, error)
}

// FederationExecutor sends a cross-kernel call to a remote proxy target.
// HTTPExecutor implementations may optionally implement this interface; kernel checks via type assertion.
type FederationExecutor interface {
	ExecuteFederation(ctx context.Context, source, idempotencyKey string, args map[string]any) (result map[string]any, receiptJSON string, err error)
}

// HostFunctions are the callbacks available to a running script.
type HostFunctions interface {
	Call(ctx context.Context, actionName string, args []byte) ([]byte, error)
	Emit(ctx context.Context, event string, args []byte) error
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

// ---- Persistence interface ----

// TxFilter narrows a ListTransactions query.
type TxFilter struct {
	OwnerUserID   string
	CallerUserID string
	TargetUserID  string
	ProcessID     string
	// PartyUserID matches transactions where the user is a party: buyer
	// (owner_user_id) or seller (owner of the called action). See §9.4.
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
	UpdateRemoteBaseURL(ctx context.Context, userID, baseURL string) error
	ListUsers(ctx context.Context, limit, offset int) ([]*User, error)
	SuspendUser(ctx context.Context, id string) error
	UnsuspendUser(ctx context.Context, id string) error

	// ---- Actions ----

	CreateAction(ctx context.Context, a *Action) error
	ReadAction(ctx context.Context, id string) (*Action, error)
	ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*Action, error)
	ReadActionByOwnerRemoteID(ctx context.Context, ownerID, remoteActionID string) (*Action, error)
	// ListActionsByOwnerOpenAPISpec returns all non-deleted actions with matching owner + OpenAPI spec_url.
	ListActionsByOwnerOpenAPISpec(ctx context.Context, ownerID, specURL string) ([]*Action, error)
	UpdateAction(ctx context.Context, a *Action) error
	DeleteAction(ctx context.Context, id string) error
	ListPublicActions(ctx context.Context, limit, offset int) ([]*Action, error)
	// ListActionsByOwner returns all non-deleted actions owned by ownerID, including
	// inactive and private ones. Used to give an owner their full private view.
	ListActionsByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*Action, error)
	ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error)

	// ---- Processes ----

	// StartProcess atomically creates the process, debits owner funds, and creates the root trace.
	// All three writes occur in a single SQLite transaction. Either all succeed or none do.
	StartProcess(ctx context.Context, p *Process, t *Trace, ownerID string, funds int64) error

	ReadProcess(ctx context.Context, id string) (*Process, error)
	ListProcesses(ctx context.Context, ownerID string, limit, offset int) ([]*Process, error)
	ListAllProcesses(ctx context.Context, limit, offset int) ([]*Process, error)

	// GrantProcessAuthority grants explicit call authority over a process to a caller.
	GrantProcessAuthority(ctx context.Context, callerID, processID string) error
	// RevokeProcessAuthority removes explicit call authority from a caller.
	RevokeProcessAuthority(ctx context.Context, callerID, processID string) error
	// CheckProcessAuthority returns true if callerID has explicit authority over processID.
	CheckProcessAuthority(ctx context.Context, callerID, processID string) (bool, error)

	// BeginCall atomically locks price credits in the process and creates the child trace.
	// Either both succeed or neither does. Returns ErrInsufficientFunds if the process
	// has insufficient available credits; any other error is a store failure.
	BeginCall(ctx context.Context, processID string, t *Trace, price int64) error

	// RefundFunds moves `amount` back from process.locked to process.available.
	RefundFunds(ctx context.Context, processID string, amount int64) error

	// FundProcess moves `amount` from user.available to process.available.
	// Fails atomically if user.available < amount.
	FundProcess(ctx context.Context, userID, processID string, amount int64) error

	// CommitCall atomically records a successful transaction, creates its receipt, settles funds,
	// updates trace cost/latency for all ancestor traces, upserts action stats, settles the event
	// (if eventID is non-empty), and completes the idempotency record (if idempotencyRecordID is
	// non-empty) — all in one SQLite transaction.
	CommitCall(ctx context.Context, tx *Transaction, receipt *Receipt, processID, targetUserID, feeRecipientID string, net, fee int64, stats *Stats, eventID, idempotencyRecordID string) error

	// CommitFailedCall atomically refunds locked funds, records a failure transaction, creates its receipt,
	// updates trace latency, upserts action stats, and completes the idempotency record (if
	// idempotencyRecordID is non-empty) — all in one SQLite transaction.
	CommitFailedCall(ctx context.Context, tx *Transaction, receipt *Receipt, processID string, gross int64, stats *Stats, idempotencyRecordID, errorCode string) error

	// EndProcess closes the process and returns all remaining funds to the owner.
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
	// CreateRatingAndUpdateStats atomically inserts a rating and updates rating_count/rating_mean.
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

	// ---- Listeners & Events ----

	CreateListener(ctx context.Context, l *Listener) error
	ReadListener(ctx context.Context, id string) (*Listener, error)
	ListListeners(ctx context.Context, sourceUserID, eventName string) ([]*Listener, error)
	ListListenersByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*Listener, error)
	// CreateEvents inserts all events in a single atomic transaction.
	// Either all events are created or none are.
	CreateEvents(ctx context.Context, events []*Event) error
	ReadEvent(ctx context.Context, id string) (*Event, error)
	ListPendingEvents(ctx context.Context, listenerID string) ([]*Event, error)
	// LockEvent atomically marks an event as in-flight (sets consumed_at).
	// Returns ErrInvalidState if the event is already consumed or in-flight.
	LockEvent(ctx context.Context, eventID string) error
	// UnlockEvent resets an in-flight event back to pending on consume failure.
	UnlockEvent(ctx context.Context, eventID string) error
	// DeleteListenerWithEvents atomically deactivates a listener and purges its pending events.
	DeleteListenerWithEvents(ctx context.Context, listenerID string) error
	// ResetInFlightEvents resets all in-flight events (consumed_at set, tx_id null)
	// back to pending. Called at startup to recover from crashed consume calls.
	ResetInFlightEvents(ctx context.Context) error
	// ResetInFlightCalls restores locked process funds to available.
	// Called at startup to recover from calls that crashed before settlement.
	ResetInFlightCalls(ctx context.Context) error

	// ---- Traces (by process) ----

	ListTraces(ctx context.Context, processID string) ([]*Trace, error)

	// ---- Auth codes (PKCE flow) ----

	CreateAuthCode(ctx context.Context, c *AuthCode) error
	ConsumeAuthCode(ctx context.Context, code string) (*AuthCode, error)

	// ---- Refresh tokens ----

	CreateRefreshToken(ctx context.Context, t *RefreshToken) error
	RotateRefreshToken(ctx context.Context, oldToken string) (*RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, token string) error

	// ---- Config ----

	GetConfig(ctx context.Context, key string) (string, error)
	SetConfig(ctx context.Context, key, value string) error

	// InitFirstBoot atomically creates a user and sets all given config entries.
	// If the user handle already exists the user INSERT is skipped; config entries are always set.
	InitFirstBoot(ctx context.Context, u *User, configs map[string]string) error

	// ---- Receipts (by ID) ----

	// ReadReceipt returns the receipt with the given ID.
	ReadReceipt(ctx context.Context, id string) (*Receipt, error)

	// ---- Deposits ----

	CreateDeposit(ctx context.Context, d *Deposit) error

	// ---- Embeddings ----

	// UpsertEmbedding stores a pre-computed embedding vector for an action.
	UpsertEmbedding(ctx context.Context, actionID string, vec []float32) error
	// ListEmbeddings returns stored embedding vectors keyed by action ID,
	// filtered to active, public, non-deleted actions only.
	ListEmbeddings(ctx context.Context) (map[string][]float32, error)

	// ---- Federation ----

	// UpdateRemoteProxySourceURLs replaces oldBase with newBase in Action.source for all
	// remote_proxy actions owned by ownerUserID. Used when a remote peer's base URL changes.
	UpdateRemoteProxySourceURLs(ctx context.Context, ownerUserID, oldBase, newBase string) error

	// ReadRemoteKernelByBaseURL returns the remote-kernel user with the given base URL,
	// or ErrNotFound if no such user exists.
	ReadRemoteKernelByBaseURL(ctx context.Context, baseURL string) (*User, error)
}
