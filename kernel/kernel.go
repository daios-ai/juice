package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	AuthIssuer        string             // JUICE_AUTH_ISSUER — iss claim in JWTs; empty = no claim
	AuthAudience      string             // JUICE_AUTH_AUDIENCE — aud claim in JWTs; empty = no validation
}

// DefaultConfig returns safe local defaults.
func DefaultConfig() Config {
	return Config{
		FeeBPS:        2000,
		TokenTTL:      15 * time.Minute,
		ScriptTimeout: 10 * time.Second,
		ScriptMemory:  64 * 1024 * 1024, // 64 MiB
	}
}

// AllowsLocalSources reports whether the kernel is configured to permit loopback/private source URLs.
func (k *Kernel) AllowsLocalSources() bool { return k.cfg.AllowLocalSources }

// NativeFunc is the signature for a registered native action handler.
// targetID is the action's owner; callerID is the call caller; ownerUserID is the process owner.
type NativeFunc func(ctx context.Context, args map[string]any, targetID, callerID, ownerUserID, processID, parentTraceID string) (map[string]any, error)

// Kernel is the central service object.
// It holds all dependencies and exposes operations to both the CLI and HTTP server.
type Kernel struct {
	store          Store
	scripts        ScriptExecutor
	http           HTTPExecutor
	llm            Embedder
	cfg            Config
	log            *log.Logger
	nativeHandlers map[string]NativeFunc
}

// New constructs a Kernel. scripts, http, and llm may be nil if those features are unused.
func New(store Store, scripts ScriptExecutor, http HTTPExecutor, llm Embedder, cfg Config, logger *log.Logger) *Kernel {
	if logger == nil {
		logger = log.Default()
	}
	return &Kernel{
		store:          store,
		scripts:        scripts,
		http:           http,
		llm:            llm,
		cfg:            cfg,
		log:            logger,
		nativeHandlers: make(map[string]NativeFunc),
	}
}

// RegisterNativeHandler registers a native action handler by action name.
// Call from bootstrap to wire each native action without touching call.go.
func (k *Kernel) RegisterNativeHandler(name string, fn NativeFunc) {
	k.nativeHandlers[name] = fn
}

// SetSigningKey stores the Ed25519 signing key and issuer user ID after bootstrap completes.
func (k *Kernel) SetSigningKey(priv ed25519.PrivateKey, issuerUserID string) {
	k.cfg.SigningKey = priv
	k.cfg.IssuerUserID = issuerUserID
	k.cfg.FeeRecipientID = issuerUserID
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

// validateHandle rejects empty handles and handles containing /, enforcing the
// invariant that @owner/name references are unambiguous (handles ≡ hostnames, no /).
func validateHandle(handle string) error {
	if handle == "" {
		return ErrInvalidInput.Wrap("handle is required")
	}
	if strings.Contains(handle, "/") {
		return ErrInvalidInput.Wrap("handle must not contain /")
	}
	return nil
}

// CreateUser creates a new user account and returns the user.
func (k *Kernel) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("user.create.start", "handle", req.Handle)
	if err := validateHandle(req.Handle); err != nil {
		return nil, err
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
	u, err := k.requireActiveUser(ctx, operatorID)
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
// ValidateFeeRecipient returns an error if the fee configuration is inconsistent.
// fee_bps > 0 requires a non-empty fee_recipient_id that resolves to a known user.
// Call after bootstrap to reject misconfiguration before serving requests.
func (k *Kernel) ValidateFeeRecipient(ctx context.Context) error {
	if k.cfg.FeeBPS == 0 {
		return nil
	}
	if k.cfg.FeeRecipientID == "" {
		return ErrInvalidState.Wrap("fee_bps > 0 requires fee_recipient_id to be set")
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
	WasmArtifact string // base64-encoded pre-compiled WASM; if set, stored as-is and used for the hash
}

// lookupHostFn resolves a hostname to IP addresses. Overridable in tests.
var lookupHostFn = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// validateHTTPSource rejects URLs that could be used for SSRF attacks.
// Allowed: http and https schemes with public hostnames or literal public IPs.
// Rejected: other schemes, localhost, loopback, RFC 1918 private, and link-local addresses.
// For hostname (non-literal-IP) sources, DNS is resolved to catch SSRF via private hostnames.
// DNS failures are allowed through; the runtime dialer re-validates at call time.
func validateHTTPSource(ctx context.Context, source string, allowLocal bool) error {
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
		} else {
			// Resolve the hostname and reject if any address is private/loopback/link-local.
			if addrs, err := lookupHostFn(ctx, host); err == nil {
				for _, a := range addrs {
					if ip := net.ParseIP(a); ip != nil {
						if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
							return ErrInvalidInput.Wrap("URL must not target private or reserved addresses")
						}
					}
				}
			}
		}
	}
	return nil
}

func (k *Kernel) CreateAction(ctx context.Context, callerID string, req CreateActionRequest) (*Action, error) {
	if err := k.requireSelf(ctx, callerID, req.OwnerUserID); err != nil {
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
		if err := validateHTTPSource(ctx, req.Source, k.cfg.AllowLocalSources); err != nil {
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

	if req.Kind == KindWasm && k.scripts != nil {
		wasmBytes := []byte(req.Source)
		if req.WasmArtifact != "" {
			decoded, err := base64.StdEncoding.DecodeString(req.WasmArtifact)
			if err != nil {
				return nil, ErrInvalidInput.Wrapf("wasm artifact invalid: %v", err)
			}
			wasmBytes = decoded
			a.WasmArtifact = req.WasmArtifact
		}
		if len(wasmBytes) > 0 {
			_, hash, err := k.scripts.Compile(ctx, wasmBytes)
			if err != nil {
				return nil, ErrInvalidInput.Wrapf("wasm compilation failed: %v", err)
			}
			a.ArtifactHash = hash
		}
	}

	if err := k.store.CreateAction(ctx, a); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("action.created", "action_id", a.ID, "name", a.Name, "status", "success")
	return a, nil
}

// ResetInFlightCalls restores locked process funds to available. Called at startup.
func (k *Kernel) ResetInFlightCalls(ctx context.Context) error {
	return k.store.ResetInFlightCalls(ctx)
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

// validateAndInitActivation validates schema descriptions and ensures a stats row exists.
// Called by both SetActive and ActivateNativeAction to eliminate duplicated checks.
// Errors from validateSchemaDescriptions are returned as-is (ErrSchemaViolation).
func (k *Kernel) validateAndInitActivation(ctx context.Context, a *Action) error {
	if err := validateSchemaDescriptions(a.InputSchema, "input"); err != nil {
		return err
	}
	if err := validateSchemaDescriptions(a.OutputSchema, "output"); err != nil {
		return err
	}
	stats, _ := k.store.ReadStats(ctx, a.ID)
	if stats == nil {
		if err := k.store.UpsertStats(ctx, DefaultStats(a.ID)); err != nil {
			return err
		}
	}
	return nil
}

// ActivateNativeAction reconciles spec fields and activates a native action for bootstrap use.
// It overwrites description, inputSchema, and outputSchema so schema drift is corrected on every boot.
func (k *Kernel) ActivateNativeAction(ctx context.Context, actionID, description string, inputSchema, outputSchema map[string]any) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if a.Kind != KindNative {
		return ErrInvalidInput.Wrap("action is not native")
	}
	a.Description = description
	a.InputSchema = inputSchema
	a.OutputSchema = outputSchema
	a.Public = true
	if err := k.validateAndInitActivation(ctx, a); err != nil {
		return err
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

// ReadAction returns the action with the given ID (no authorization check).
// Used internally; external callers should use ReadActionForSubject.
func (k *Kernel) ReadAction(ctx context.Context, id string) (*Action, error) {
	return k.store.ReadAction(ctx, id)
}

// ReadActionForSubject returns an action only if the subject has read access.
// Public actions are readable by anyone; private actions only by their owner or the superuser.
func (k *Kernel) ReadActionForSubject(ctx context.Context, callerID, actionID string) (*Action, error) {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if a.Public || a.OwnerUserID == callerID {
		return a, nil
	}
	if u, err := k.store.ReadUser(ctx, callerID); err == nil && k.isUserSuperuser(ctx, u) {
		return a, nil
	}
	return nil, ErrUnauthorized.Wrap("read permission denied")
}

// ReadActionByOwnerName returns an action by (ownerID, name).
func (k *Kernel) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*Action, error) {
	return k.store.ReadActionByOwnerName(ctx, ownerID, name)
}

// ReadCallableAction resolves an @owner/name reference and returns the action only if
// canCall(processOwnerID, action) is satisfied. Used by native actions to discover
// composable actions without bypassing the kernel's access-control layer.
func (k *Kernel) ReadCallableAction(ctx context.Context, ownerHandle, actionName, processOwnerID string) (*Action, error) {
	owner, err := k.store.ReadUserByHandle(ctx, ownerHandle)
	if err != nil {
		return nil, ErrNotFound.Wrap("action owner not found")
	}
	a, err := k.store.ReadActionByOwnerName(ctx, owner.ID, actionName)
	if err != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	if !canCall(processOwnerID, a) {
		return nil, ErrUnauthorized.Wrap("action not callable by this process")
	}
	return a, nil
}

// ListPublicActions returns public active actions.
func (k *Kernel) ListPublicActions(ctx context.Context, limit, offset int) ([]*Action, error) {
	return k.store.ListPublicActions(ctx, limit, offset)
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
	k.SetSigningKey(ed25519.PrivateKey(storedPriv), su.ID)
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
	Public       *bool
}

// UpdateAction modifies an action and deactivates it (schema/source changes require re-activation).
func (k *Kernel) UpdateAction(ctx context.Context, callerID string, req UpdateActionRequest) (*Action, error) {
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
	if err := k.requireAdmin(ctx, callerID, a); err != nil {
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
		if a.Kind == KindHTTP {
			if err := validateHTTPSource(ctx, *req.Source, k.cfg.AllowLocalSources); err != nil {
				return nil, err
			}
		}
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
	if req.Public != nil {
		a.Public = *req.Public
		if err := requireOpenAPIOwnershipIfPublic(a); err != nil {
			return nil, err
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
func (k *Kernel) SetActive(ctx context.Context, callerID, actionID string, active bool) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if a.Kind == KindNative {
		return ErrUnauthorized.Wrap("native actions are managed by bootstrap")
	}
	if err := k.requireAdmin(ctx, callerID, a); err != nil {
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
		if err := k.validateAndInitActivation(ctx, a); err != nil {
			return err
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
			if err := validateHTTPSource(ctx, src, k.cfg.AllowLocalSources); err != nil {
				return err
			}
		}
		if a.Kind == KindWasm {
			if k.scripts == nil {
				return ErrInvalidState.Wrap("cannot activate wasm action: script executor not configured")
			}
			wasmBytes := []byte(a.Source)
			if a.WasmArtifact != "" {
				// Artifact pre-stored (e.g. by @sys/make); compile it for the hash, not the TinyGo source.
				decoded, decErr := base64.StdEncoding.DecodeString(a.WasmArtifact)
				if decErr != nil {
					return ErrInvalidState.Wrapf("wasm artifact decode failed: %v", decErr)
				}
				wasmBytes = decoded
			}
			_, hash, err := k.scripts.Compile(ctx, wasmBytes)
			if err != nil {
				return ErrInvalidState.Wrapf("wasm compile failed: %v", err)
			}
			a.ArtifactHash = hash
		}
		if err := requireOpenAPIOwnershipIfPublic(a); err != nil {
			return err
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
func (k *Kernel) DeleteAction(ctx context.Context, callerID, actionID string) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if a.Kind == KindNative {
		return ErrUnauthorized.Wrap("native actions are managed by bootstrap")
	}
	if err := k.requireAdmin(ctx, callerID, a); err != nil {
		return err
	}
	if err := k.store.DeleteAction(ctx, actionID); err != nil {
		return err
	}
	k.log.With(ctx).Info("action.deleted", "action_id", actionID, "status", "success")
	return nil
}

// ---- Process operations ----

// StartProcess creates a new process and locks funds from the owner's account.
// Process creation, user debit, and root trace creation are atomic.
func (k *Kernel) StartProcess(ctx context.Context, callerID, ownerID string, funds int64) (*Process, *Trace, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("process.start.start", "owner", ownerID, "funds", funds)
	if err := k.requireSelf(ctx, callerID, ownerID); err != nil {
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
	// Root trace: ParentTraceID == nil.
	t := &Trace{
		ID:        uuid.New().String(),
		ProcessID: p.ID,
		CreatedAt: now,
	}

	if err := k.store.StartProcess(ctx, p, t, ownerID, funds); err != nil {
		logger.Warn("process.start.failed", "owner", ownerID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, nil, err
	}
	p.Available = funds

	logger.Info("process.started", "process_id", p.ID, "owner", ownerID, "funds", funds, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return p, t, nil
}

// FundProcess adds more credits to an existing open process.
func (k *Kernel) FundProcess(ctx context.Context, callerID, processID string, funds int64) error {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return err
	}
	p, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return err
	}
	if p.Status != ProcessOpen {
		return ErrInvalidState.Wrap("process is closed")
	}
	if p.OwnerUserID != callerID {
		return ErrUnauthorized.Wrap("only the process owner may add funds")
	}
	if funds <= 0 {
		return ErrInvalidInput.Wrap("funds must be positive")
	}
	if err := k.store.FundProcess(ctx, callerID, processID, funds); err != nil {
		return err
	}
	k.log.With(ctx).Info("process.funded", "process_id", processID, "funds", funds, "status", "success")
	return nil
}

// EndProcess closes a process and returns all remaining funds to the owner.
func (k *Kernel) EndProcess(ctx context.Context, callerID, processID string) error {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return err
	}
	p, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return err
	}
	if p.OwnerUserID != callerID {
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

// ReadProcess returns a process by ID, requiring the caller to be its owner.
func (k *Kernel) ReadProcess(ctx context.Context, callerID, id string) (*Process, error) {
	p, err := k.store.ReadProcess(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.OwnerUserID != callerID {
		return nil, ErrUnauthorized.Wrap("not authorized to view this process")
	}
	return p, nil
}

// ---- Transaction operations ----

// ReadTransaction returns a transaction by ID with embedded rating, checking subject authority.
func (k *Kernel) ReadTransaction(ctx context.Context, callerID, txID string) (*TransactionView, error) {
	tx, err := k.store.ReadTransaction(ctx, txID)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, ErrNotFound.Wrap("transaction not found")
	}
	if !k.canReadTransaction(ctx, callerID, tx) {
		return nil, ErrNotFound.Wrap("transaction not found")
	}
	return k.toTransactionView(ctx, tx), nil
}

// canReadTransaction reports whether callerID is a party to tx — process owner, call caller,
// or action owner — or a superuser. All checks use immutable transaction fields.
func (k *Kernel) canReadTransaction(ctx context.Context, callerID string, tx *Transaction) bool {
	if tx.OwnerUserID == callerID || tx.CallerUserID == callerID || tx.TargetUserID == callerID {
		return true
	}
	if u, err := k.store.ReadUser(ctx, callerID); err == nil && u != nil && k.isUserSuperuser(ctx, u) {
		return true
	}
	return false
}

// ListTransactions returns transactions visible to callerID, matching the filter, each with an embedded rating.
// Superusers see all transactions; ordinary callers are restricted to transactions where they are a party.
func (k *Kernel) ListTransactions(ctx context.Context, callerID string, filter TxFilter) ([]*TransactionView, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if !k.isUserSuperuser(ctx, u) {
		filter.PartyUserID = callerID
	}
	txs, err := k.store.ListTransactions(ctx, filter)
	if err != nil {
		return nil, err
	}
	views := make([]*TransactionView, len(txs))
	for i, tx := range txs {
		views[i] = k.toTransactionView(ctx, tx)
	}
	return views, nil
}

// toTransactionView wraps a Transaction with its associated rating (if any).
func (k *Kernel) toTransactionView(ctx context.Context, tx *Transaction) *TransactionView {
	v := &TransactionView{Transaction: tx}
	if r, err := k.store.ReadRatingByTxID(ctx, tx.ID); err == nil && r != nil {
		v.Rating = &EmbeddedRating{Value: r.Rating, Note: r.Note}
	}
	return v
}

// RateTransaction submits a rating for a completed transaction.
// Only the direct buyer (the process owner who paid) may rate.
// Ratings are stored in a separate ratings table; the transaction row is never modified.
func (k *Kernel) RateTransaction(ctx context.Context, callerID, txID string, rating float64, note *string) (*Rating, error) {
	if rating != 0 && rating != 1 {
		return nil, ErrInvalidInput.Wrap("rating must be 0 or 1")
	}
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
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
	if callerID != tx.OwnerUserID {
		return nil, ErrUnauthorized.Wrap("only the direct buyer may rate a transaction")
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
		RaterUserID: callerID,
		Rating:      rating,
		Note:        note,
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
	CallerID string // authenticated caller
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
		if !canCall(req.CallerID, a) {
			continue
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

// requireActiveUser rejects missing or suspended users.
func (k *Kernel) requireActiveUser(ctx context.Context, userID string) (*User, error) {
	u, err := k.store.ReadUser(ctx, userID)
	if err != nil {
		return nil, ErrUnauthenticated.Wrap("user not found")
	}
	if u.SuspendedAt != nil {
		return nil, ErrUnauthenticated.Wrap("account suspended")
	}
	return u, nil
}

// isUserSuperuser returns true if u is the platform superuser (@sys is fixed by the spec).
func (k *Kernel) isUserSuperuser(_ context.Context, u *User) bool {
	return u.Handle == "@sys"
}

// requireAdmin returns nil if callerID is authenticated, non-suspended, and is the owner
// of a or the platform superuser.
func (k *Kernel) requireAdmin(ctx context.Context, callerID string, a *Action) error {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return err
	}
	if a.OwnerUserID == callerID || k.isUserSuperuser(ctx, u) {
		return nil
	}
	return ErrUnauthorized.Wrap("owner or superuser required")
}

// requireSelf returns nil if callerID is authenticated, non-suspended, and equals ownerID
// or is the platform superuser.
func (k *Kernel) requireSelf(ctx context.Context, callerID, ownerID string) error {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return err
	}
	if u.ID == ownerID || k.isUserSuperuser(ctx, u) {
		return nil
	}
	return ErrUnauthorized.Wrap("cannot act on behalf of another user")
}

// requireOpenAPIOwnershipIfPublic returns ErrUnauthorized if a is a public OpenAPI action
// whose ownership has not been verified. This prevents making unverified API imports public.
func requireOpenAPIOwnershipIfPublic(a *Action) error {
	if !a.Public {
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(a.Source), "{") {
		return nil
	}
	var osrc OpenAPISource
	if jsonErr := json.Unmarshal([]byte(a.Source), &osrc); jsonErr != nil || osrc.Type != "openapi" {
		return nil
	}
	if !osrc.OwnershipVerified {
		return ErrUnauthorized.Wrap("ownership not verified: add x-juice-owner to spec")
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

// ---- Receipts ----

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

// CompleteIdempotencyRecordIfPending transitions a pending idempotency record to complete.
// If the record is already complete (CommitFailedCall already ran), this is a no-op.
func (k *Kernel) CompleteIdempotencyRecordIfPending(ctx context.Context, id, resultJSON, receiptJSON string) error {
	return k.store.CompleteIdempotencyRecordIfPending(ctx, id, resultJSON, receiptJSON)
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
		CallerUserID: tx.CallerUserID,
		ProcessID:    tx.ProcessID,
		ArgsHash:     argsHash,
		ReplyHash:    replyHash,
		Status:       tx.Status,
		Gross:        tx.Gross,
		Net:          tx.Net,
		Fee:          tx.Fee,
		Reason:       tx.Reason,
		StartedAt:    tx.StartedAt,
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
// resetStats controls whether changed or stale actions have their stats row zeroed:
// true for OpenAPI (contract change invalidates prior stats), false for remote (local usage stats are preserved).
// Used by both ImportOpenAPI and ImportRemoteAction.
func (k *Kernel) reconcileImport(ctx context.Context, existingByKey map[string]*Action, hashOf func(*Action) string, incoming []incomingOp, resetStats bool) (*ImportResult, error) {
	incomingKeys := make(map[string]struct{}, len(incoming))
	for _, op := range incoming {
		incomingKeys[op.key] = struct{}{}
	}

	var result ImportResult

	// Deactivate existing actions whose ops were removed from the spec.
	var stale []*Action
	for key, a := range existingByKey {
		if _, ok := incomingKeys[key]; ok {
			continue
		}
		stale = append(stale, a)
	}
	if err := k.deactivateImported(ctx, stale, resetStats); err != nil {
		return nil, err
	}
	result.Deactivated = append(result.Deactivated, stale...)

	// Process each incoming op.
	for _, op := range incoming {
		if ex, ok := existingByKey[op.key]; ok {
			if hashOf(ex) == op.hash {
				result.Unchanged = append(result.Unchanged, ex)
			} else {
				ex.Active = false
				ex.UpdatedAt = time.Now().UTC()
				op.apply(ex)
				if resetStats {
					if err := k.store.UpdateActionAndResetStats(ctx, ex); err != nil {
						return nil, err
					}
				} else {
					if err := k.store.UpdateAction(ctx, ex); err != nil {
						return nil, err
					}
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

// deactivateImported sets Active=false for each action and optionally resets its stats.
// Invariant: unimport ⇒ active=false ∧ history unchanged.
func (k *Kernel) deactivateImported(ctx context.Context, actions []*Action, resetStats bool) error {
	for _, a := range actions {
		a.Active = false
		a.UpdatedAt = time.Now().UTC()
		if resetStats {
			if err := k.store.UpdateActionAndResetStats(ctx, a); err != nil {
				return err
			}
		} else {
			if err := k.store.UpdateAction(ctx, a); err != nil {
				return err
			}
		}
	}
	return nil
}
