package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	FeeBPS            int64         // basis points, e.g. 2000 = 20%
	FeeRecipientID    string        // user ID that receives fees
	TokenSecret       string        // HMAC secret for JWT signing
	TokenTTL          time.Duration // token validity window
	ScriptTimeout     time.Duration
	ScriptMemory      int64              // bytes
	AllowLocalSources bool               // permit loopback/private URLs as action sources (tests only)
	SigningKey        ed25519.PrivateKey // Ed25519 private key for receipt/manifest signatures; nil until bootstrap
	IssuerUserID      string             // @sys user ID, set during bootstrap
	SuperuserHandle   string             // cached superuser handle for deposit checks
}

// DefaultConfig returns safe local defaults.
func DefaultConfig() Config {
	return Config{
		FeeBPS:        0,
		TokenTTL:      15 * time.Minute,
		ScriptTimeout: 10 * time.Second,
		ScriptMemory:  64 * 1024 * 1024, // 64 MiB
	}
}

// Kernel is the central service object.
// It holds all dependencies and exposes operations to both the CLI and HTTP server.
type Kernel struct {
	store   Store
	scripts ScriptExecutor
	http    HTTPExecutor
	llm     Embedder
	chatter Chatter
	cfg     Config
	log     *log.Logger
}

// federationExecutor is an optional extension of HTTPExecutor for cross-kernel calls.
// When the HTTP executor also implements this interface, Call() uses it for remote-kernel targets
// to send an idempotency key and receive the remote receipt for audit purposes.
type federationExecutor interface {
	ExecuteFederation(ctx context.Context, source, idempotencyKey string, args map[string]any) (result map[string]any, receiptJSON string, err error)
}

// New constructs a Kernel. scripts, http, llm, and chatter may be nil if those features are unused.
func New(store Store, scripts ScriptExecutor, http HTTPExecutor, llm Embedder, chatter Chatter, cfg Config, logger *log.Logger) *Kernel {
	if logger == nil {
		logger = log.Default()
	}
	return &Kernel{store: store, scripts: scripts, http: http, llm: llm, chatter: chatter, cfg: cfg, log: logger}
}

// SetSigningKey stores the Ed25519 signing key and issuer user ID after bootstrap completes.
func (k *Kernel) SetSigningKey(priv ed25519.PrivateKey, issuerUserID, superuserHandle string) {
	k.cfg.SigningKey = priv
	k.cfg.IssuerUserID = issuerUserID
	k.cfg.SuperuserHandle = superuserHandle
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
	if err := rejectSuspended(u); err != nil {
		return "", err
	}
	tok, err := IssueToken(u.ID, k.cfg.TokenSecret, k.cfg.TokenTTL)
	if err != nil {
		return "", err
	}
	k.log.With(ctx).Info("user.login", "user_id", u.ID)
	return tok, nil
}

func rejectSuspended(u *User) error {
	if u.SuspendedAt != nil {
		return ErrUnauthenticated.Wrap("account suspended")
	}
	return nil
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
// Only the superuser may call this; the check is enforced here, not only at the CLI boundary.
func (k *Kernel) Deposit(ctx context.Context, operatorID, targetUserID string, amount int64, reason string) (*Deposit, error) {
	operator, err := k.store.ReadUser(ctx, operatorID)
	if err != nil {
		return nil, err
	}
	suHandle := k.cfg.SuperuserHandle
	if suHandle == "" {
		suHandle, _ = k.store.GetConfig(ctx, "superuser_handle")
	}
	if operator.Handle != suHandle {
		return nil, ErrUnauthorized.Wrap("only the superuser may issue deposits")
	}
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
func validateHTTPSource(source string, allowLocal bool) error {
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
	if !allowLocal {
		if strings.EqualFold(host, "localhost") {
			return ErrInvalidInput.Wrap("URL must not target localhost")
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return ErrInvalidInput.Wrap("URL must not target private or reserved addresses")
			}
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
		if err := validateHTTPSource(req.Source, k.cfg.AllowLocalSources); err != nil {
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
		_, hash, err := k.scripts.Compile(ctx, []byte(req.Source))
		if err != nil {
			return nil, ErrInvalidInput.Wrapf("wasm compilation failed: %v", err)
		}
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

// RegisterNativeAction creates a native action for bootstrap use.
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

// ActivateNativeAction activates a native action for bootstrap use.
// Native actions are not managed by the normal user-facing action lifecycle.
func (k *Kernel) ActivateNativeAction(ctx context.Context, actionID string) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if a.Kind != KindNative {
		return ErrInvalidInput.Wrap("action is not native")
	}
	stats, _ := k.store.ReadStats(ctx, actionID)
	if stats == nil {
		if err := k.store.UpsertStats(ctx, DefaultStats(actionID)); err != nil {
			return err
		}
	}
	a.Active = true
	a.UpdatedAt = time.Now().UTC()
	if err := k.store.UpdateAction(ctx, a); err != nil {
		return err
	}
	k.log.With(ctx).Info("action.native_enabled", "action_id", actionID)
	return nil
}

// ReadAction returns the action with the given ID (no ACL check).
// Returns ErrNotFound for soft-deleted actions.
// Used internally; external callers should use ReadActionForSubject.
func (k *Kernel) ReadAction(ctx context.Context, id string) (*Action, error) {
	a, err := k.store.ReadAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.DeletedAt != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	return a, nil
}

// ReadActionForSubject returns an action only if the subject has read access.
// Public actions are readable by anyone. Otherwise Owner ∨ ACL(read) ∨ ACL(admin) is required.
func (k *Kernel) ReadActionForSubject(ctx context.Context, subjectID, actionID string) (*Action, error) {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if a.Public || a.OwnerUserID == subjectID {
		return a, nil
	}
	ok, err := k.canRead(ctx, subjectID, a)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrUnauthorized.Wrap("read permission denied")
	}
	return a, nil
}

// canRead returns true if subjectID may read action a.
// CanRead(u,a) := Owner(u,a) ∨ Public(a) ∨ ACL(u,a,read) ∨ ACL(u,a,admin)
func (k *Kernel) canRead(ctx context.Context, subjectID string, a *Action) (bool, error) {
	if a.OwnerUserID == subjectID || a.Public {
		return true, nil
	}
	if ok, err := k.store.CheckACL(ctx, subjectID, a.ID, PermRead); err != nil {
		return false, ErrInternal.Wrapf("acl check: %v", err)
	} else if ok {
		return true, nil
	}
	ok, err := k.store.CheckACL(ctx, subjectID, a.ID, PermAdmin)
	if err != nil {
		return false, ErrInternal.Wrapf("acl check: %v", err)
	}
	return ok, nil
}

// ReadActionByOwnerName returns an action by (ownerID, name).
func (k *Kernel) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*Action, error) {
	return k.store.ReadActionByOwnerName(ctx, ownerID, name)
}

// ListActions returns public actions. With activeOnly set it returns public active actions.
func (k *Kernel) ListActions(ctx context.Context, activeOnly bool, limit, offset int) ([]*Action, error) {
	return k.store.ListActions(ctx, activeOnly, limit, offset)
}

// ListAllActions returns all actions regardless of active state.
func (k *Kernel) ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error) {
	return k.store.ListAllActions(ctx, limit, offset)
}

// ListAllProcesses returns all processes ordered by creation time.
func (k *Kernel) ListProcesses(ctx context.Context, ownerID string, limit, offset int) ([]*Process, error) {
	return k.store.ListProcesses(ctx, ownerID, limit, offset)
}

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
	a.Public = true
	a.UpdatedAt = time.Now().UTC()
	if err := k.store.UpdateAction(ctx, a); err != nil {
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
	a.Public = false
	a.UpdatedAt = time.Now().UTC()
	if err := k.store.UpdateAction(ctx, a); err != nil {
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

// FirstBoot atomically creates the @sys superuser account, generates an Ed25519 signing
// keypair, and stores all three config entries in a single SQLite transaction.
// Safe to call on a database that was already initialized — user INSERT is skipped.
func (k *Kernel) FirstBoot(ctx context.Context, password string) error {
	if password == "" {
		return ErrInvalidInput.Wrap("password cannot be empty")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return ErrInvalidInput.Wrapf("could not hash password: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return ErrInternal.Wrapf("generate signing key: %v", err)
	}
	now := time.Now().UTC()
	u := &User{
		ID:           uuid.New().String(),
		Handle:       "@sys",
		Email:        "sys@sys",
		PasswordHash: hash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	configs := map[string]string{
		"superuser_handle":    "@sys",
		"signing_public_key":  base64.RawURLEncoding.EncodeToString(pub),
		"signing_private_key": base64.RawURLEncoding.EncodeToString(priv),
	}
	if err := k.store.InitFirstBoot(ctx, u, configs); err != nil {
		return err
	}
	su, err := k.store.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		return err
	}
	k.SetSigningKey(ed25519.PrivateKey(priv), su.ID, "@sys")
	return nil
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
	if a.Kind == KindNative {
		return nil, ErrUnauthorized.Wrap("native actions are managed by bootstrap")
	}
	if err := k.requireAdmin(ctx, subjectID, a); err != nil {
		return nil, err
	}

	if req.Price != nil {
		if *req.Price < 0 {
			return nil, ErrInvalidInput.Wrap("price must be non-negative")
		}
		a.Price = *req.Price
		a.Active = false
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
			_, hash, err := k.scripts.Compile(ctx, []byte(*req.Source))
			if err != nil {
				return nil, ErrInvalidInput.Wrapf("wasm compilation failed: %v", err)
			}
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
	if a.Kind == KindNative {
		return ErrUnauthorized.Wrap("native actions are managed by bootstrap")
	}
	if err := k.requireAdmin(ctx, subjectID, a); err != nil {
		return err
	}
	if active {
		if a.Source == "" && a.Kind != KindNative {
			return ErrInvalidState.Wrap("cannot activate action with no source")
		}
		if a.Kind == KindHTTP {
			if err := validateHTTPSource(a.Source, k.cfg.AllowLocalSources); err != nil {
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
	if a.Kind == KindNative {
		return ErrUnauthorized.Wrap("native actions are managed by bootstrap")
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
// Process creation, user debit, and root trace creation are atomic.
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
	// Root trace: ParentTraceID == ID.
	t := &Trace{
		ID:        uuid.New().String(),
		ProcessID: p.ID,
		CreatedAt: now,
	}
	t.ParentTraceID = t.ID

	if err := k.store.StartProcess(ctx, p, t, ownerID, funds); err != nil {
		return nil, nil, err
	}
	p.Available = funds

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

// RateTransaction submits a rating for a completed transaction and cascades to unrated descendants.
// Ratings are stored in a separate ratings table; the transaction row is never modified.
// Both the root rating insertion and the cascade are performed atomically in a single store operation.
// After cascade, action stats are updated to reflect the new rating.
func (k *Kernel) RateTransaction(ctx context.Context, subjectID, txID string, rating float64) error {
	if rating != 0 && rating != 1 {
		return ErrInvalidInput.Wrap("rating must be 0 or 1")
	}
	rater, err := k.store.ReadUser(ctx, subjectID)
	if err != nil {
		return err
	}
	if rater.SuspendedAt != nil {
		return ErrUnauthenticated.Wrap("account suspended")
	}
	tx, err := k.store.ReadTransaction(ctx, txID)
	if err != nil {
		return err
	}
	if tx == nil {
		return ErrNotFound.Wrap("transaction not found")
	}
	// Check for duplicate rating (transaction already has a rating record).
	if existing, _ := k.store.ReadRatingByTxID(ctx, txID); existing != nil {
		return ErrInvalidInput.Wrap("transaction already rated")
	}
	// Look up receipt for this transaction (may be nil for old transactions).
	receipt, _ := k.store.ReadReceiptByTxID(ctx, txID)
	r := &Rating{
		ID:          uuid.New().String(),
		RatedTxID:   txID,
		RaterUserID: subjectID,
		Rating:      rating,
		CreatedAt:   time.Now().UTC(),
	}
	if receipt != nil {
		r.RatedReceiptID = &receipt.ID
	}
	sig, err := signRating(k.cfg.SigningKey, r)
	if err != nil {
		return err
	}
	r.Signature = sig
	if err := k.store.CreateRatingCascade(ctx, txID, tx.TraceID, r); err != nil {
		return err
	}
	// Update action stats to keep rating_mean and rating_count current.
	stats, err := k.store.ReadStats(ctx, tx.ActionID)
	if err != nil || stats == nil {
		stats = DefaultStats(tx.ActionID)
	}
	stats.RatingCount++
	stats.RatingMean = IncrementalMean(stats.RatingMean, stats.RatingCount-1, rating)
	if updateErr := k.store.UpsertStats(ctx, stats); updateErr != nil {
		k.log.With(ctx).Warn("rate.stats_update_failed", "action_id", tx.ActionID, "error", updateErr)
	}
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
	Action      *Action
	OwnerHandle string
	Score       float32
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

	// Resolve owner handles; cache to avoid redundant store reads.
	ownerHandles := make(map[string]string)
	for _, r := range out {
		if _, cached := ownerHandles[r.Action.OwnerUserID]; !cached {
			if u, err := k.store.ReadUser(ctx, r.Action.OwnerUserID); err == nil {
				ownerHandles[r.Action.OwnerUserID] = u.Handle
			}
		}
		r.OwnerHandle = ownerHandles[r.Action.OwnerUserID]
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

// requireAdmin returns nil if subjectID is the owner of a, the platform superuser, or has admin ACL.
func (k *Kernel) requireAdmin(ctx context.Context, subjectID string, a *Action) error {
	if a.OwnerUserID == subjectID {
		return nil
	}
	if k.isSuperuser(ctx, subjectID) {
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

// isSuperuser returns true if subjectID is the platform superuser registered during bootstrap.
func (k *Kernel) isSuperuser(ctx context.Context, subjectID string) bool {
	handle, _ := k.store.GetConfig(ctx, "superuser_handle")
	if handle == "" {
		return false
	}
	u, err := k.store.ReadUserByHandle(ctx, handle)
	return err == nil && u.ID == subjectID
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
		ok, err := k.canCall(ctx, req.OwnerUserID, a)
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

// DeleteListener atomically deactivates a listener and purges its pending events.
func (k *Kernel) DeleteListener(ctx context.Context, subjectID, listenerID string) error {
	l, err := k.store.ReadListener(ctx, listenerID)
	if err != nil {
		return err
	}
	if l.OwnerUserID != subjectID {
		return ErrUnauthorized.Wrap("only the listener owner may remove it")
	}
	return k.store.DeleteListenerWithEvents(ctx, listenerID)
}

// ListListeners returns all listeners owned by ownerID.
func (k *Kernel) ListListeners(ctx context.Context, ownerID string, limit, offset int) ([]*Listener, error) {
	return k.store.ListListenersByOwner(ctx, ownerID, limit, offset)
}

// GetListener returns listener metadata. Subject must be the owner or source user.
func (k *Kernel) GetListener(ctx context.Context, subjectID, listenerID string) (*Listener, error) {
	l, err := k.store.ReadListener(ctx, listenerID)
	if err != nil {
		return nil, err
	}
	if l.OwnerUserID != subjectID && l.SourceUserID != subjectID {
		return nil, ErrUnauthorized.Wrap("not authorized to view this listener")
	}
	return l, nil
}

// GetReceiptByTxID returns the receipt for a transaction.
func (k *Kernel) GetReceiptByTxID(ctx context.Context, txID string) (*Receipt, error) {
	return k.store.ReadReceiptByTxID(ctx, txID)
}

// GetReceiptByID returns the receipt with the given ID.
func (k *Kernel) GetReceiptByID(ctx context.Context, id string) (*Receipt, error) {
	return k.store.ReadReceipt(ctx, id)
}

// GetIdempotencyRecord returns an unexpired idempotency record matching key + counterparty.
func (k *Kernel) GetIdempotencyRecord(ctx context.Context, key, counterpartyUserID string) (*IdempotencyRecord, error) {
	return k.store.ReadIdempotencyRecord(ctx, key, counterpartyUserID)
}

// CreateIdempotencyRecord stores a new idempotency record.
func (k *Kernel) CreateIdempotencyRecord(ctx context.Context, r *IdempotencyRecord) error {
	return k.store.CreateIdempotencyRecord(ctx, r)
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
		k.log.With(ctx).Error("event.settle_failed", "event_id", eventID, "tx_id", reply.TxID, "error", err.Error())
		return nil, ErrInternal.Wrap("could not settle event after successful call")
	}
	k.log.With(ctx).Info("event.consumed", "event_id", eventID, "tx_id", reply.TxID)
	return reply, nil
}

// ---- Receipt helpers ----

// buildReceipt constructs a Receipt from a committed transaction and signs it.
// Returns ErrInvalidState if the kernel has not been bootstrapped (no issuer configured).
func (k *Kernel) buildReceipt(tx *Transaction) (*Receipt, error) {
	if err := k.requireReceiptSigningReady(); err != nil {
		return nil, err
	}
	argsHash, err := jcsHashStr(tx.ArgsJSON)
	if err != nil {
		return nil, ErrInternal.Wrapf("hash args: %v", err)
	}
	replyHash, err := jcsHashStr(tx.ReplyJSON)
	if err != nil {
		return nil, ErrInternal.Wrapf("hash reply: %v", err)
	}
	r := &Receipt{
		ID:           uuid.New().String(),
		IssuerUserID: k.cfg.IssuerUserID,
		TxID:         tx.ID,
		TraceID:      tx.TraceID,
		ActionID:     tx.ActionID,
		ArgsHash:     argsHash,
		ReplyHash:    replyHash,
		Status:       tx.Status,
		Gross:        tx.Gross,
		Net:          tx.Net,
		Fee:          tx.Fee,
		Reason:       tx.Reason,
		CreatedAt:    time.Now().UTC(),
	}
	sig, err := signReceipt(k.cfg.SigningKey, r)
	if err != nil {
		return nil, err
	}
	r.Signature = sig
	return r, nil
}

func (k *Kernel) requireReceiptSigningReady() error {
	if k.cfg.IssuerUserID == "" || len(k.cfg.SigningKey) != ed25519.PrivateKeySize {
		return ErrInvalidState.Wrap("kernel cannot issue signed receipts")
	}
	return nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// signReceipt signs the canonical receipt payload (excluding Signature) with JCS.
func signReceipt(key ed25519.PrivateKey, r *Receipt) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := CanonicalJSON(receiptPayload{
		ActionID:     r.ActionID,
		ArgsHash:     r.ArgsHash,
		CreatedAt:    r.CreatedAt.UTC().Format(time.RFC3339),
		Fee:          r.Fee,
		Gross:        r.Gross,
		ID:           r.ID,
		IssuerUserID: r.IssuerUserID,
		Net:          r.Net,
		Reason:       r.Reason,
		ReplyHash:    r.ReplyHash,
		Status:       string(r.Status),
		TraceID:      r.TraceID,
		TxID:         r.TxID,
	})
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize receipt: %v", err)
	}
	sig := ed25519.Sign(key, payload)
	return base64.RawURLEncoding.EncodeToString(sig), nil
}

// receiptPayload is the canonical signed form of a receipt (fields alphabetically ordered).
type receiptPayload struct {
	ActionID     string `json:"action_id"`
	ArgsHash     string `json:"args_hash"`
	CreatedAt    string `json:"created_at"`
	Fee          int64  `json:"fee"`
	Gross        int64  `json:"gross"`
	ID           string `json:"id"`
	IssuerUserID string `json:"issuer_user_id"`
	Net          int64  `json:"net"`
	Reason       string `json:"reason"`
	ReplyHash    string `json:"reply_hash"`
	Status       string `json:"status"`
	TraceID      string `json:"trace_id"`
	TxID         string `json:"tx_id"`
}

// signRating signs the canonical rating payload (excluding Signature) with JCS.
func signRating(key ed25519.PrivateKey, r *Rating) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := canonicalRatingPayload(r)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(key, payload)
	return base64.RawURLEncoding.EncodeToString(sig), nil
}

func canonicalRatingPayload(r *Rating) ([]byte, error) {
	receiptID := ""
	if r.RatedReceiptID != nil {
		receiptID = *r.RatedReceiptID
	}
	payload, err := CanonicalJSON(ratingPayload{
		CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339),
		ID:             r.ID,
		RatedReceiptID: receiptID,
		RatedTxID:      r.RatedTxID,
		RaterUserID:    r.RaterUserID,
		Rating:         r.Rating,
	})
	if err != nil {
		return nil, ErrInternal.Wrapf("canonicalize rating: %v", err)
	}
	return payload, nil
}

// ratingPayload is the canonical signed form of a rating (fields alphabetically ordered).
type ratingPayload struct {
	CreatedAt      string  `json:"created_at"`
	ID             string  `json:"id"`
	RatedReceiptID string  `json:"rated_receipt_id"`
	RatedTxID      string  `json:"rated_tx_id"`
	RaterUserID    string  `json:"rater_user_id"`
	Rating         float64 `json:"rating"`
}

// ---- Federation operations ----

// RegisterRemoteKernel creates or updates a local user record representing a remote kernel peer.
func (k *Kernel) RegisterRemoteKernel(ctx context.Context, handle, publicKey, baseURL string) (*User, error) {
	if handle == "" || publicKey == "" || baseURL == "" {
		return nil, ErrInvalidInput.Wrap("handle, public_key, and base_url are required")
	}
	if _, err := decodeRemotePublicKey(publicKey); err != nil {
		return nil, err
	}
	if err := validateRemoteBaseURL(baseURL); err != nil {
		return nil, err
	}
	// Check if a user with this public key already exists.
	existing, err := k.store.ReadUserByPublicKey(ctx, publicKey)
	if err == nil && existing != nil {
		// Update the existing record's base URL.
		existing.RemoteBaseURL = baseURL
		existing.UpdatedAt = time.Now().UTC()
		if err := k.store.UpdateUser(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}
	now := time.Now().UTC()
	u := &User{
		ID:            uuid.New().String(),
		Handle:        handle,
		Email:         handle + "@remote",
		PasswordHash:  "remote",
		PublicKey:     publicKey,
		RemoteBaseURL: baseURL,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := k.store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("remote_kernel.registered", "handle", handle, "base_url", baseURL)
	return u, nil
}

func decodeRemotePublicKey(publicKey string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil {
		return nil, ErrInvalidInput.Wrap("public_key must be base64url")
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, ErrInvalidInput.Wrap("public_key must be a 32-byte Ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}

func validateRemoteBaseURL(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return ErrInvalidInput.Wrap("remote_base_url must be an absolute URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidInput.Wrap("remote_base_url must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ErrInvalidInput.Wrap("remote_base_url must not include userinfo, query, or fragment")
	}
	return nil
}

// ListRemoteKernels returns all local user records that represent remote kernel peers.
func (k *Kernel) ListRemoteKernels(ctx context.Context) ([]*User, error) {
	all, err := k.store.ListUsers(ctx, 1000, 0)
	if err != nil {
		return nil, err
	}
	var remote []*User
	for _, u := range all {
		if u.RemoteBaseURL != "" {
			remote = append(remote, u)
		}
	}
	return remote, nil
}

// ImportRemoteAction creates a local HTTP action from a remote kernel's action manifest.
// The action is owned by the remote kernel user identified by remoteUserID.
func (k *Kernel) ImportRemoteAction(ctx context.Context, remoteUserID string, m ActionManifest) (*Action, error) {
	remoteUser, err := k.store.ReadUser(ctx, remoteUserID)
	if err != nil {
		return nil, err
	}
	if remoteUser.RemoteBaseURL == "" {
		return nil, ErrInvalidInput.Wrap("user is not a remote kernel")
	}
	source := strings.TrimRight(remoteUser.RemoteBaseURL, "/") +
		"/v1/federation/call?action=" + url.QueryEscape(m.Name)
	now := time.Now().UTC()
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  remoteUserID,
		Name:         m.Name,
		Kind:         KindHTTP,
		Active:       false,
		Price:        m.Price,
		Description:  m.Description,
		InputSchema:  m.InputSchema,
		OutputSchema: m.OutputSchema,
		Source:       source,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := k.store.CreateAction(ctx, a); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("action.imported_remote", "action_id", a.ID, "name", a.Name)
	return a, nil
}

// GetActionManifest returns a signed manifest for a public active action.
// Manifests are only available for actions that are both active and public.
func (k *Kernel) GetActionManifest(ctx context.Context, subjectID, actionID string) (*ActionManifest, error) {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if a.DeletedAt != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	if !a.Active || !a.Public {
		return nil, ErrUnauthorized.Wrap("manifest only available for public active actions")
	}
	owner, err := k.store.ReadUser(ctx, a.OwnerUserID)
	if err != nil {
		return nil, err
	}
	stats, _ := k.store.ReadStats(ctx, a.ID)
	m := &ActionManifest{
		OwnerHandle:  owner.Handle,
		Name:         a.Name,
		Description:  a.Description,
		InputSchema:  a.InputSchema,
		OutputSchema: a.OutputSchema,
		Price:        a.Price,
		Kind:         a.Kind,
		ArtifactHash: a.ArtifactHash,
		UpdatedAt:    a.UpdatedAt,
		Stats:        stats,
	}
	sig, err := signManifest(k.cfg.SigningKey, m)
	if err != nil {
		return nil, err
	}
	m.Signature = sig
	return m, nil
}

// signManifest signs the canonical manifest payload (excluding Signature) with JCS.
func signManifest(key ed25519.PrivateKey, m *ActionManifest) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	inputJSON, _ := CanonicalJSON(m.InputSchema)
	outputJSON, _ := CanonicalJSON(m.OutputSchema)
	statsJSON := ""
	if m.Stats != nil {
		if b, err := CanonicalJSON(m.Stats); err == nil {
			statsJSON = string(b)
		}
	}
	payload, err := CanonicalJSON(manifestPayload{
		ArtifactHash: m.ArtifactHash,
		Description:  m.Description,
		InputSchema:  string(inputJSON),
		Kind:         string(m.Kind),
		Name:         m.Name,
		OutputSchema: string(outputJSON),
		OwnerHandle:  m.OwnerHandle,
		Price:        m.Price,
		Stats:        statsJSON,
		UpdatedAt:    m.UpdatedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize manifest: %v", err)
	}
	sig := ed25519.Sign(key, payload)
	return base64.RawURLEncoding.EncodeToString(sig), nil
}

type manifestPayload struct {
	ArtifactHash string `json:"artifact_hash"`
	Description  string `json:"description"`
	InputSchema  string `json:"input_schema"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	OutputSchema string `json:"output_schema"`
	OwnerHandle  string `json:"owner_handle"`
	Price        int64  `json:"price"`
	Stats        string `json:"stats"`
	UpdatedAt    string `json:"updated_at"`
}
