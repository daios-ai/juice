package kernel

import (
	"context"
	"encoding/json"
	"time"

	"github.com/daios/juice/log"
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
	k.log.Info("user.created", "user_id", u.ID, "handle", u.Handle)
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
	tok, err := IssueToken(u.ID, k.cfg.TokenSecret, k.cfg.TokenTTL)
	if err != nil {
		return "", err
	}
	k.log.Info("user.login", "user_id", u.ID)
	return tok, nil
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
func (k *Kernel) CreateAction(ctx context.Context, req CreateActionRequest) (*Action, error) {
	if req.Name == "" {
		return nil, ErrInvalidInput.Wrap("name is required")
	}
	if req.Kind != KindHTTP && req.Kind != KindWasm && req.Kind != KindNative {
		return nil, ErrInvalidInput.Wrapf("unknown kind %q", req.Kind)
	}
	if req.Price < 0 {
		return nil, ErrInvalidInput.Wrap("price must be non-negative")
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
	k.log.Info("action.created", "action_id", a.ID, "name", a.Name)
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
	return k.store.UpdateAction(ctx, a)
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
	return k.store.DeleteAction(ctx, actionID)
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
	return k.store.GrantACL(ctx, &ACLEntry{
		SubjectUserID: subjectID,
		ActionID:      actionID,
		Permission:    perm,
		CreatedAt:     time.Now().UTC(),
	})
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
	return k.store.RevokeACL(ctx, subjectID, actionID, perm)
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

	k.log.Info("process.started", "process_id", p.ID, "owner", ownerID, "funds", funds)
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
	return k.store.FundProcess(ctx, subjectID, processID, funds)
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
	k.log.Info("process.ended", "process_id", processID)
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

// RateTransaction sets a rating on a completed transaction.
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
	return k.store.UpdateTransaction(ctx, tx)
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

// Lookup returns active actions ranked by semantic similarity to the query.
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
	actions, err := k.store.ListActions(ctx, true, 200, 0)
	if err != nil {
		return nil, err
	}

	type scored struct {
		a     *Action
		score float32
	}
	results := make([]scored, 0, len(actions))
	for _, a := range actions {
		desc := a.Description
		if desc == "" {
			continue
		}
		vec, err := k.llm.Embed(ctx, desc)
		if err != nil {
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
	if x <= 0 {
		return 0
	}
	// Newton-Raphson — sufficient precision for ranking.
	z := x
	for i := 0; i < 10; i++ {
		z = (z + x/z) / 2
	}
	return z
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

// jsonMarshal is a thin wrapper so call.go doesn't import encoding/json directly.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
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
	ProcessID      string
	TraceID        string
	TargetActionID string
}

// CreateListener registers a new listener and returns it.
func (k *Kernel) CreateListener(ctx context.Context, req CreateListenerRequest) (*Listener, error) {
	if req.EventName == "" {
		return nil, ErrInvalidInput.Wrap("event_name is required")
	}
	p, err := k.store.ReadProcess(ctx, req.ProcessID)
	if err != nil {
		return nil, err
	}
	if p.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}
	if p.OwnerUserID != req.OwnerUserID {
		return nil, ErrUnauthorized.Wrap("only the process owner may create listeners")
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
		ProcessID:      req.ProcessID,
		TraceID:        req.TraceID,
		TargetActionID: req.TargetActionID,
		Active:         true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := k.store.CreateListener(ctx, l); err != nil {
		return nil, err
	}
	k.log.Info("listener.created", "listener_id", l.ID, "event", req.EventName)
	return l, nil
}

// PollListener returns the queued transaction IDs for a listener.
func (k *Kernel) PollListener(ctx context.Context, subjectID, listenerID string) ([]string, error) {
	l, err := k.store.ReadListener(ctx, listenerID)
	if err != nil {
		return nil, err
	}
	if l.OwnerUserID != subjectID && l.SourceUserID != subjectID {
		return nil, ErrUnauthorized.Wrap("not authorized to poll this listener")
	}
	return k.store.ReadEvents(ctx, listenerID)
}

// DeleteListener deactivates a listener.
func (k *Kernel) DeleteListener(ctx context.Context, subjectID, listenerID string) error {
	l, err := k.store.ReadListener(ctx, listenerID)
	if err != nil {
		return err
	}
	if l.OwnerUserID != subjectID {
		return ErrUnauthorized.Wrap("only the listener owner may remove it")
	}
	l.Active = false
	return k.store.UpdateListener(ctx, l)
}

// EmitEvent fires all active listeners matching (sourceUserID, eventName).
func (k *Kernel) EmitEvent(ctx context.Context, sourceUserID, eventName string, args map[string]any) ([]string, error) {
	listeners, err := k.store.ListListeners(ctx, sourceUserID, eventName)
	if err != nil {
		return nil, err
	}
	var txIDs []string
	for _, l := range listeners {
		if !l.Active {
			continue
		}
		action, err := k.store.ReadAction(ctx, l.TargetActionID)
		if err != nil {
			k.log.Warn("emit.listener_skip", "listener_id", l.ID, "error", err.Error())
			continue
		}
		owner, err := k.store.ReadUser(ctx, action.OwnerUserID)
		if err != nil {
			k.log.Warn("emit.listener_skip", "listener_id", l.ID, "error", err.Error())
			continue
		}
		reply, err := k.Call(ctx, CallRequest{
			SubjectID:     l.OwnerUserID,
			ProcessID:     l.ProcessID,
			ParentTraceID: l.TraceID,
			TargetUserID:  owner.ID,
			ActionName:    action.Name,
			Args:          args,
		})
		if err != nil {
			k.log.Warn("emit.call_failed", "listener_id", l.ID, "event", eventName, "error", err.Error())
			continue
		}
		_ = k.store.AppendEvent(ctx, l.ID, reply.TxID)
		txIDs = append(txIDs, reply.TxID)
	}
	k.log.Info("event.emitted", "source", sourceUserID, "event", eventName, "fired", len(txIDs))
	return txIDs, nil
}

// ---- Recursive feedback ----

// RecursiveFeedback computes recursive cost and latency for a trace subtree.
func (k *Kernel) RecursiveFeedback(ctx context.Context, processID, traceID string) (*TraceFeedback, error) {
	traces, err := k.store.ListTraces(ctx, processID)
	if err != nil {
		return nil, err
	}
	children := make(map[string][]string)
	for _, t := range traces {
		if t.ID == t.ParentTraceID {
			continue
		}
		children[t.ParentTraceID] = append(children[t.ParentTraceID], t.ID)
	}
	subtree := collectSubtree(traceID, children)

	txs, err := k.store.ListTransactions(ctx, TxFilter{ProcessID: processID, Limit: 10000})
	if err != nil {
		return nil, err
	}
	var rootStartedAt time.Time
	for _, tx := range txs {
		if tx.TraceID == traceID {
			rootStartedAt = tx.StartedAt
			break
		}
	}
	var totalCost int64
	var maxEndedAt time.Time
	for _, tx := range txs {
		if !subtree[tx.TraceID] {
			continue
		}
		totalCost += tx.Gross
		if tx.EndedAt.After(maxEndedAt) {
			maxEndedAt = tx.EndedAt
		}
	}
	var latency float64
	if !rootStartedAt.IsZero() && !maxEndedAt.IsZero() {
		latency = maxEndedAt.Sub(rootStartedAt).Seconds()
	}
	return &TraceFeedback{
		TraceID:          traceID,
		RecursiveCost:    totalCost,
		RecursiveLatency: latency,
	}, nil
}

func collectSubtree(root string, children map[string][]string) map[string]bool {
	visited := make(map[string]bool)
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		queue = append(queue, children[cur]...)
	}
	return visited
}
