package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
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
	AuthIssuer        string             // JUICE_AUTH_ISSUER — iss claim in JWTs; empty = no claim
	AuthAudience      string             // JUICE_AUTH_AUDIENCE — aud claim in JWTs; empty = no validation
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

// AllowsLocalSources reports whether the kernel is configured to permit loopback/private source URLs.
func (k *Kernel) AllowsLocalSources() bool { return k.cfg.AllowLocalSources }

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

// SetSuperuserHandle updates the in-memory superuser handle. Used in tests.
func (k *Kernel) SetSuperuserHandle(handle string) {
	k.cfg.SuperuserHandle = handle
}

// SetTokenSecret updates the JWT HMAC secret after bootstrap completes.
func (k *Kernel) SetTokenSecret(secret string) {
	k.cfg.TokenSecret = secret
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
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("user.create.start", "handle", req.Handle)
	if req.Handle == "" {
		return nil, ErrInvalidInput.Wrap("handle is required")
	}
	if strings.Contains(req.Handle, "/") {
		return nil, ErrInvalidInput.Wrap("handle must not contain /")
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
		logger.Warn("user.create.failed", "handle", req.Handle, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	logger.Info("user.created", "user_id", u.ID, "handle", u.Handle, "status", "success", "duration_ms", time.Since(start).Milliseconds())
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

// ReadUserByPublicKey returns the user with the given base64url Ed25519 public key.
func (k *Kernel) ReadUserByPublicKey(ctx context.Context, publicKey string) (*User, error) {
	return k.store.ReadUserByPublicKey(ctx, publicKey)
}

// Login authenticates handle+password and returns a signed JWT.
func (k *Kernel) Login(ctx context.Context, handle, password string) (string, error) {
	u, err := k.store.ReadUserByHandle(ctx, handle)
	if err != nil {
		return "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if u.RemoteBaseURL != "" {
		return "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if !CheckPassword(password, u.PasswordHash) {
		return "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if err := rejectSuspended(u); err != nil {
		return "", err
	}
	tok, err := IssueToken(u.ID, k.cfg.TokenSecret, k.cfg.AuthIssuer, k.cfg.AuthAudience, k.cfg.TokenTTL)
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
// Only the superuser may call this.
func (k *Kernel) SuspendUser(ctx context.Context, operatorID, targetID string) error {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("user.suspend.start", "target_id", targetID)
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		logger.Warn("user.suspend.failed", "target_id", targetID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	if err := k.store.SuspendUser(ctx, targetID); err != nil {
		logger.Warn("user.suspend.failed", "target_id", targetID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	logger.Info("user.suspended", "target_id", targetID, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// UnsuspendUser removes the suspension from a user.
// Only the superuser may call this.
func (k *Kernel) UnsuspendUser(ctx context.Context, operatorID, targetID string) error {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("user.unsuspend.start", "target_id", targetID)
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		logger.Warn("user.unsuspend.failed", "target_id", targetID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	if err := k.store.UnsuspendUser(ctx, targetID); err != nil {
		logger.Warn("user.unsuspend.failed", "target_id", targetID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	logger.Info("user.unsuspended", "target_id", targetID, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// requireSuperuser returns ErrUnauthorized if operatorID is not the configured superuser.
func (k *Kernel) requireSuperuser(ctx context.Context, operatorID string) error {
	u, err := k.authenticatedSubject(ctx, operatorID)
	if err != nil {
		return err
	}
	if !k.isUserSuperuser(ctx, u) {
		return ErrUnauthorized.Wrap("only the superuser may perform this operation")
	}
	return nil
}

// Deposit adds credits directly to a user's available balance and records an audit entry.
// Only the superuser may call this; the check is enforced here, not only at the CLI boundary.
// ValidateFeeRecipient returns an error if FeeBPS > 0 and the configured fee
// recipient user does not exist. Call after bootstrap to catch misconfiguration.
func (k *Kernel) ValidateFeeRecipient(ctx context.Context) error {
	if k.cfg.FeeBPS == 0 || k.cfg.FeeRecipientID == "" {
		return nil
	}
	if _, err := k.store.ReadUser(ctx, k.cfg.FeeRecipientID); err != nil {
		return ErrInvalidState.Wrapf("fee recipient %q not found in database", k.cfg.FeeRecipientID)
	}
	return nil
}

func (k *Kernel) Deposit(ctx context.Context, operatorID, targetUserID string, amount int64, reason string) (*Deposit, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("deposit.start", "target_user_id", targetUserID, "amount", amount)
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		logger.Warn("deposit.failed", "target_user_id", targetUserID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
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
		logger.Warn("deposit.failed", "target_user_id", targetUserID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	logger.Info("deposit.created", "deposit_id", d.ID, "target_user_id", targetUserID, "amount", amount, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return d, nil
}

// VerifyToken validates a bearer token and returns the subject user ID.
func (k *Kernel) VerifyToken(token string) (string, error) {
	return VerifyToken(token, k.cfg.TokenSecret, k.cfg.AuthIssuer, k.cfg.AuthAudience)
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

func (k *Kernel) CreateAction(ctx context.Context, subjectID string, req CreateActionRequest) (*Action, error) {
	if err := k.requireSelf(ctx, subjectID, req.OwnerUserID); err != nil {
		return nil, err
	}
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
	if req.InputSchema != nil {
		if err := ValidateSchema(req.InputSchema); err != nil {
			return nil, err
		}
	}
	if req.OutputSchema != nil {
		if err := ValidateSchema(req.OutputSchema); err != nil {
			return nil, err
		}
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
	k.log.With(ctx).Info("action.created", "action_id", a.ID, "name", a.Name, "status", "success")
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
	if err := validateSchemaDescriptions(a.InputSchema, "input"); err != nil {
		return err
	}
	if err := validateSchemaDescriptions(a.OutputSchema, "output"); err != nil {
		return err
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
	k.storeEmbedding(ctx, actionID, a.Description)
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

// ListOwnedActions returns all non-deleted actions owned by ownerID, including inactive
// and private ones. Intended for authenticated owner list views.
func (k *Kernel) ListOwnedActions(ctx context.Context, ownerID string, limit, offset int) ([]*Action, error) {
	return k.store.ListActionsByOwner(ctx, ownerID, limit, offset)
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
	if strings.HasPrefix(strings.TrimSpace(a.Source), "{") {
		var osrc OpenAPISource
		if jsonErr := json.Unmarshal([]byte(a.Source), &osrc); jsonErr == nil && osrc.Type == "openapi" {
			if !osrc.OwnershipVerified {
				return ErrUnauthorized.Wrap("ownership not verified: add x-juice-owner to spec")
			}
		}
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
	jwtRaw := make([]byte, 32)
	if _, err := rand.Read(jwtRaw); err != nil {
		return ErrInternal.Wrapf("generate jwt secret: %v", err)
	}
	configs := map[string]string{
		"superuser_handle":    "@sys",
		"signing_public_key":  base64.RawURLEncoding.EncodeToString(pub),
		"signing_private_key": base64.RawURLEncoding.EncodeToString(priv),
		"jwt_secret":          hex.EncodeToString(jwtRaw),
	}
	if err := k.store.InitFirstBoot(ctx, u, configs); err != nil {
		return err
	}
	su, err := k.store.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		return err
	}
	// Read persisted values rather than using in-memory generated ones.
	// InitFirstBoot uses INSERT OR IGNORE, so on a re-run the stored values may differ
	// from those generated above. Using stored values ensures the kernel always
	// matches what is in the database.
	storedPrivB64, err := k.store.GetConfig(ctx, "signing_private_key")
	if err != nil {
		return ErrInternal.Wrapf("read stored signing key: %v", err)
	}
	storedPriv, err := base64.RawURLEncoding.DecodeString(storedPrivB64)
	if err != nil {
		return ErrInternal.Wrapf("decode stored signing key: %v", err)
	}
	k.SetSigningKey(ed25519.PrivateKey(storedPriv), su.ID, "@sys")
	// Only apply the stored secret when no secret was provided at construction
	// (e.g. no JUICE_SECRET_KEY env var). If one was already configured, it takes
	// precedence and the stored value serves as the fallback for future startups.
	if k.cfg.TokenSecret == "" {
		storedJWT, err := k.store.GetConfig(ctx, "jwt_secret")
		if err != nil {
			return ErrInternal.Wrapf("read stored jwt secret: %v", err)
		}
		k.SetTokenSecret(storedJWT)
	}
	return nil
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
		k.storeEmbedding(ctx, a.ID, a.Description)
	}
	k.log.With(ctx).Info("action.updated", "action_id", a.ID, "status", "success")
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
		if strings.TrimSpace(a.Description) == "" {
			return ErrInvalidState.Wrap("description is required before activation")
		}
		if a.Source == "" && a.Kind != KindNative {
			return ErrInvalidState.Wrap("cannot activate action with no source")
		}
		if err := ValidateSchema(a.InputSchema); err != nil {
			return ErrInvalidState.Wrapf("invalid input schema: %v", err)
		}
		if err := ValidateSchema(a.OutputSchema); err != nil {
			return ErrInvalidState.Wrapf("invalid output schema: %v", err)
		}
		if err := validateSchemaDescriptions(a.InputSchema, "input"); err != nil {
			return ErrInvalidState.Wrapf("input schema: %v", err)
		}
		if err := validateSchemaDescriptions(a.OutputSchema, "output"); err != nil {
			return ErrInvalidState.Wrapf("output schema: %v", err)
		}
		if a.Kind == KindHTTP {
			src := a.Source
			if strings.HasPrefix(strings.TrimSpace(src), "{") {
				var osrc OpenAPISource
				if err := json.Unmarshal([]byte(src), &osrc); err != nil {
					return ErrInvalidInput.Wrap("invalid OpenAPI source JSON")
				}
				src = osrc.BaseURL
			}
			if err := validateHTTPSource(src, k.cfg.AllowLocalSources); err != nil {
				return err
			}
		}
		if a.Kind == KindWasm {
			if k.scripts == nil {
				return ErrInvalidState.Wrap("cannot activate wasm action: script executor not configured")
			}
			_, hash, err := k.scripts.Compile(ctx, []byte(a.Source))
			if err != nil {
				return ErrInvalidState.Wrapf("wasm compile failed: %v", err)
			}
			a.ArtifactHash = hash
		}
		if a.Public && strings.HasPrefix(strings.TrimSpace(a.Source), "{") {
			var osrc OpenAPISource
			if jsonErr := json.Unmarshal([]byte(a.Source), &osrc); jsonErr == nil && osrc.Type == "openapi" {
				if !osrc.OwnershipVerified {
					return ErrUnauthorized.Wrap("ownership not verified: add x-juice-owner to spec")
				}
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
	if active {
		k.storeEmbedding(ctx, actionID, a.Description)
	}
	event := "action.disabled"
	if active {
		event = "action.enabled"
	}
	k.log.With(ctx).Info(event, "action_id", actionID, "status", "success")
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
	k.log.With(ctx).Info("action.deleted", "action_id", actionID, "status", "success")
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
	if perm != PermRead && perm != PermCall && perm != PermAdmin {
		return ErrInvalidInput.Wrapf("unknown permission %q", perm)
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
func (k *Kernel) StartProcess(ctx context.Context, subjectID, ownerID string, funds int64) (*Process, *Trace, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("process.start.start", "owner", ownerID, "funds", funds)
	if err := k.requireSelf(ctx, subjectID, ownerID); err != nil {
		logger.Warn("process.start.failed", "owner", ownerID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, nil, err
	}
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
		logger.Warn("process.start.failed", "owner", ownerID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, nil, err
	}
	p.Available = funds

	logger.Info("process.started", "process_id", p.ID, "owner", ownerID, "funds", funds, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return p, t, nil
}

// FundProcess adds more credits to an existing open process.
func (k *Kernel) FundProcess(ctx context.Context, subjectID, processID string, funds int64) error {
	if _, err := k.authenticatedSubject(ctx, subjectID); err != nil {
		return err
	}
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
	k.log.With(ctx).Info("process.funded", "process_id", processID, "funds", funds, "status", "success")
	return nil
}

// EndProcess closes a process and returns all remaining funds to the owner.
func (k *Kernel) EndProcess(ctx context.Context, subjectID, processID string) error {
	if _, err := k.authenticatedSubject(ctx, subjectID); err != nil {
		return err
	}
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
	k.log.With(ctx).Info("process.ended", "process_id", processID, "status", "success")
	return nil
}

// GrantProcessAuthority grants another user explicit authority to use a process.
// Only the process owner may grant this right.
func (k *Kernel) GrantProcessAuthority(ctx context.Context, operatorID, subjectID, processID string) error {
	if _, err := k.authenticatedSubject(ctx, operatorID); err != nil {
		return err
	}
	p, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return err
	}
	if p.OwnerUserID != operatorID {
		return ErrUnauthorized.Wrap("only the process owner may grant process authority")
	}
	if err := k.store.GrantProcessAuthority(ctx, subjectID, processID); err != nil {
		return err
	}
	k.log.With(ctx).Info("process.authority_granted", "process_id", processID, "subject_id", subjectID)
	return nil
}

// RevokeProcessAuthority removes explicit call authority over a process from a user.
// Only the process owner may revoke.
func (k *Kernel) RevokeProcessAuthority(ctx context.Context, operatorID, subjectID, processID string) error {
	if _, err := k.authenticatedSubject(ctx, operatorID); err != nil {
		return err
	}
	p, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return err
	}
	if p.OwnerUserID != operatorID {
		return ErrUnauthorized.Wrap("only the process owner may revoke process authority")
	}
	if err := k.store.RevokeProcessAuthority(ctx, subjectID, processID); err != nil {
		return err
	}
	k.log.With(ctx).Info("process.authority_revoked", "process_id", processID, "subject_id", subjectID)
	return nil
}

// ReadProcess returns a process by ID, requiring the caller to be its owner.
func (k *Kernel) ReadProcess(ctx context.Context, subjectID, id string) (*Process, error) {
	p, err := k.store.ReadProcess(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.OwnerUserID != subjectID {
		return nil, ErrUnauthorized.Wrap("not authorized to view this process")
	}
	return p, nil
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

// RateTransaction submits a rating for a completed transaction.
// Only the direct buyer (the process owner who paid) may rate.
// Ratings are stored in a separate ratings table; the transaction row is never modified.
func (k *Kernel) RateTransaction(ctx context.Context, subjectID, txID string, rating float64) (*Rating, error) {
	if rating != 0 && rating != 1 {
		return nil, ErrInvalidInput.Wrap("rating must be 0 or 1")
	}
	if _, err := k.authenticatedSubject(ctx, subjectID); err != nil {
		return nil, err
	}
	tx, err := k.store.ReadTransaction(ctx, txID)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, ErrNotFound.Wrap("transaction not found")
	}
	// Only the direct buyer (process owner) may rate.
	if subjectID != tx.OwnerUserID {
		return nil, ErrUnauthorized.Wrap("only the direct buyer may rate a transaction")
	}
	// A subject may not rate its own output.
	if subjectID == tx.TargetUserID {
		return nil, ErrUnauthorized.Wrap("subject may not rate its own output")
	}
	// Check for duplicate rating (transaction already has a rating record).
	if existing, _ := k.store.ReadRatingByTxID(ctx, txID); existing != nil {
		return nil, ErrInvalidInput.Wrap("transaction already rated")
	}
	// Look up receipt for this transaction (may be nil for old transactions).
	receipt, _ := k.store.ReadReceiptByTxID(ctx, txID)
	r := &Rating{
		ID:          uuid.New().String(),
		RatedTxID:   txID,
		RaterUserID: subjectID,
		Rating:      rating,
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if receipt != nil {
		r.RatedReceiptID = &receipt.ID
	}
	sig, err := signRating(k.cfg.SigningKey, r)
	if err != nil {
		return nil, err
	}
	r.Signature = sig
	if err := k.store.CreateRatingAndUpdateStats(ctx, r, tx.ActionID, rating); err != nil {
		return nil, err
	}
	return r, nil
}

// ListRatings returns ratings for an action ordered by creation time descending.
func (k *Kernel) ListRatings(ctx context.Context, actionID string, limit, offset int) ([]*Rating, error) {
	return k.store.ListRatings(ctx, actionID, limit, offset)
}

// ---- Stats ----

// ReadStats returns statistics for an action.
func (k *Kernel) ReadStats(ctx context.Context, actionID string) (*Stats, error) {
	return k.store.ReadStats(ctx, actionID)
}

// ResetActionStats resets the statistics row for an action to zero counters.
func (k *Kernel) ResetActionStats(ctx context.Context, actionID string) error {
	return k.store.UpsertStats(ctx, &Stats{ActionID: actionID})
}

// ---- Lookup ----

// LookupRequest is a natural-language query for actions.
type LookupRequest struct {
	Query     string
	Limit     int
	Offset    int
	SubjectID string // authenticated caller; used to include owned and ACL-granted actions
}

// LookupResult is a ranked action for a lookup query.
type LookupResult struct {
	Action      *Action
	OwnerHandle string
	Score       float32
}

// Lookup returns active actions ranked by semantic similarity to the query.
// Embeddings are pre-stored at activation time; only actions with a stored vector
// are ranked. Returns ErrInvalidState if no embedder is configured.
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

	embeddings, err := k.store.ListEmbeddings(ctx)
	if err != nil {
		return nil, err
	}

	type candidate struct {
		actionID string
		score    float32
	}
	scored := make([]candidate, 0, len(embeddings))
	for actionID, vec := range embeddings {
		scored = append(scored, candidate{actionID: actionID, score: cosine(qvec, vec)})
	}

	// Sort by cosine similarity and oversample for quality re-ranking.
	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	oversub := limit * 10
	if oversub > len(scored) {
		oversub = len(scored)
	}
	scored = scored[:oversub]

	// Apply quality factor (success rate) to the oversampled candidates.
	for i := range scored {
		quality := float32(0.5)
		if stats, _ := k.store.ReadStats(ctx, scored[i].actionID); stats != nil && stats.Uses > 0 {
			quality = float32(0.5 + 0.5*float64(stats.Successes)/float64(stats.Uses))
		}
		scored[i].score *= quality
	}

	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	if len(scored) > limit {
		scored = scored[:limit]
	}

	out := make([]*LookupResult, 0, len(scored))
	ownerHandles := make(map[string]string)
	for _, c := range scored {
		a, err := k.store.ReadAction(ctx, c.actionID)
		if err != nil {
			continue
		}
		// Only include actions the subject can call per CanCall rule.
		if !a.Public {
			if req.SubjectID == "" || (a.OwnerUserID != req.SubjectID) {
				ok, _ := k.canCall(ctx, req.SubjectID, a)
				if !ok {
					continue
				}
			}
		}
		if _, cached := ownerHandles[a.OwnerUserID]; !cached {
			if u, err := k.store.ReadUser(ctx, a.OwnerUserID); err == nil {
				ownerHandles[a.OwnerUserID] = u.Handle
			}
		}
		out = append(out, &LookupResult{Action: a, OwnerHandle: ownerHandles[a.OwnerUserID], Score: c.score})
	}
	return out, nil
}

// storeEmbedding embeds the description and persists the vector. Best-effort: logs on failure, never returns an error.
func (k *Kernel) storeEmbedding(ctx context.Context, actionID, description string) {
	if k.llm == nil || strings.TrimSpace(description) == "" {
		return
	}
	vec, err := k.llm.Embed(ctx, description)
	if err != nil {
		k.log.With(ctx).Warn("lookup.embed_failed", "action_id", actionID, "error", err.Error())
		return
	}
	if err := k.store.UpsertEmbedding(ctx, actionID, vec); err != nil {
		k.log.With(ctx).Warn("lookup.embed_store_failed", "action_id", actionID, "error", err.Error())
	}
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

// authenticatedSubject reads the subject user and rejects missing or suspended users.
// All supervision operations call this first so that Authenticated(s) ∧ ¬Suspended(s)
// is a kernel-level invariant, not just an adapter-level check.
func (k *Kernel) authenticatedSubject(ctx context.Context, subjectID string) (*User, error) {
	u, err := k.store.ReadUser(ctx, subjectID)
	if err != nil {
		return nil, ErrUnauthenticated.Wrap("subject not found")
	}
	if u.SuspendedAt != nil {
		return nil, ErrUnauthenticated.Wrap("account suspended")
	}
	return u, nil
}

// isUserSuperuser returns true if u is the configured platform superuser.
func (k *Kernel) isUserSuperuser(ctx context.Context, u *User) bool {
	handle := k.cfg.SuperuserHandle
	if handle == "" {
		handle, _ = k.store.GetConfig(ctx, "superuser_handle")
	}
	return handle != "" && u.Handle == handle
}

// requireAdmin returns nil if subjectID is authenticated, non-suspended, and is the owner
// of a, the platform superuser, or holds admin ACL on a.
func (k *Kernel) requireAdmin(ctx context.Context, subjectID string, a *Action) error {
	u, err := k.authenticatedSubject(ctx, subjectID)
	if err != nil {
		return err
	}
	if a.OwnerUserID == subjectID || k.isUserSuperuser(ctx, u) {
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

// requireSelf returns nil if subjectID is authenticated, non-suspended, and equals ownerID
// or is the platform superuser.
func (k *Kernel) requireSelf(ctx context.Context, subjectID, ownerID string) error {
	u, err := k.authenticatedSubject(ctx, subjectID)
	if err != nil {
		return err
	}
	if u.ID == ownerID || k.isUserSuperuser(ctx, u) {
		return nil
	}
	return ErrUnauthorized.Wrap("cannot act on behalf of another user")
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
		s.PriceMean = IncrementalMean(s.PriceMean, s.Successes-1, float64(tx.Gross))
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
	SourceUserID   string
	EventName      string
	TargetActionID string
}

// CreateListener registers a new listener owned by subjectID and returns it.
func (k *Kernel) CreateListener(ctx context.Context, subjectID string, req CreateListenerRequest) (*Listener, error) {
	if _, err := k.authenticatedSubject(ctx, subjectID); err != nil {
		return nil, err
	}
	if req.EventName == "" {
		return nil, ErrInvalidInput.Wrap("event_name is required")
	}
	if req.SourceUserID == "" {
		return nil, ErrInvalidInput.Wrap("source_user_id is required")
	}
	if _, err := k.store.ReadUser(ctx, req.SourceUserID); err != nil {
		return nil, ErrNotFound.Wrap("source_user_id not found")
	}
	a, err := k.store.ReadAction(ctx, req.TargetActionID)
	if err != nil {
		return nil, err
	}
	if a.OwnerUserID != subjectID {
		ok, err := k.canCall(ctx, subjectID, a)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrUnauthorized.Wrap("call permission required to register listener")
		}
	}
	l := &Listener{
		ID:             uuid.New().String(),
		OwnerUserID:    subjectID,
		SourceUserID:   req.SourceUserID,
		EventName:      req.EventName,
		TargetActionID: req.TargetActionID,
		Active:         true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := k.store.CreateListener(ctx, l); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("listener.created", "listener_id", l.ID, "event", req.EventName, "status", "success")
	return l, nil
}

// PollListener returns the pending (unconsumed) events for a listener.
func (k *Kernel) PollListener(ctx context.Context, subjectID, listenerID string) ([]*Event, error) {
	if _, err := k.authenticatedSubject(ctx, subjectID); err != nil {
		return nil, err
	}
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
	if _, err := k.authenticatedSubject(ctx, subjectID); err != nil {
		return err
	}
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
	if _, err := k.authenticatedSubject(ctx, subjectID); err != nil {
		return nil, err
	}
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

// InsertPendingIdempotencyRecord inserts a record with status="pending" before execution.
func (k *Kernel) InsertPendingIdempotencyRecord(ctx context.Context, r *IdempotencyRecord) error {
	return k.store.InsertPendingIdempotencyRecord(ctx, r)
}

// DeleteIdempotencyRecord removes a record to allow retry after execution failure.
func (k *Kernel) DeleteIdempotencyRecord(ctx context.Context, id string) error {
	return k.store.DeleteIdempotencyRecord(ctx, id)
}

// SignFederation signs a federation payload with the platform key and returns
// (signature, timestamp). Returns an error if the signing key is not configured.
func (k *Kernel) SignFederation(action, idempotencyKey string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignFederationPayload(k.cfg.SigningKey, action, idempotencyKey, ts)
	return
}

// EmitEvent queues an event for all active listeners matching (sourceUserID, eventName).
// It does NOT call the target action — the listener owner must call ConsumeEvent explicitly.
// causingTraceID is stored as a FOLLOWS_FROM reference on each event record.
// All events are inserted atomically: either every active listener receives its event or none do.
func (k *Kernel) EmitEvent(ctx context.Context, subjectID, sourceUserID, eventName string, args map[string]any, causingTraceID string) ([]string, error) {
	if err := k.requireSelf(ctx, subjectID, sourceUserID); err != nil {
		return nil, err
	}
	listeners, err := k.store.ListListeners(ctx, sourceUserID, eventName)
	if err != nil {
		return nil, err
	}
	argsJSON, _ := json.Marshal(args)
	now := time.Now().UTC()
	var events []*Event
	for _, l := range listeners {
		if !l.Active {
			continue
		}
		events = append(events, &Event{
			ID:             uuid.New().String(),
			ListenerID:     l.ID,
			ArgsJSON:       json.RawMessage(argsJSON),
			CausingTraceID: causingTraceID,
			CreatedAt:      now,
		})
	}
	if len(events) == 0 {
		k.log.With(ctx).Info("event.emitted", "source", sourceUserID, "event", eventName, "queued", 0)
		return nil, nil
	}
	if err := k.store.CreateEvents(ctx, events); err != nil {
		return nil, err
	}
	eventIDs := make([]string, len(events))
	for i, e := range events {
		eventIDs[i] = e.ID
	}
	k.log.With(ctx).Info("event.emitted", "source", sourceUserID, "event", eventName, "queued", len(eventIDs))
	return eventIDs, nil
}

// ConsumeEvent atomically locks an event and executes its listener's target action.
// At-least-once delivery: if the action call fails, the event is reset to pending.
// processID is the caller's open process; parentTraceID optionally sets the CHILD_OF
// parent within that process (defaults to the process root when empty).
func (k *Kernel) ConsumeEvent(ctx context.Context, subjectID, eventID, processID, parentTraceID string) (*CallReply, error) {
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
	_ = json.Unmarshal(e.ArgsJSON, &args)
	// Call the action using the supplied process. EventID causes CommitCall to settle
	// the event atomically in the same transaction, eliminating the double-charge window.
	reply, err := k.Call(ctx, CallRequest{
		SubjectID:       subjectID,
		ProcessID:       processID,
		ParentTraceID:   parentTraceID,
		CausedByTraceID: e.CausingTraceID,
		TargetUserID:    owner.ID,
		ActionName:      action.Name,
		Args:            args,
		EventID:         eventID,
	})
	if err != nil {
		_ = k.store.UnlockEvent(ctx, eventID)
		return nil, err
	}
	k.log.With(ctx).Info("event.consumed", "event_id", eventID, "tx_id", reply.TxID, "status", "success")
	return reply, nil
}

// ---- Receipt helpers ----

// buildReceipt constructs a Receipt from a committed transaction and signs it.
// Returns ErrInvalidState if the kernel has not been bootstrapped (no issuer configured).
func (k *Kernel) buildReceipt(tx *Transaction) (*Receipt, error) {
	if err := k.requireReceiptSigningReady(); err != nil {
		return nil, err
	}
	argsHash, err := jcsHashStr(string(tx.ArgsJSON))
	if err != nil {
		return nil, ErrInternal.Wrapf("hash args: %v", err)
	}
	replyHash, err := jcsHashStr(string(tx.ReplyJSON))
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
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
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

// signReceipt signs the canonical Receipt object (with Signature cleared) using JCS.
func signReceipt(key ed25519.PrivateKey, r *Receipt) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	cp := *r
	cp.Signature = ""
	payload, err := CanonicalJSON(cp)
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize receipt: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

// signRating signs the canonical Rating object (with Signature cleared) using JCS.
func signRating(key ed25519.PrivateKey, r *Rating) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	cp := *r
	cp.Signature = ""
	payload, err := CanonicalJSON(cp)
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize rating: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

// ---- Federation operations ----

// RegisterRemoteKernel creates or updates a local user record representing a remote kernel peer.
// Only the superuser may register remote peers.
func (k *Kernel) RegisterRemoteKernel(ctx context.Context, subjectID, handle, publicKey, baseURL string) (*User, error) {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return nil, err
	}
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

// ---- Import shared logic ----

// incomingOp describes one operation from an external source (OpenAPI or federation manifest).
type incomingOp struct {
	key   string        // unique identifier: operation_key (OpenAPI) or remote_action_id (federation)
	hash  string        // content hash for change detection
	apply func(*Action) // update mutable fields on an existing action
	new   func() *Action
}

// reconcileImport applies create/update/deactivate logic given existing actions (keyed by op key)
// and incoming operations. hashOf extracts the stored content hash from an existing action.
// Used by both ImportOpenAPI and ImportRemoteAction.
func (k *Kernel) reconcileImport(ctx context.Context, existingByKey map[string]*Action, hashOf func(*Action) string, incoming []incomingOp) (*ImportResult, error) {
	incomingKeys := make(map[string]struct{}, len(incoming))
	for _, op := range incoming {
		incomingKeys[op.key] = struct{}{}
	}

	var result ImportResult

	// Deactivate existing actions whose ops were removed from the spec.
	for key, a := range existingByKey {
		if _, ok := incomingKeys[key]; ok {
			continue
		}
		a.Active = false
		a.UpdatedAt = time.Now().UTC()
		if err := k.store.UpdateAction(ctx, a); err != nil {
			return nil, err
		}
		if err := k.store.UpsertStats(ctx, &Stats{ActionID: a.ID}); err != nil {
			return nil, err
		}
		result.Deactivated = append(result.Deactivated, a)
	}

	// Process each incoming op.
	for _, op := range incoming {
		if ex, ok := existingByKey[op.key]; ok {
			if hashOf(ex) == op.hash {
				result.Unchanged = append(result.Unchanged, ex)
			} else {
				ex.Active = false
				ex.UpdatedAt = time.Now().UTC()
				op.apply(ex)
				if err := k.store.UpdateAction(ctx, ex); err != nil {
					return nil, err
				}
				if err := k.store.UpsertStats(ctx, &Stats{ActionID: ex.ID}); err != nil {
					return nil, err
				}
				result.Updated = append(result.Updated, ex)
			}
		} else {
			a := op.new()
			if err := k.store.CreateAction(ctx, a); err != nil {
				return nil, err
			}
			result.Created = append(result.Created, a)
		}
	}

	return &result, nil
}

// deactivateActions sets Active=false and persists each action.
// Used by both UnimportOpenAPI and UnimportRemoteAction.
func (k *Kernel) deactivateActions(ctx context.Context, actions []*Action) error {
	for _, a := range actions {
		a.Active = false
		a.UpdatedAt = time.Now().UTC()
		if err := k.store.UpdateAction(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// ---- OpenAPI import ----

// rawOp is one parsed OpenAPI operation before it is bound to an owner.
type rawOp struct {
	key          string
	description  string
	method       string
	path         string
	baseURL      string
	params       []OpenAPIParam
	inputSchema  map[string]any
	outputSchema map[string]any
	price        int64
	hash         string
	sourceJSON   string
}

// parseOpenAPISpec parses specBytes (already-fetched JSON) and returns one rawOp per supported
// operation (GET/POST/PUT/PATCH/DELETE). specURL is stored in provenance only; no HTTP is performed.
// The third return value is the base URL extracted from the spec's servers array.
func parseOpenAPISpec(specBytes []byte, specURL string) ([]rawOp, []ImportRejection, string, error) {
	var spec map[string]any
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return nil, nil, "", ErrInvalidInput.Wrap("spec is not valid JSON")
	}

	// Extract base URL from first server entry.
	baseURL := ""
	if servers, ok := spec["servers"].([]any); ok && len(servers) > 0 {
		if s, ok := servers[0].(map[string]any); ok {
			baseURL, _ = s["url"].(string)
		}
	}
	if baseURL == "" {
		u, _ := url.Parse(specURL)
		baseURL = u.Scheme + "://" + u.Host
	}
	baseURL = strings.TrimRight(baseURL, "/")

	paths, _ := spec["paths"].(map[string]any)
	var ops []rawOp
	var rejected []ImportRejection

	for path, pathItemRaw := range paths {
		pathItem, ok := pathItemRaw.(map[string]any)
		if !ok {
			continue
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			opRaw, ok := pathItem[method]
			if !ok {
				continue
			}
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}

			key := openAPIOperationKey(op, method, path)
			desc := openAPIDescription(op)
			if desc == "" {
				rejected = append(rejected, ImportRejection{Key: key, Reason: "missing description and summary"})
				continue
			}

			outputSchema, ok := openAPIOutputSchema(op)
			if !ok {
				rejected = append(rejected, ImportRejection{Key: key, Reason: "no 2xx JSON response schema"})
				continue
			}

			if secRaw, ok := op["security"]; ok {
				if secs, ok := secRaw.([]any); ok && len(secs) > 0 {
					rejected = append(rejected, ImportRejection{Key: key, Reason: "operation has security requirements"})
					continue
				}
			}

			if rb, ok := op["requestBody"].(map[string]any); ok {
				if content, ok := rb["content"].(map[string]any); ok && len(content) > 0 {
					if _, hasJSON := content["application/json"]; !hasJSON {
						rejected = append(rejected, ImportRejection{Key: key, Reason: "requestBody has no application/json content"})
						continue
					}
				}
			}

			if ambig, reason := openAPIAmbiguous2xxSchema(op); ambig {
				rejected = append(rejected, ImportRejection{Key: key, Reason: reason})
				continue
			}

			var price int64
			if v, ok := op["x-juice-price"]; ok {
				if f, ok := v.(float64); ok {
					if int64(f) < 0 || f != float64(int64(f)) {
						rejected = append(rejected, ImportRejection{Key: key, Reason: "price must be a non-negative integer"})
						continue
					}
					price = int64(f)
				}
			}

			params := openAPIParams(op, pathItem)
			inputSchema := openAPIInputSchema(op, pathItem)
			hash := openAPIOperationHash(baseURL, desc, method, path, inputSchema, outputSchema, price, params)

			src := OpenAPISource{
				Type:          "openapi",
				SpecURL:       specURL,
				BaseURL:       baseURL,
				Method:        strings.ToUpper(method),
				Path:          path,
				OperationKey:  key,
				OperationHash: hash,
				Params:        params,
			}
			srcBytes, _ := json.Marshal(src)

			ops = append(ops, rawOp{
				key:          key,
				description:  desc,
				method:       strings.ToUpper(method),
				path:         path,
				baseURL:      baseURL,
				params:       params,
				inputSchema:  inputSchema,
				outputSchema: outputSchema,
				price:        price,
				hash:         hash,
				sourceJSON:   string(srcBytes),
			})
		}
	}
	return ops, rejected, baseURL, nil
}

func openAPIOperationKey(op map[string]any, method, path string) string {
	if v, ok := op["x-juice-name"].(string); ok && v != "" {
		return v
	}
	if v, ok := op["operationId"].(string); ok && v != "" {
		return v
	}
	return openAPISlug(method + "-" + path)
}

func openAPIDescription(op map[string]any) string {
	if v, ok := op["description"].(string); ok && v != "" {
		return v
	}
	if v, ok := op["summary"].(string); ok && v != "" {
		return v
	}
	return ""
}

func openAPIOutputSchema(op map[string]any) (map[string]any, bool) {
	responses, ok := op["responses"].(map[string]any)
	if !ok {
		return nil, false
	}
	for _, code := range []string{"200", "201", "202", "203", "204"} {
		if schema := openAPIJSONSchema(responses[code]); schema != nil {
			return schema, true
		}
	}
	for code, resp := range responses {
		if len(code) == 3 && code[0] == '2' {
			if schema := openAPIJSONSchema(resp); schema != nil {
				return schema, true
			}
		}
	}
	return nil, false
}

func openAPIJSONSchema(respRaw any) map[string]any {
	resp, ok := respRaw.(map[string]any)
	if !ok {
		return nil
	}
	content, _ := resp["content"].(map[string]any)
	jsonContent, _ := content["application/json"].(map[string]any)
	schema, _ := jsonContent["schema"].(map[string]any)
	if len(schema) > 0 {
		return schema
	}
	return nil
}

func openAPIAmbiguous2xxSchema(op map[string]any) (bool, string) {
	responses, _ := op["responses"].(map[string]any)
	var seen []string
	for code, resp := range responses {
		if len(code) != 3 || code[0] != '2' {
			continue
		}
		s := openAPIJSONSchema(resp)
		if s == nil {
			continue
		}
		b, _ := json.Marshal(s)
		seen = append(seen, string(b))
	}
	if len(seen) < 2 {
		return false, ""
	}
	first := seen[0]
	for _, s := range seen[1:] {
		if s != first {
			return true, "ambiguous 2xx response schemas"
		}
	}
	return false, ""
}

func openAPIInputSchema(op, pathItem map[string]any) map[string]any {
	properties := map[string]any{}
	var required []string

	for _, source := range []map[string]any{pathItem, op} {
		params, _ := source["parameters"].([]any)
		for _, pRaw := range params {
			p, ok := pRaw.(map[string]any)
			if !ok {
				continue
			}
			in, _ := p["in"].(string)
			if in != "path" && in != "query" {
				continue
			}
			name, _ := p["name"].(string)
			if name == "" {
				continue
			}
			schema, _ := p["schema"].(map[string]any)
			if schema == nil {
				schema = map[string]any{"type": "string"}
			}
			if desc, ok := p["description"].(string); ok && desc != "" {
				schema["description"] = desc
			}
			properties[name] = schema
			if req, _ := p["required"].(bool); req || in == "path" {
				required = append(required, name)
			}
		}
	}

	if rb, ok := op["requestBody"].(map[string]any); ok {
		content, _ := rb["content"].(map[string]any)
		jc, _ := content["application/json"].(map[string]any)
		if bodySchema, ok := jc["schema"].(map[string]any); ok {
			if props, ok := bodySchema["properties"].(map[string]any); ok {
				for k, v := range props {
					properties[k] = v
				}
			}
			if reqs, ok := bodySchema["required"].([]any); ok {
				for _, r := range reqs {
					if s, ok := r.(string); ok {
						required = append(required, s)
					}
				}
			}
		}
	}

	result := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		result["required"] = required
	}
	return result
}

// openAPIParams records the binding location for each input field so the executor
// can route path params, query params, and body fields correctly regardless of HTTP method.
func openAPIParams(op, pathItem map[string]any) []OpenAPIParam {
	var params []OpenAPIParam
	seen := map[string]struct{}{}
	for _, source := range []map[string]any{pathItem, op} {
		ps, _ := source["parameters"].([]any)
		for _, pRaw := range ps {
			p, ok := pRaw.(map[string]any)
			if !ok {
				continue
			}
			in, _ := p["in"].(string)
			if in != "path" && in != "query" {
				continue
			}
			name, _ := p["name"].(string)
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			params = append(params, OpenAPIParam{Name: name, In: in})
		}
	}
	if rb, ok := op["requestBody"].(map[string]any); ok {
		content, _ := rb["content"].(map[string]any)
		jc, _ := content["application/json"].(map[string]any)
		if bodySchema, ok := jc["schema"].(map[string]any); ok {
			if props, ok := bodySchema["properties"].(map[string]any); ok {
				for name := range props {
					if _, dup := seen[name]; !dup {
						seen[name] = struct{}{}
						params = append(params, OpenAPIParam{Name: name, In: "body"})
					}
				}
			}
		}
	}
	return params
}

func openAPIOperationHash(baseURL, description, method, path string, inputSchema, outputSchema map[string]any, price int64, params []OpenAPIParam) string {
	sorted := make([]OpenAPIParam, len(params))
	copy(sorted, params)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	paramsSlice := make([]any, len(sorted))
	for i, p := range sorted {
		paramsSlice[i] = map[string]any{"in": p.In, "name": p.Name}
	}
	payload := map[string]any{
		"base_url":      baseURL,
		"description":   description,
		"input_schema":  inputSchema,
		"method":        method,
		"output_schema": outputSchema,
		"params":        paramsSlice,
		"path":          path,
		"price":         price,
	}
	b, _ := CanonicalJSON(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func openAPISlug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prev := '-'
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prev = r
		} else if prev != '-' {
			b.WriteRune('-')
			prev = '-'
		}
	}
	return strings.Trim(b.String(), "-")
}

// ImportOpenAPI parses specBytes (caller-fetched OpenAPI JSON), reconciles operations with
// existing OpenAPI-imported actions for the owner, and returns the diff. It is idempotent.
func (k *Kernel) ImportOpenAPI(ctx context.Context, subjectID, ownerID, specURL string, specBytes []byte) (*ImportResult, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("openapi.import.start", "spec_url", specURL)
	if err := k.requireSelf(ctx, subjectID, ownerID); err != nil {
		logger.Warn("openapi.import.failed", "spec_url", specURL, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	owner, err := k.store.ReadUser(ctx, ownerID)
	if err != nil {
		return nil, err
	}

	rawOps, rejected, baseURL, err := parseOpenAPISpec(specBytes, specURL)
	if err != nil {
		return nil, err
	}

	// Proof 1: well-known challenge — GET {baseURL}/.well-known/juice-owner.txt must return the owner handle.
	// Proof 2: x-juice-owner field in the spec document (embedded challenge, less strong).
	ownershipVerified := false
	if baseURL != "" {
		if uf, ok := k.http.(URLFetcher); ok {
			wkURL := strings.TrimRight(baseURL, "/") + "/.well-known/juice-owner.txt"
			if body, fetchErr := uf.FetchURL(ctx, wkURL); fetchErr == nil {
				ownershipVerified = strings.TrimSpace(string(body)) == owner.Handle
			}
		}
	}
	if !ownershipVerified {
		var specMap map[string]any
		_ = json.Unmarshal(specBytes, &specMap)
		ownershipVerified = specMap["x-juice-owner"] == owner.Handle
	}

	existing, err := k.store.ListActionsByOwnerOpenAPISpec(ctx, ownerID, specURL)
	if err != nil {
		return nil, err
	}

	existingByKey := make(map[string]*Action, len(existing))
	for _, a := range existing {
		var src OpenAPISource
		if err := json.Unmarshal([]byte(a.Source), &src); err == nil {
			existingByKey[src.OperationKey] = a
		}
	}

	hashOf := func(a *Action) string {
		var src OpenAPISource
		if err := json.Unmarshal([]byte(a.Source), &src); err == nil {
			return src.OperationHash
		}
		return ""
	}

	var incoming []incomingOp
	for _, raw := range rawOps {
		raw := raw
		name := raw.key
		// Name collision: only check for truly new ops (not already imported).
		if _, exists := existingByKey[raw.key]; !exists {
			if _, err := k.store.ReadActionByOwnerName(ctx, ownerID, name); err == nil {
				rejected = append(rejected, ImportRejection{Key: raw.key, Reason: "name collision with existing action"})
				continue
			}
		}
		sourceJSON := raw.sourceJSON
		if ownershipVerified {
			var osrc OpenAPISource
			_ = json.Unmarshal([]byte(raw.sourceJSON), &osrc)
			osrc.OwnershipVerified = true
			if b, marshalErr := json.Marshal(osrc); marshalErr == nil {
				sourceJSON = string(b)
			}
		}
		incoming = append(incoming, incomingOp{
			key:  raw.key,
			hash: raw.hash,
			apply: func(a *Action) {
				a.Description  = raw.description
				a.Price        = raw.price
				a.InputSchema  = raw.inputSchema
				a.OutputSchema = raw.outputSchema
				a.Source       = sourceJSON
			},
			new: func() *Action {
				now := time.Now().UTC()
				return &Action{
					ID:           uuid.New().String(),
					OwnerUserID:  ownerID,
					Name:         name,
					Kind:         KindHTTP,
					Active:       false,
					Description:  raw.description,
					Price:        raw.price,
					InputSchema:  raw.inputSchema,
					OutputSchema: raw.outputSchema,
					Source:       sourceJSON,
					CreatedAt:    now,
					UpdatedAt:    now,
				}
			},
		})
	}

	result, err := k.reconcileImport(ctx, existingByKey, hashOf, incoming)
	if err != nil {
		return nil, err
	}

	// Staleness fix: re-evaluate ownership on Unchanged actions too.
	// Proof state may have changed since the last import (e.g., well-known file removed).
	for _, a := range result.Unchanged {
		var src OpenAPISource
		if jsonErr := json.Unmarshal([]byte(a.Source), &src); jsonErr == nil && src.OwnershipVerified != ownershipVerified {
			src.OwnershipVerified = ownershipVerified
			if b, marshalErr := json.Marshal(src); marshalErr == nil {
				a.Source = string(b)
				a.UpdatedAt = time.Now().UTC()
				_ = k.store.UpdateAction(ctx, a)
			}
		}
	}

	result.Rejected = append(result.Rejected, rejected...)
	logger.Info("openapi.import.done", "spec_url", specURL, "created", len(result.Created), "updated", len(result.Updated), "unchanged", len(result.Unchanged), "deactivated", len(result.Deactivated), "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return result, nil
}

// UnimportOpenAPI deactivates all OpenAPI-imported actions with matching owner + spec_url.
// If name is non-empty, only actions whose name or operation_key matches are deactivated.
func (k *Kernel) UnimportOpenAPI(ctx context.Context, subjectID, ownerID, specURL, name string) ([]*Action, error) {
	if err := k.requireSelf(ctx, subjectID, ownerID); err != nil {
		return nil, err
	}
	actions, err := k.store.ListActionsByOwnerOpenAPISpec(ctx, ownerID, specURL)
	if err != nil {
		return nil, err
	}
	if name != "" {
		var filtered []*Action
		for _, a := range actions {
			var src OpenAPISource
			json.Unmarshal([]byte(a.Source), &src)
			if a.Name == name || src.OperationKey == name {
				filtered = append(filtered, a)
			}
		}
		actions = filtered
	}
	if err := k.deactivateActions(ctx, actions); err != nil {
		return nil, err
	}
	return actions, nil
}

// ---- Federation import (refactored to use reconcileImport) ----

// remoteManifestHash returns the manifest hash for a remote_proxy action: a hex-encoded
// SHA-256 over the manifest contract fields defined in §12.2 (description, input/output
// schemas, price, kind, artifact_hash, and execution identity: action_id, name,
// owner_handle). Stats and updated_at are excluded because they are not contract fields.
func remoteManifestHash(m ActionManifest) string {
	inputJSON, _ := CanonicalJSON(m.InputSchema)
	outputJSON, _ := CanonicalJSON(m.OutputSchema)
	payload, _ := CanonicalJSON(map[string]any{
		"action_id":     m.ActionID,
		"artifact_hash": m.ArtifactHash,
		"description":   m.Description,
		"input_schema":  string(inputJSON),
		"kind":          string(m.Kind),
		"name":          m.Name,
		"output_schema": string(outputJSON),
		"owner_handle":  m.OwnerHandle,
		"price":         m.Price,
	})
	h := sha256.Sum256(payload)
	return hex.EncodeToString(h[:])
}

// ImportRemoteAction creates or updates a local remote_proxy action from a remote kernel's manifest.
// The action is owned by the remote kernel user identified by remoteUserID.
// It is idempotent: re-running with the same manifest preserves the action's active state.
// Only the superuser may import remote actions.
func (k *Kernel) ImportRemoteAction(ctx context.Context, subjectID, remoteUserID string, m ActionManifest) (*ImportResult, error) {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return nil, err
	}
	remoteUser, err := k.store.ReadUser(ctx, remoteUserID)
	if err != nil {
		return nil, err
	}
	if remoteUser.RemoteBaseURL == "" {
		return nil, ErrInvalidInput.Wrap("user is not a remote kernel")
	}
	if m.ActionID == "" {
		return nil, ErrInvalidInput.Wrap("manifest missing action_id")
	}
	if remoteUser.PublicKey == "" {
		return nil, ErrInvalidInput.Wrap("remote kernel has no public key")
	}
	if err := VerifyManifestSignature(remoteUser.PublicKey, &m); err != nil {
		return nil, err
	}
	if m.Price < 0 {
		return nil, ErrInvalidInput.Wrap("price must be non-negative")
	}
	// counterparty is this kernel's base64url Ed25519 public key so the remote can
	// look it up by key (handle-based lookup would require knowing what handle the
	// remote assigned to us, which we don't have without a round-trip).
	localCounterparty := ""
	if len(k.cfg.SigningKey) == ed25519.PrivateKeySize {
		pub := k.cfg.SigningKey.Public().(ed25519.PublicKey)
		localCounterparty = base64.RawURLEncoding.EncodeToString(pub)
	}
	source := strings.TrimRight(remoteUser.RemoteBaseURL, "/") +
		"/v1/federation/call?action=" + url.QueryEscape(m.OwnerHandle+"/"+m.Name) +
		"&counterparty=" + url.QueryEscape(localCounterparty)

	existingByKey := map[string]*Action{}
	if existing, err := k.store.ReadActionByOwnerRemoteID(ctx, remoteUserID, m.ActionID); err == nil {
		existingByKey[m.ActionID] = existing
	}

	contentHash := remoteManifestHash(m)
	name := m.Name
	incoming := []incomingOp{{
		key:  m.ActionID,
		hash: contentHash,
		apply: func(a *Action) {
			a.Name         = name
			a.Source       = source
			a.Price        = m.Price
			a.Description  = m.Description
			a.InputSchema  = m.InputSchema
			a.OutputSchema = m.OutputSchema
			a.ArtifactHash = contentHash
		},
		new: func() *Action {
			now := time.Now().UTC()
			return &Action{
				ID:             uuid.New().String(),
				OwnerUserID:    remoteUserID,
				Name:           name,
				Kind:           KindRemoteProxy,
				Active:         false,
				Price:          m.Price,
				Description:    m.Description,
				InputSchema:    m.InputSchema,
				OutputSchema:   m.OutputSchema,
				Source:         source,
				ArtifactHash:   contentHash, // store content hash so reconcileImport can compare on re-import
				RemoteActionID: m.ActionID,
				CreatedAt:      now,
				UpdatedAt:      now,
			}
		},
	}}

	result, err := k.reconcileImport(ctx, existingByKey, func(a *Action) string { return a.ArtifactHash }, incoming)
	if err != nil {
		return nil, err
	}

	if len(result.Created) > 0 {
		k.log.With(ctx).Info("action.imported_remote", "action_id", result.Created[0].ID, "name", result.Created[0].Name, "status", "success")
	} else if len(result.Updated) > 0 {
		k.log.With(ctx).Info("action.reimported_remote", "action_id", result.Updated[0].ID, "name", result.Updated[0].Name, "status", "success")
	} else {
		k.log.With(ctx).Info("action.remote_unchanged", "remote_action_id", m.ActionID, "status", "success")
	}
	return result, nil
}

// UnimportRemoteAction deactivates the local proxy action for the given remote handle and action name.
func (k *Kernel) UnimportRemoteAction(ctx context.Context, subjectID, remoteHandle, actionName string) (*Action, error) {
	remoteUser, err := k.store.ReadUserByHandle(ctx, remoteHandle)
	if err != nil {
		return nil, ErrNotFound.Wrapf("remote kernel %q not found", remoteHandle)
	}
	if remoteUser.RemoteBaseURL == "" {
		return nil, ErrInvalidInput.Wrapf("%q is not a remote kernel", remoteHandle)
	}
	a, err := k.store.ReadActionByOwnerName(ctx, remoteUser.ID, actionName)
	if err != nil {
		return nil, err
	}
	if a.Kind != KindRemoteProxy {
		return nil, ErrInvalidInput.Wrap("action is not a remote proxy")
	}
	if subjectID != a.OwnerUserID {
		if err := k.requireSuperuser(ctx, subjectID); err != nil {
			return nil, ErrUnauthorized.Wrap("owner or superuser required to unimport remote action")
		}
	}
	if err := k.deactivateActions(ctx, []*Action{a}); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("action.unimported_remote", "action_id", a.ID, "name", a.Name)
	return a, nil
}

// ReconcileRemoteAction applies the remote-import policy for a single action.
// When manifest is nil (manifest endpoint non-200 or action gone), the local proxy is
// deactivated and stats are reset. When manifest is non-nil, ImportRemoteAction runs.
func (k *Kernel) ReconcileRemoteAction(ctx context.Context, subjectID, remoteHandle, actionName string, manifest *ActionManifest) (*ImportResult, error) {
	if manifest == nil {
		a, err := k.UnimportRemoteAction(ctx, subjectID, remoteHandle, actionName)
		if err != nil {
			return nil, err
		}
		_ = k.ResetActionStats(ctx, a.ID)
		return &ImportResult{}, nil
	}
	remoteUser, err := k.store.ReadUserByHandle(ctx, remoteHandle)
	if err != nil {
		return nil, ErrNotFound.Wrapf("remote kernel %q not found", remoteHandle)
	}
	return k.ImportRemoteAction(ctx, subjectID, remoteUser.ID, *manifest)
}

// GetActionManifest returns a signed manifest for a public active action.
// Manifests are only available for actions that are both active and public.
func (k *Kernel) GetActionManifest(ctx context.Context, actionID string) (*ActionManifest, error) {
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
		ActionID:     a.ID,
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
	sig, err := SignManifest(k.cfg.SigningKey, m)
	if err != nil {
		return nil, err
	}
	m.Signature = sig
	return m, nil
}

// manifestCanonicalPayload returns the canonical JCS bytes of m with Signature cleared.
func manifestCanonicalPayload(m *ActionManifest) ([]byte, error) {
	cp := *m
	cp.Signature = ""
	return CanonicalJSON(cp)
}

// SignManifest creates a base64url Ed25519 signature over the canonical ActionManifest.
func SignManifest(key ed25519.PrivateKey, m *ActionManifest) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := manifestCanonicalPayload(m)
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize manifest: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

// VerifyManifestSignature checks that m.Signature was produced by the private key
// corresponding to pubKeyB64 (base64url Ed25519 public key).
func VerifyManifestSignature(pubKeyB64 string, m *ActionManifest) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return err
	}
	payload, err := manifestCanonicalPayload(m)
	if err != nil {
		return ErrInternal.Wrapf("canonicalize manifest: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(m.Signature)
	if err != nil || !ed25519.Verify(pub, payload, sig) {
		return ErrUnauthorized.Wrap("manifest signature is invalid")
	}
	return nil
}

// VerifyFederationSignature verifies an Ed25519 signature over the canonical federation payload
// {"action": action, "idempotency_key": idempotencyKey, "timestamp": timestamp}.
func VerifyFederationSignature(pubKeyB64, action, idempotencyKey, timestamp, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	payload, err := CanonicalJSON(map[string]string{
		"action":          action,
		"idempotency_key": idempotencyKey,
		"timestamp":       timestamp,
	})
	if err != nil {
		return ErrInternal.Wrapf("canonicalize federation payload: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !ed25519.Verify(pub, payload, sig) {
		return ErrUnauthenticated.Wrap("federation signature is invalid")
	}
	return nil
}

// SignFederationPayload creates a base64url Ed25519 signature over the canonical federation payload.
func SignFederationPayload(key Ed25519PrivateKey, action, idempotencyKey, timestamp string) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := CanonicalJSON(map[string]string{
		"action":          action,
		"idempotency_key": idempotencyKey,
		"timestamp":       timestamp,
	})
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize federation payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

