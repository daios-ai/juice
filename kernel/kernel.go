package kernel

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

// Config holds kernel-level configuration.
type Config struct {
	FeeBPS         int64         // basis points, e.g. 2000 = 20%
	FeeRecipientID string        // user ID that receives fees
	TokenSecret    string        // HMAC secret for JWT signing
	TokenTTL       time.Duration // token validity window
	ScriptTimeout  time.Duration
	ScriptMemory   int64 // bytes
}

// DefaultConfig returns safe local defaults.
func DefaultConfig() Config {
	return Config{
		FeeBPS:       2000,
		TokenTTL:     15 * time.Minute,
		ScriptTimeout: 10 * time.Second,
		ScriptMemory:  64 * 1024 * 1024, // 64 MiB
	}
}

// Kernel is the central service object.
// It holds all dependencies and exposes operations to both the CLI and HTTP server.
type Kernel struct {
	store   Store
	scripts ScriptExecutor
	llm     Embedder
	cfg     Config
	log     *log.Logger
}

// New constructs a Kernel. scripts and llm may be nil if those features are unused.
func New(store Store, scripts ScriptExecutor, llm Embedder, cfg Config, logger *log.Logger) *Kernel {
	if logger == nil {
		logger = log.Default()
	}
	return &Kernel{store: store, scripts: scripts, llm: llm, cfg: cfg, log: logger}
}

// ---- User operations ----

// CreateUserRequest holds validated input for user creation.
type CreateUserRequest struct {
	Handle   string
	Email    string
	Password string
}

// CreateUser creates a new user account and returns the user.
func (k *Kernel) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	if req.Handle == "" {
		return nil, ErrInvalidInput.Wrap("handle is required")
	}
	if req.Email == "" {
		return nil, ErrInvalidInput.Wrap("email is required")
	}
	if req.Password == "" {
		return nil, ErrInvalidInput.Wrap("password is required")
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	u := &User{
		ID:           uuid.New().String(),
		Handle:       req.Handle,
		Email:        req.Email,
		PasswordHash: hash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := k.store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("user.created", "user_id", u.ID, "handle", u.Handle)
	return u, nil
}

// ReadUser returns the user with the given ID.
func (k *Kernel) ReadUser(ctx context.Context, id string) (*User, error) {
	return k.store.ReadUser(ctx, id)
}

// ReadUserByHandle returns the user with the given handle.
func (k *Kernel) ReadUserByHandle(ctx context.Context, handle string) (*User, error) {
	return k.store.ReadUserByHandle(ctx, handle)
}

// Login authenticates handle+password and returns a signed JWT.
func (k *Kernel) Login(ctx context.Context, handle, password string) (string, error) {
	u, err := k.store.ReadUserByHandle(ctx, handle)
	if err != nil {
		return "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if !CheckPassword(password, u.PasswordHash) {
		return "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if u.SuspendedAt != nil {
		return "", ErrUnauthenticated.Wrap("account suspended")
	}
	tok, err := IssueToken(u.ID, k.cfg.TokenSecret, k.cfg.TokenTTL)
	if err != nil {
		return "", err
	}
	k.log.With(ctx).Info("user.login", "user_id", u.ID)
	return tok, nil
}

// ListUsers returns all users ordered by creation time.
func (k *Kernel) ListUsers(ctx context.Context, limit, offset int) ([]*User, error) {
	return k.store.ListUsers(ctx, limit, offset)
}

// SuspendUser marks the user as suspended, preventing login.
func (k *Kernel) SuspendUser(ctx context.Context, targetID string) error {
	if err := k.store.SuspendUser(ctx, targetID); err != nil {
		return err
	}
	k.log.With(ctx).Info("user.suspended", "target_id", targetID)
	return nil
}

// UnsuspendUser removes the suspension from a user.
func (k *Kernel) UnsuspendUser(ctx context.Context, targetID string) error {
	if err := k.store.UnsuspendUser(ctx, targetID); err != nil {
		return err
	}
	k.log.With(ctx).Info("user.unsuspended", "target_id", targetID)
	return nil
}

// Deposit adds credits directly to a user's available balance and records an audit entry.
// The operatorID is stored for audit; superuser enforcement is the caller's responsibility.
func (k *Kernel) Deposit(ctx context.Context, operatorID, targetUserID string, amount int64, reason string) (*Deposit, error) {
	if amount <= 0 {
		return nil, ErrInvalidInput.Wrap("amount must be positive")
	}
	if _, err := k.store.ReadUser(ctx, targetUserID); err != nil {
		return nil, err
	}
	d := &Deposit{
		ID:             uuid.New().String(),
		OperatorUserID: operatorID,
		TargetUserID:   targetUserID,
		Amount:         amount,
		Reason:         reason,
		CreatedAt:      time.Now().UTC(),
	}
	if err := k.store.CreateDeposit(ctx, d); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("deposit.created", "deposit_id", d.ID, "target_user_id", targetUserID, "amount", amount)
	return d, nil
}

// VerifyToken validates a bearer token and returns the subject user ID.
func (k *Kernel) VerifyToken(token string) (string, error) {
	return VerifyToken(token, k.cfg.TokenSecret)
}

// ---- Action operations ----

// CreateActionRequest holds validated input for action creation.
type CreateActionRequest struct {
	OwnerUserID  string
	Name         string
	Kind         ActionKind
	Price        int64
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Source       string
}

// CreateAction registers a new action (inactive by default).
// validateHTTPSource rejects URLs that could be used for SSRF attacks.
// Allowed: http and https schemes with public hostnames or IPs.
// Rejected: other schemes, localhost, loopback, private, and link-local addresses.
func validateHTTPSource(source string) error {
	u, err := url.Parse(source)
	if err != nil {
		return ErrInvalidInput.Wrapf("invalid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidInput.Wrap("URL scheme must be http or https")
	}
	// Bare (unbracketed) IPv6 in a URL is malformed and may be an SSRF probe.
	// Go 1.25's url.splitHostPort misparses "::1" as host=":" port="1", so we
	// must catch this before calling Hostname().
	if strings.Count(u.Host, ":") > 1 && !strings.HasPrefix(u.Host, "[") {
		return ErrInvalidInput.Wrap("URL must not target private or reserved addresses")
	}
	host := u.Hostname()
	if host == "" {
		return ErrInvalidInput.Wrap("URL must have a host")
	}
	if strings.EqualFold(host, "localhost") {
		return ErrInvalidInput.Wrap("URL must not target localhost")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return ErrInvalidInput.Wrap("URL must not target private or reserved addresses")
		}
	}
	return nil
}

func (k *Kernel) CreateAction(ctx context.Context, req CreateActionRequest) (*Action, error) {
	if req.Name == "" {
		return nil, ErrInvalidInput.Wrap("name is required")
	}
	if req.Kind != KindHTTP && req.Kind != KindWasm && req.Kind != KindNative {
		return nil, ErrInvalidInput.Wrapf("unknown kind %q", req.Kind)
	}
	if req.Kind == KindNative {
		return nil, ErrUnauthorized.Wrap("native actions may only be registered by the kernel")
	}
	if req.Price < 0 {
		return nil, ErrInvalidInput.Wrap("price must be non-negative")
	}
	if req.Kind == KindHTTP && req.Source != "" {
		if err := validateHTTPSource(req.Source); err != nil {
			return nil, err
		}
	}
	if err := ValidateSchema(req.InputSchema); err != nil {
		return nil, err
	}
	if err := ValidateSchema(req.OutputSchema); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  req.OwnerUserID,
		Name:         req.Name,
		Kind:         req.Kind,
		Active:       false,
		Price:        req.Price,
		Description:  req.Description,
		InputSchema:  req.InputSchema,
		OutputSchema: req.OutputSchema,
		Source:       req.Source,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if req.Kind == KindWasm && len(req.Source) > 0 && k.scripts != nil {
		artifact, hash, err := k.scripts.Compile(ctx, []byte(req.Source))
		if err != nil {
			return nil, ErrInvalidInput.Wrapf("wasm compilation failed: %v", err)
		}
		a.Source = string(artifact)
		a.ArtifactHash = hash
	}

	if err := k.store.CreateAction(ctx, a); err != nil {
		return nil, err
	}
	k.embedActionAsync(ctx, a)
	k.log.With(ctx).Info("action.created", "action_id", a.ID, "name", a.Name)
	return a, nil
}

// ResetInFlightEvents resets in-flight events to pending. Called at startup.
func (k *Kernel) ResetInFlightEvents(ctx context.Context) error {
	return k.store.ResetInFlightEvents(ctx)
}

// RegisterNativeAction creates and activates a native action for bootstrap use.
// Unlike CreateAction, it does not reject KindNative. Call only from bootstrap.
func (k *Kernel) RegisterNativeAction(ctx context.Context, req CreateActionRequest) (*Action, error) {
	now := time.Now().UTC()
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  req.OwnerUserID,
		Name:         req.Name,
		Kind:         KindNative,
		Active:       false,
		Price:        req.Price,
		Description:  req.Description,
		InputSchema:  req.InputSchema,
		OutputSchema: req.OutputSchema,
		Source:       "native",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := k.store.CreateAction(ctx, a); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("action.registered_native", "action_id", a.ID, "name", a.Name)
	return a, nil
}

// ReadAction returns the action with the given ID.
func (k *Kernel) ReadAction(ctx context.Context, id string) (*Action, error) {
	return k.store.ReadAction(ctx, id)
}

// ReadActionByOwnerName returns an action by (ownerID, name).
func (k *Kernel) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*Action, error) {
	return k.store.ReadActionByOwnerName(ctx, ownerID, name)
}

// ListActions returns public active actions (or all actions for owners).
func (k *Kernel) ListActions(ctx context.Context, activeOnly bool, limit, offset int) ([]*Action, error) {
	return k.store.ListActions(ctx, activeOnly, limit, offset)
}

// ListAllActions returns all actions regardless of active state.
func (k *Kernel) ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error) {
	return k.store.ListAllActions(ctx, limit, offset)
}

// ListAllProcesses returns all processes ordered by creation time.
func (k *Kernel) ListAllProcesses(ctx context.Context, limit, offset int) ([]*Process, error) {
	return k.store.ListAllProcesses(ctx, limit, offset)
}

// ListAllTransactions returns all transactions ordered by started_at.
func (k *Kernel) ListAllTransactions(ctx context.Context, limit, offset int) ([]*Transaction, error) {
	return k.store.ListAllTransactions(ctx, limit, offset)
}

// GrantAll sets the public flag on an action, allowing anyone to call it.
func (k *Kernel) GrantAll(ctx context.Context, subjectID, actionID string) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if err := k.requireAdmin(ctx, subjectID, a); err != nil {
		return err
	}
	if err := k.store.GrantAll(ctx, actionID); err != nil {
		return err
	}
	k.log.With(ctx).Info("action.grant_all", "action_id", actionID, "subject", subjectID)
	return nil
}

// RevokeAll clears the public flag on an action.
func (k *Kernel) RevokeAll(ctx context.Context, subjectID, actionID string) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if err := k.requireAdmin(ctx, subjectID, a); err != nil {
		return err
	}
	if err := k.store.RevokeAll(ctx, actionID); err != nil {
		return err
	}
	k.log.With(ctx).Info("action.revoke_all", "action_id", actionID, "subject", subjectID)
	return nil
}

// GetConfig returns a persistent config value by key.
func (k *Kernel) GetConfig(ctx context.Context, key string) (string, error) {
	return k.store.GetConfig(ctx, key)
}

// SetConfig stores a persistent config value.
func (k *Kernel) SetConfig(ctx context.Context, key, value string) error {
	return k.store.SetConfig(ctx, key, value)
}

// BootstrapSuperuser atomically creates the superuser account and registers the
// superuser handle in config. If the handle already exists the user INSERT is
// skipped and only the config key is (re-)set. Safe to call on every startup.
func (k *Kernel) BootstrapSuperuser(ctx context.Context, req CreateUserRequest, configKey string) (*User, error) {
	if req.Handle == "" {
		return nil, ErrInvalidInput.Wrap("handle is required")
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, ErrInvalidInput.Wrapf("could not hash password: %v", err)
	}
	now := time.Now().UTC()
	u := &User{
		ID:           uuid.New().String(),
		Handle:       req.Handle,
		Email:        req.Email,
		PasswordHash: hash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := k.store.InitSuperuser(ctx, u, configKey, req.Handle); err != nil {
		return nil, err
	}
	// Return the stored user (may differ from u if handle already existed).
	return k.store.ReadUserByHandle(ctx, req.Handle)
}

// UpdateActionRequest holds validated input for action updates.
type UpdateActionRequest struct {
	ID           string
	Price        *int64
	Description  *string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Source       *string
}

// UpdateAction modifies an action and deactivates it (schema/source changes require re-activation).
func (k *Kernel) UpdateAction(ctx context.Context, subjectID string, req UpdateActionRequest) (*Action, error) {
	a, err := k.store.ReadAction(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	if err := k.requireAdmin(ctx, subjectID, a); err != nil {
		return nil, err
	}

	if req.Price != nil {
		if *req.Price < 0 {
			return nil, ErrInvalidInput.Wrap("price must be non-negative")
		}
		a.Price = *req.Price
	}
	if req.Description != nil {
		a.Description = *req.Description
	}
	if req.InputSchema != nil {
		if err := ValidateSchema(req.InputSchema); err != nil {
			return nil, err
		}
		a.InputSchema = req.InputSchema
		a.Active = false
	}
	if req.OutputSchema != nil {
		if err := ValidateSchema(req.OutputSchema); err != nil {
			return nil, err
		}
		a.OutputSchema = req.OutputSchema
		a.Active = false
	}
	if req.Source != nil {
		a.Source = *req.Source
		a.Active = false
		if a.Kind == KindWasm && k.scripts != nil {
			artifact, hash, err := k.scripts.Compile(ctx, []byte(*req.Source))
			if err != nil {
				return nil, ErrInvalidInput.Wrapf("wasm compilation failed: %v", err)
			}
			a.Source = string(artifact)
			a.ArtifactHash = hash
		}
	}
	a.UpdatedAt = time.Now().UTC()

	if err := k.store.UpdateAction(ctx, a); err != nil {
		return nil, err
	}
	if req.Description != nil {
		k.embedActionAsync(ctx, a)
	}
	k.log.With(ctx).Info("action.updated", "action_id", a.ID)
	return a, nil
}

// SetActive activates or deactivates an action.
func (k *Kernel) SetActive(ctx context.Context, subjectID, actionID string, active bool) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if err := k.requireAdmin(ctx, subjectID, a); err != nil {
		return err
	}
	if active {
		if a.Source == "" && a.Kind != KindNative {
			return ErrInvalidState.Wrap("cannot activate action with no source")
		}
		if a.Kind == KindHTTP {
			if err := validateHTTPSource(a.Source); err != nil {
				return err
			}
		}
		// Ensure stats exist.
		stats, _ := k.store.ReadStats(ctx, actionID)
		if stats == nil {
			if err := k.store.UpsertStats(ctx, DefaultStats(actionID)); err != nil {
				return err
			}
		}
	}
	a.Active = active
	a.UpdatedAt = time.Now().UTC()
	if err := k.store.UpdateAction(ctx, a); err != nil {
		return err
	}
	event := "action.disabled"
	if active {
		event = "action.enabled"
	}
	k.log.With(ctx).Info(event, "action_id", actionID)
	return nil
}

// DeleteAction removes an action (marks deleted; keeps transaction history).
func (k *Kernel) DeleteAction(ctx context.Context, subjectID, actionID string) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if err := k.requireAdmin(ctx, subjectID, a); err != nil {
		return err
	}
	if err := k.store.DeleteAction(ctx, actionID); err != nil {
		return err
	}
	k.log.With(ctx).Info("action.deleted", "action_id", actionID)
	return nil
}

// ---- ACL operations ----

// GrantACL grants a permission to a subject on an action.
func (k *Kernel) GrantACL(ctx context.Context, subjectID, actionID string, perm Permission, grantorID string) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if err := k.requireAdmin(ctx, grantorID, a); err != nil {
		return err
	}
	if err := k.store.GrantACL(ctx, &ACLEntry{
		SubjectUserID: subjectID,
		ActionID:      actionID,
		Permission:    perm,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		return err
	}
	k.log.With(ctx).Info("acl.granted", "action_id", actionID, "subject", subjectID, "perm", perm)
	return nil
}

// RevokeACL removes a permission.
func (k *Kernel) RevokeACL(ctx context.Context, subjectID, actionID string, perm Permission, revokerID string) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if err := k.requireAdmin(ctx, revokerID, a); err != nil {
		return err
	}
	if err := k.store.RevokeACL(ctx, subjectID, actionID, perm); err != nil {
		return err
	}
	k.log.With(ctx).Info("acl.revoked", "action_id", actionID, "subject", subjectID, "perm", perm)
	return nil
}

// ---- Process operations ----

// StartProcess creates a new process and locks funds from the owner's account.
func (k *Kernel) StartProcess(ctx context.Context, ownerID string, funds int64) (*Process, *Trace, error) {
	if funds < 0 {
		return nil, nil, ErrInvalidInput.Wrap("funds must be non-negative")
	}

	now := time.Now().UTC()
	p := &Process{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Status:      ProcessOpen,
		CreatedAt:   now,
	}
	if err := k.store.CreateProcess(ctx, p); err != nil {
		return nil, nil, err
	}
	if funds > 0 {
		if err := k.store.FundProcess(ctx, ownerID, p.ID, funds); err != nil {
			return nil, nil, err
		}
		p.Available = funds
	}

	// Create root trace (ParentTraceID == ID).
	t := &Trace{
		ID:        uuid.New().String(),
		ProcessID: p.ID,
		CreatedAt: now,
	}
	t.ParentTraceID = t.ID
	if err := k.store.CreateTrace(ctx, t); err != nil {
		return nil, nil, err
	}

	k.log.With(ctx).Info("process.started", "process_id", p.ID, "owner", ownerID, "funds", funds)
	return p, t, nil
}

// FundProcess adds more credits to an existing open process.
func (k *Kernel) FundProcess(ctx context.Context, subjectID, processID string, funds int64) error {
	p, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return err
	}
	if p.Status != ProcessOpen {
		return ErrInvalidState.Wrap("process is closed")
	}
	if p.OwnerUserID != subjectID {
		return ErrUnauthorized.Wrap("only the process owner may add funds")
	}
	if funds <= 0 {
		return ErrInvalidInput.Wrap("funds must be positive")
	}
	if err := k.store.FundProcess(ctx, subjectID, processID, funds); err != nil {
		return err
	}
	k.log.With(ctx).Info("process.funded", "process_id", processID, "funds", funds)
	return nil
}

// EndProcess closes a process and returns all remaining funds to the owner.
func (k *Kernel) EndProcess(ctx context.Context, subjectID, processID string) error {
	p, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return err
	}
	if p.OwnerUserID != subjectID {
		return ErrUnauthorized.Wrap("only the process owner may end it")
	}
	if p.Status != ProcessOpen {
		return ErrInvalidState.Wrap("process is already closed")
	}
	if err := k.store.EndProcess(ctx, processID); err != nil {
		return err
	}
	k.log.With(ctx).Info("process.ended", "process_id", processID)
	return nil
}

// ReadProcess returns a process by ID.
func (k *Kernel) ReadProcess(ctx context.Context, id string) (*Process, error) {
	return k.store.ReadProcess(ctx, id)
}

// ---- Transaction operations ----

// ReadTransaction returns a transaction by ID, checking subject authority.
func (k *Kernel) ReadTransaction(ctx context.Context, subjectID, txID string) (*Transaction, error) {
	tx, err := k.store.ReadTransaction(ctx, txID)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, ErrNotFound.Wrap("transaction not found")
	}
	if tx.OwnerUserID != subjectID && tx.SubjectUserID != subjectID && tx.TargetUserID != subjectID {
		return nil, ErrUnauthorized.Wrap("not authorized to view this transaction")
	}
	return tx, nil
}

// ListTransactions returns transactions matching the filter.
func (k *Kernel) ListTransactions(ctx context.Context, filter TxFilter) ([]*Transaction, error) {
	return k.store.ListTransactions(ctx, filter)
}

// RateTransaction sets a rating on a completed transaction and cascades to unrated descendants.
func (k *Kernel) RateTransaction(ctx context.Context, subjectID, txID string, rating float64) error {
	if rating != 0 && rating != 1 {
		return ErrInvalidInput.Wrap("rating must be 0 or 1")
	}
	tx, err := k.store.ReadTransaction(ctx, txID)
	if err != nil {
		return err
	}
	if tx == nil {
		return ErrNotFound.Wrap("transaction not found")
	}
	if tx.OwnerUserID != subjectID {
		return ErrUnauthorized.Wrap("only the process owner may rate a transaction")
	}
	tx.Rating = &rating
	if err := k.store.UpdateTransaction(ctx, tx); err != nil {
		return err
	}
	// Cascade to unrated descendants.
	_ = k.store.CascadeRating(ctx, tx.TraceID, rating)
	return nil
}

// ---- Stats ----

// ReadStats returns statistics for an action.
func (k *Kernel) ReadStats(ctx context.Context, actionID string) (*Stats, error) {
	return k.store.ReadStats(ctx, actionID)
}

// ---- Lookup ----

// LookupRequest is a natural-language query for actions.
type LookupRequest struct {
	Query  string
	Limit  int
	Offset int
}

// LookupResult is a ranked action for a lookup query.
type LookupResult struct {
	Action *Action
	Score  float32
}

// embedActionAsync computes and stores an embedding for an action's description.
// No-op if the embedder is nil or the description is empty. Errors are logged, not surfaced.
func (k *Kernel) embedActionAsync(ctx context.Context, a *Action) {
	if k.llm == nil || a.Description == "" {
		return
	}
	vec, err := k.llm.Embed(ctx, a.Description)
	if err != nil {
		k.log.With(ctx).Warn("embed.failed", "action_id", a.ID, "error", err.Error())
		return
	}
	if err := k.store.UpdateActionEmbedding(ctx, a.ID, vec); err != nil {
		k.log.With(ctx).Warn("embed.store_failed", "action_id", a.ID, "error", err.Error())
	}
}

// Lookup returns active actions ranked by semantic similarity to the query.
// Uses pre-computed stored embeddings — O(1) embedding API calls regardless of catalog size.
// Actions without a stored embedding are not returned until their description is set or updated.
// Returns ErrInvalidState if no embedder is configured.
func (k *Kernel) Lookup(ctx context.Context, req LookupRequest) ([]*LookupResult, error) {
	if k.llm == nil {
		return nil, ErrInvalidState.Wrap("lookup requires an embedding service")
	}
	qvec, err := k.llm.Embed(ctx, req.Query)
	if err != nil {
		return nil, ErrInternal.Wrapf("embedding failed: %v", err)
	}

	limit := req.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	embeddings, err := k.store.ListActionEmbeddings(ctx, 200)
	if err != nil {
		return nil, err
	}
	if len(embeddings) == 0 {
		return nil, nil
	}
	// Fetch action metadata for the actions that have stored embeddings.
	actions, err := k.store.ListActions(ctx, true, 200, 0)
	if err != nil {
		return nil, err
	}
	actionByID := make(map[string]*Action, len(actions))
	for _, a := range actions {
		actionByID[a.ID] = a
	}

	type scored struct {
		a     *Action
		score float32
	}
	results := make([]scored, 0, len(embeddings))
	for id, vec := range embeddings {
		a, ok := actionByID[id]
		if !ok {
			continue
		}
		sim := cosine(qvec, vec)
		// Combine semantic similarity with quality: score = sim * (0.5 + 0.5 * successRate)
		// An action with no call history gets 0.5 weight; perfect success gets full weight.
		quality := float32(0.5)
		if stats, _ := k.store.ReadStats(ctx, a.ID); stats != nil && stats.Uses > 0 {
			quality = float32(0.5 + 0.5*float64(stats.Successes)/float64(stats.Uses))
		}
		results = append(results, scored{a: a, score: sim * quality})
	}

	// Simple insertion sort — adequate for small catalogs in milestone 1.
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].score > results[j-1].score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	out := make([]*LookupResult, 0, limit)
	for i, r := range results {
		if i >= limit {
			break
		}
		out = append(out, &LookupResult{Action: r.a, Score: r.score})
	}
	return out, nil
}

func cosine(a, b []float32) float32 {
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt32(na) * sqrt32(nb))
}

func sqrt32(x float32) float32 {
	return float32(math.Sqrt(float64(x)))
}

// ---- Helpers ----

// requireAdmin returns nil if subjectID is the owner of a OR has admin ACL.
func (k *Kernel) requireAdmin(ctx context.Context, subjectID string, a *Action) error {
	if a.OwnerUserID == subjectID {
		return nil
	}
	ok, err := k.store.CheckACL(ctx, subjectID, a.ID, PermAdmin)
	if err != nil {
		return ErrInternal.Wrapf("acl check failed: %v", err)
	}
	if !ok {
		return ErrUnauthorized.Wrap("admin permission required")
	}
	return nil
}

// ---- Stats helpers ----

// IncrementalMean updates a running mean with a new observation.
func IncrementalMean(mean float64, n int64, x float64) float64 {
	return mean + (x-mean)/float64(n+1)
}

// UpdateStats applies one completed transaction's outcome to stats.
func UpdateStats(s *Stats, tx *Transaction, latencySeconds float64) {
	s.Uses++
	s.LastUsedAt = time.Now().UTC()

	if tx.Status == TxSuccess {
		s.Successes++
		if tx.Gross > 0 {
			s.PriceMean = IncrementalMean(s.PriceMean, s.Successes-1, float64(tx.Gross))
		}
	} else {
		s.Failures++
	}

	s.LatencyMean = IncrementalMean(s.LatencyMean, s.Uses-1, latencySeconds)

	if tx.Rating != nil {
		ratingCount := s.Uses
		s.RatingMean = IncrementalMean(s.RatingMean, ratingCount-1, *tx.Rating)
	}
}

// DefaultStats returns a zeroed Stats struct for a newly activated action.
func DefaultStats(actionID string) *Stats {
	return &Stats{ActionID: actionID, LastUsedAt: time.Now().UTC()}
}

// ---- Events ----

// CreateListenerRequest holds input for registering a listener.
type CreateListenerRequest struct {
	OwnerUserID    string
	SourceUserID   string
	EventName      string
	TargetActionID string
}

// CreateListener registers a new listener and returns it.
func (k *Kernel) CreateListener(ctx context.Context, req CreateListenerRequest) (*Listener, error) {
	if req.EventName == "" {
		return nil, ErrInvalidInput.Wrap("event_name is required")
	}
	a, err := k.store.ReadAction(ctx, req.TargetActionID)
	if err != nil {
		return nil, err
	}
	if a.OwnerUserID != req.OwnerUserID {
		ok, err := k.canCall(ctx, req.OwnerUserID, req.TargetActionID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrUnauthorized.Wrap("call permission required to register listener")
		}
	}
	l := &Listener{
		ID:             uuid.New().String(),
		OwnerUserID:    req.OwnerUserID,
		SourceUserID:   req.SourceUserID,
		EventName:      req.EventName,
		TargetActionID: req.TargetActionID,
		Active:         true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := k.store.CreateListener(ctx, l); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("listener.created", "listener_id", l.ID, "event", req.EventName)
	return l, nil
}

// PollListener returns the pending (unconsumed) events for a listener.
func (k *Kernel) PollListener(ctx context.Context, subjectID, listenerID string) ([]*Event, error) {
	l, err := k.store.ReadListener(ctx, listenerID)
	if err != nil {
		return nil, err
	}
	if l.OwnerUserID != subjectID && l.SourceUserID != subjectID {
		return nil, ErrUnauthorized.Wrap("not authorized to poll this listener")
	}
	return k.store.ListPendingEvents(ctx, listenerID)
}

// DeleteListener deactivates a listener and purges its pending events.
func (k *Kernel) DeleteListener(ctx context.Context, subjectID, listenerID string) error {
	l, err := k.store.ReadListener(ctx, listenerID)
	if err != nil {
		return err
	}
	if l.OwnerUserID != subjectID {
		return ErrUnauthorized.Wrap("only the listener owner may remove it")
	}
	if err := k.store.PurgeListenerEvents(ctx, listenerID); err != nil {
		return err
	}
	l.Active = false
	return k.store.UpdateListener(ctx, l)
}

// EmitEvent queues an event for all active listeners matching (sourceUserID, eventName).
// It does NOT call the target action — the listener owner must call ConsumeEvent explicitly.
// causingTraceID is stored as a FOLLOWS_FROM reference on each event record.
func (k *Kernel) EmitEvent(ctx context.Context, sourceUserID, eventName string, args map[string]any, causingTraceID string) ([]string, error) {
	listeners, err := k.store.ListListeners(ctx, sourceUserID, eventName)
	if err != nil {
		return nil, err
	}
	argsJSON, _ := json.Marshal(args)
	var eventIDs []string
	for _, l := range listeners {
		if !l.Active {
			continue
		}
		e := &Event{
			ID:             uuid.New().String(),
			ListenerID:     l.ID,
			ArgsJSON:       string(argsJSON),
			CausingTraceID: causingTraceID,
			CreatedAt:      time.Now().UTC(),
		}
		if err := k.store.CreateEvent(ctx, e); err != nil {
			k.log.With(ctx).Warn("emit.event_create_failed", "listener_id", l.ID, "error", err.Error())
			continue
		}
		eventIDs = append(eventIDs, e.ID)
	}
	k.log.With(ctx).Info("event.emitted", "source", sourceUserID, "event", eventName, "queued", len(eventIDs))
	return eventIDs, nil
}

// ConsumeEvent atomically locks an event and executes its listener's target action.
// At-least-once delivery: if the action call fails, the event is reset to pending.
// processID is the caller's open process, which must have sufficient funds to cover
// the action price. Returns the call reply on success.
func (k *Kernel) ConsumeEvent(ctx context.Context, subjectID, eventID, processID string) (*CallReply, error) {
	e, err := k.store.ReadEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	l, err := k.store.ReadListener(ctx, e.ListenerID)
	if err != nil {
		return nil, err
	}
	if l.OwnerUserID != subjectID {
		return nil, ErrUnauthorized.Wrap("only the listener owner may consume events")
	}
	if !l.Active {
		return nil, ErrInvalidState.Wrap("listener is inactive")
	}
	// Atomic lock — ErrInvalidState if already consumed or in-flight.
	if err := k.store.LockEvent(ctx, eventID); err != nil {
		return nil, err
	}
	// Resolve action target.
	action, err := k.store.ReadAction(ctx, l.TargetActionID)
	if err != nil {
		_ = k.store.UnlockEvent(ctx, eventID)
		return nil, err
	}
	owner, err := k.store.ReadUser(ctx, action.OwnerUserID)
	if err != nil {
		_ = k.store.UnlockEvent(ctx, eventID)
		return nil, err
	}
	// Decode event args.
	var args map[string]any
	_ = json.Unmarshal([]byte(e.ArgsJSON), &args)
	// Call the action using the supplied process.
	reply, err := k.Call(ctx, CallRequest{
		SubjectID:       subjectID,
		ProcessID:       processID,
		CausedByTraceID: e.CausingTraceID,
		TargetUserID:    owner.ID,
		ActionName:      action.Name,
		Args:            args,
	})
	if err != nil {
		_ = k.store.UnlockEvent(ctx, eventID)
		return nil, err
	}
	if err := k.store.SettleEvent(ctx, eventID, reply.TxID); err != nil {
		k.log.With(ctx).Warn("event.settle_failed", "event_id", eventID, "tx_id", reply.TxID, "error", err.Error())
	}
	k.log.With(ctx).Info("event.consumed", "event_id", eventID, "tx_id", reply.TxID)
	return reply, nil
}



