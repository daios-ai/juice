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
	Execute(ctx context.Context, action *Action, args map[string]any) (map[string]any, error)
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
	StepCreate(ctx context.Context, partialArgs, inputSchema []byte, requiredCallerUserID, nextActionID string) (string, error)
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

// ---- Persistence interface ----

// TxFilter narrows a ListTransactions query.
type TxFilter struct {
	OwnerUserID   string
	CallerUserID string
	TargetUserID  string
	ProcessID     string
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
	UpdateRemoteBaseURL(ctx context.Context, userID, baseURL string) error
	// ReadRemoteKernelByBaseURL returns the remote-kernel user with the given base URL.
	ReadRemoteKernelByBaseURL(ctx context.Context, baseURL string) (*User, error)
	// UpdateRemoteProxySourceURLs replaces oldBase with newBase in Action.source for all
	// remote_proxy actions owned by ownerUserID.
	UpdateRemoteProxySourceURLs(ctx context.Context, ownerUserID, oldBase, newBase string) error
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

	// CreateProcess atomically debits price from owner.available, credits owner.locked,
	// and creates the process with available=price. Returns ErrInsufficientFunds if
	// owner.available < price.
	CreateProcess(ctx context.Context, p *Process, ownerID string, price int64) error

	ReadProcess(ctx context.Context, id string) (*Process, error)
	ListProcesses(ctx context.Context, ownerID string, limit, offset int) ([]*Process, error)
	ListAllProcesses(ctx context.Context, limit, offset int) ([]*Process, error)

	// BeginRootCall atomically deducts price from process.available into process.locked
	// and creates the root trace with available=price.
	BeginRootCall(ctx context.Context, processID string, t *Trace, price int64) error

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
	// wallet, decrements owner.locked, records a failure transaction, creates its receipt,
	// updates trace latency, upserts action stats, completes the idempotency record (if
	// non-empty), and marks the step done (if non-empty).
	CommitFailedCall(ctx context.Context, tx *Transaction, receipt *Receipt, traceID, callerWalletID, callerWalletKind string, gross int64, stats *Stats, idempotencyRecordID, errorCode, stepID string) error

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
	ListSteps(ctx context.Context, callerUserID, processID, status string, isSuperuser bool) ([]*Step, error)
	// ResetStep resets a single running step (tx_id IS NULL) back to waiting. Called when Call
	// fails before creating a transaction — the step can be retried.
	ResetStep(ctx context.Context, stepID string) error
	// ResetStepAndRepark re-parks a step's price and resets to waiting. Used when the
	// completion trace is empty (crash during execution) to prevent double-completion minting.
	ResetStepAndRepark(ctx context.Context, stepID string) error
	// ListOrphanRunningStepIDs returns IDs of running steps with a completion trace but no tx.
	ListOrphanRunningStepIDs(ctx context.Context) ([]string, error)
	// ResetRunningSteps sets status=waiting where status=running AND tx_id IS NULL.
	ResetRunningSteps(ctx context.Context) error
	// ListOrphanTraces returns traces that have no associated transaction, ordered deepest-first
	// (longest parent chain first). Used by recovery to settle interrupted calls.
	ListOrphanTraces(ctx context.Context) ([]*Trace, error)

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

	// ---- Deposits / Withdrawals ----

	CreateDeposit(ctx context.Context, d *Deposit) error
	// CreateWithdrawal atomically debits amount from target.available and records the withdrawal.
	// Returns ErrInsufficientFunds if target.available < amount.
	CreateWithdrawal(ctx context.Context, w *Withdrawal) error

	// ---- Embeddings ----

	// UpsertEmbedding stores a pre-computed embedding vector for an action.
	UpsertEmbedding(ctx context.Context, actionID string, vec []float32) error
	// ListEmbeddings returns stored embedding vectors keyed by action ID,
	// filtered to active, public, non-deleted actions only.
	ListEmbeddings(ctx context.Context) (map[string][]float32, error)

	// ---- Users (extended) ----

	// DenyUser sets denied_at on the user (unfriend operation).
	DenyUser(ctx context.Context, id string) error
	// UndenyUser clears denied_at on the user.
	UndenyUser(ctx context.Context, id string) error
	// CreateProxyUser creates a proxy user record (Kind is inferred from empty PasswordHash +
	// non-empty PublicKey + non-empty RemoteBaseURL). Does not create a password.
	CreateProxyUser(ctx context.Context, u *User) error

	// ---- Gossip / Federation ----

	// CreateOrUpdateDiscoveredKernel upserts a DiscoveredKernel row keyed by (public_key, introduced_by).
	CreateOrUpdateDiscoveredKernel(ctx context.Context, k *DiscoveredKernel) error
	// ListDiscoveredKernels returns all discovered kernel rows.
	ListDiscoveredKernels(ctx context.Context) ([]*DiscoveredKernel, error)

	// ---- Peer lifecycle (used by DenyPeer / UndenyPeer) ----

	// DeactivateActionsOwnedBy sets active=false for all non-deleted actions owned by ownerUserID.
	DeactivateActionsOwnedBy(ctx context.Context, ownerUserID string) error
	// ActivateActionsOwnedBy sets active=true for all non-deleted actions owned by ownerUserID.
	ActivateActionsOwnedBy(ctx context.Context, ownerUserID string) error
	// CancelAndRefundStepsForCaller cancels all waiting steps where required_caller_user_id=callerUserID,
	// atomically refunding each step's parked price to its parent trace (available+=price, locked-=price).
	CancelAndRefundStepsForCaller(ctx context.Context, callerUserID string) error
	// ListStatsByOwner returns Stats rows for actions owned by ownerUserID that have uses > 0.
	// Used by GetGossip to identify transacted friends.
	ListStatsByOwner(ctx context.Context, ownerUserID string) ([]*Stats, error)
}
