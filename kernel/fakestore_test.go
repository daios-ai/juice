package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// fakeStore is an in-memory Store implementation for tests.
type fakeStore struct {
	mu                  sync.Mutex
	users               map[string]*User
	userByHandle        map[string]*User
	actions             map[string]*Action
	actionEmbeds        map[string][]float32
	acl                 map[string]map[Permission]bool // key: subjectID+":"+actionID
	processes           map[string]*Process
	processAuthorities  map[string]map[string]bool // processID -> set of subjectUserIDs
	traces              map[string]*Trace
	transactions        map[string]*Transaction
	stats               map[string]*Stats
	statTags            []*StatTag
	listeners           map[string]*Listener
	events              map[string]*Event // eventID -> Event
	authCodes           map[string]*AuthCode
	refreshTokens       map[string]*RefreshToken
	config              map[string]string
	deposits            []*Deposit
	receipts            map[string]*Receipt           // txID -> Receipt
	ratings             map[string]*Rating            // txID -> Rating
	idempotencyRecords  map[string]*IdempotencyRecord // key+counterparty -> record
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:              make(map[string]*User),
		userByHandle:       make(map[string]*User),
		actions:            make(map[string]*Action),
		actionEmbeds:       make(map[string][]float32),
		acl:                make(map[string]map[Permission]bool),
		processes:          make(map[string]*Process),
		processAuthorities: make(map[string]map[string]bool),
		traces:             make(map[string]*Trace),
		transactions:       make(map[string]*Transaction),
		stats:              make(map[string]*Stats),
		listeners:          make(map[string]*Listener),
		events:             make(map[string]*Event),
		authCodes:          make(map[string]*AuthCode),
		refreshTokens:      make(map[string]*RefreshToken),
		config:             make(map[string]string),
		receipts:           make(map[string]*Receipt),
		ratings:            make(map[string]*Rating),
		idempotencyRecords: make(map[string]*IdempotencyRecord),
	}
}

func (f *fakeStore) CreateUser(_ context.Context, u *User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.userByHandle[u.Handle]; exists {
		return fmt.Errorf("handle already exists")
	}
	cp := *u
	f.users[u.ID] = &cp
	f.userByHandle[u.Handle] = &cp
	return nil
}

func (f *fakeStore) ReadUser(_ context.Context, id string) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return nil, ErrNotFound.Wrap("user not found")
	}
	cp := *u
	return &cp, nil
}

func (f *fakeStore) ReadUserByHandle(_ context.Context, handle string) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.userByHandle[handle]
	if !ok {
		return nil, ErrNotFound.Wrap("user not found")
	}
	cp := *u
	return &cp, nil
}

func (f *fakeStore) CreateAction(_ context.Context, a *Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *a
	f.actions[a.ID] = &cp
	return nil
}

func (f *fakeStore) ReadAction(_ context.Context, id string) (*Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.actions[id]
	if !ok {
		return nil, ErrNotFound.Wrap("action not found")
	}
	cp := *a
	return &cp, nil
}

func (f *fakeStore) ReadActionByOwnerName(_ context.Context, ownerID, name string) (*Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.actions {
		if a.OwnerUserID == ownerID && a.Name == name {
			cp := *a
			return &cp, nil
		}
	}
	return nil, ErrNotFound.Wrap("action not found")
}

func (f *fakeStore) ReadActionByOwnerRemoteID(_ context.Context, ownerID, remoteActionID string) (*Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.actions {
		if a.OwnerUserID == ownerID && a.RemoteActionID == remoteActionID && remoteActionID != "" {
			cp := *a
			return &cp, nil
		}
	}
	return nil, ErrNotFound.Wrap("action not found")
}

func (f *fakeStore) ListActionsByOwnerOpenAPISpec(_ context.Context, ownerID, specURL string) ([]*Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Action
	for _, a := range f.actions {
		if a.DeletedAt != nil || a.OwnerUserID != ownerID {
			continue
		}
		var src OpenAPISource
		if err := json.Unmarshal([]byte(a.Source), &src); err != nil {
			continue
		}
		if src.Type == "openapi" && src.SpecURL == specURL {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateAction(_ context.Context, a *Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.actions[a.ID]; !ok {
		return ErrNotFound.Wrap("action not found")
	}
	cp := *a
	f.actions[a.ID] = &cp
	return nil
}

func (f *fakeStore) DeleteAction(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.actions, id)
	suffix := ":" + id
	for key := range f.acl {
		if len(key) >= len(suffix) && key[len(key)-len(suffix):] == suffix {
			delete(f.acl, key)
		}
	}
	return nil
}

func (f *fakeStore) ListActions(_ context.Context, activeOnly bool, limit, offset int) ([]*Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Action
	for _, a := range f.actions {
		if activeOnly && (!a.Active || !a.Public) {
			continue
		}
		cp := *a
		result = append(result, &cp)
	}
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeStore) UpdateActionEmbedding(_ context.Context, actionID string, vec []float32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]float32, len(vec))
	copy(cp, vec)
	f.actionEmbeds[actionID] = cp
	return nil
}

func (f *fakeStore) ListActionEmbeddings(_ context.Context, limit int) (map[string][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]float32, len(f.actionEmbeds))
	count := 0
	for id, vec := range f.actionEmbeds {
		if limit > 0 && count >= limit {
			break
		}
		a, ok := f.actions[id]
		if !ok || !a.Active {
			continue
		}
		cp := make([]float32, len(vec))
		copy(cp, vec)
		out[id] = cp
		count++
	}
	return out, nil
}

func (f *fakeStore) GrantACL(_ context.Context, e *ACLEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := e.SubjectUserID + ":" + e.ActionID
	if f.acl[key] == nil {
		f.acl[key] = make(map[Permission]bool)
	}
	f.acl[key][e.Permission] = true
	return nil
}

func (f *fakeStore) RevokeACL(_ context.Context, subjectID, actionID string, perm Permission) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := subjectID + ":" + actionID
	if m, ok := f.acl[key]; ok {
		delete(m, perm)
	}
	return nil
}

func (f *fakeStore) CheckACL(_ context.Context, subjectID, actionID string, perm Permission) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := subjectID + ":" + actionID
	m, ok := f.acl[key]
	if !ok {
		return false, nil
	}
	return m[perm], nil
}

func (f *fakeStore) GrantProcessAuthority(_ context.Context, subjectUserID, processID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.processAuthorities[processID] == nil {
		f.processAuthorities[processID] = make(map[string]bool)
	}
	f.processAuthorities[processID][subjectUserID] = true
	return nil
}

func (f *fakeStore) RevokeProcessAuthority(_ context.Context, subjectUserID, processID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.processAuthorities[processID]; ok {
		delete(m, subjectUserID)
	}
	return nil
}

func (f *fakeStore) CheckProcessAuthority(_ context.Context, subjectUserID, processID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.processAuthorities[processID]
	if !ok {
		return false, nil
	}
	return m[subjectUserID], nil
}

func (f *fakeStore) StartProcess(_ context.Context, p *Process, t *Trace, ownerID string, funds int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if funds > 0 {
		u, ok := f.users[ownerID]
		if !ok {
			return ErrNotFound.Wrap("owner not found")
		}
		if u.Available < funds {
			return ErrInsufficientFunds.Wrap("insufficient user balance")
		}
		u.Available -= funds
		u.Locked += funds
	}
	cp := *p
	cp.Available = funds
	f.processes[p.ID] = &cp
	tc := *t
	f.traces[t.ID] = &tc
	return nil
}

func (f *fakeStore) ReadProcess(_ context.Context, id string) (*Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.processes[id]
	if !ok {
		return nil, ErrNotFound.Wrap("process not found")
	}
	cp := *p
	return &cp, nil
}

func (f *fakeStore) BeginCall(_ context.Context, processID string, t *Trace, price int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.processes[processID]
	if !ok {
		return ErrNotFound.Wrap("process not found")
	}
	if price > 0 {
		if p.Available < price {
			return ErrInsufficientFunds.Wrap("insufficient process funds")
		}
		p.Available -= price
		p.Locked += price
	}
	cp := *t
	f.traces[t.ID] = &cp
	return nil
}

func (f *fakeStore) RefundFunds(_ context.Context, processID string, amount int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.processes[processID]
	if !ok {
		return ErrNotFound.Wrap("process not found")
	}
	p.Locked -= amount
	p.Available += amount
	return nil
}

func (f *fakeStore) FundProcess(_ context.Context, userID, processID string, amount int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	if !ok {
		return ErrNotFound.Wrap("user not found")
	}
	if u.Available < amount {
		return ErrInsufficientFunds.Wrap("insufficient user funds")
	}
	p, ok := f.processes[processID]
	if !ok {
		return ErrNotFound.Wrap("process not found")
	}
	u.Available -= amount
	u.Locked += amount
	p.Available += amount
	return nil
}

func (f *fakeStore) CommitFailedCall(_ context.Context, tx *Transaction, receipt *Receipt, processID string, gross int64, stats *Stats, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.processes[processID]
	if !ok {
		return ErrNotFound.Wrap("process not found")
	}
	if gross > 0 {
		p.Locked -= gross
		p.Available += gross
	}
	cp := *tx
	f.transactions[tx.ID] = &cp
	if receipt != nil {
		rcp := *receipt
		f.receipts[receipt.TxID] = &rcp
	}
	f.applyTraceLatency(tx.TraceID, 0, tx.EndedAt)
	if stats != nil {
		f.stats[stats.ActionID] = stats
	}
	return nil
}

func (f *fakeStore) CommitCall(_ context.Context, tx *Transaction, receipt *Receipt, processID, targetUserID, feeRecipientID string, net, fee int64, stats *Stats, eventID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.processes[processID]
	if !ok {
		return ErrNotFound.Wrap("process not found")
	}
	gross := net + fee
	if p.Locked < gross {
		return ErrInsufficientFunds.Wrap("insufficient locked funds")
	}
	p.Locked -= gross
	owner, ok := f.users[p.OwnerUserID]
	if !ok {
		return ErrNotFound.Wrap("process owner not found")
	}
	owner.Locked -= gross
	if net > 0 {
		target, ok := f.users[targetUserID]
		if !ok {
			return ErrNotFound.Wrap("target user not found")
		}
		target.Available += net
	}
	if fee > 0 && feeRecipientID != "" {
		if recip, ok := f.users[feeRecipientID]; ok {
			recip.Available += fee
		}
	}
	cp := *tx
	f.transactions[tx.ID] = &cp
	if receipt != nil {
		rcp := *receipt
		f.receipts[receipt.TxID] = &rcp
	}
	f.applyTraceLatency(tx.TraceID, tx.Gross, tx.EndedAt)
	if stats != nil {
		f.stats[stats.ActionID] = stats
	}
	if eventID != "" {
		if e, ok := f.events[eventID]; ok {
			txID := tx.ID
			e.TxID = &txID
		}
	}
	return nil
}

func (f *fakeStore) EndProcess(_ context.Context, processID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.processes[processID]
	if !ok {
		return ErrNotFound.Wrap("process not found")
	}
	refund := p.Available + p.Locked
	if refund > 0 {
		u, ok := f.users[p.OwnerUserID]
		if !ok {
			return ErrNotFound.Wrap("owner not found")
		}
		u.Available += refund
		u.Locked -= refund
	}
	p.Available = 0
	p.Locked = 0
	p.Status = ProcessClosed
	return nil
}

func (f *fakeStore) ReadTrace(_ context.Context, id string) (*Trace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.traces[id]
	if !ok {
		return nil, ErrNotFound.Wrap("trace not found")
	}
	cp := *t
	return &cp, nil
}

func (f *fakeStore) ReadRootTrace(_ context.Context, processID string) (*Trace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.traces {
		if t.ProcessID == processID && t.ParentTraceID == t.ID {
			cp := *t
			return &cp, nil
		}
	}
	return nil, ErrNotFound.Wrap("root trace not found for process")
}

func (f *fakeStore) ReadTransaction(_ context.Context, id string) (*Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx, ok := f.transactions[id]
	if !ok {
		return nil, ErrNotFound.Wrap("transaction not found")
	}
	cp := *tx
	return &cp, nil
}

func (f *fakeStore) ListTransactions(_ context.Context, filter TxFilter) ([]*Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Transaction
	for _, tx := range f.transactions {
		if filter.OwnerUserID != "" && tx.OwnerUserID != filter.OwnerUserID {
			continue
		}
		if filter.ProcessID != "" && tx.ProcessID != filter.ProcessID {
			continue
		}
		cp := *tx
		result = append(result, &cp)
	}
	return result, nil
}

func (f *fakeStore) ReadStats(_ context.Context, actionID string) (*Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.stats[actionID]
	if !ok {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (f *fakeStore) UpsertStats(_ context.Context, s *Stats) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *s
	f.stats[s.ActionID] = &cp
	return nil
}

func (f *fakeStore) UpsertStatTag(_ context.Context, tag *StatTag) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statTags = append(f.statTags, tag)
	return nil
}

func (f *fakeStore) CreateListener(_ context.Context, l *Listener) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *l
	f.listeners[l.ID] = &cp
	return nil
}

func (f *fakeStore) ReadListener(_ context.Context, id string) (*Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.listeners[id]
	if !ok {
		return nil, ErrNotFound.Wrap("listener not found")
	}
	cp := *l
	return &cp, nil
}

func (f *fakeStore) ListListeners(_ context.Context, sourceUserID, eventName string) ([]*Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Listener
	for _, l := range f.listeners {
		if l.SourceUserID == sourceUserID && l.EventName == eventName {
			cp := *l
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (f *fakeStore) CreateEvents(_ context.Context, events []*Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range events {
		cp := *e
		cp.CreatedAt = time.Now().UTC()
		f.events[e.ID] = &cp
	}
	return nil
}

func (f *fakeStore) ReadEvent(_ context.Context, id string) (*Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.events[id]
	if !ok {
		return nil, ErrNotFound.Wrap("event not found")
	}
	cp := *e
	return &cp, nil
}

func (f *fakeStore) ListPendingEvents(_ context.Context, listenerID string) ([]*Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Event
	for _, e := range f.events {
		if e.ListenerID == listenerID && e.ConsumedAt == nil {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeStore) LockEvent(_ context.Context, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.events[eventID]
	if !ok {
		return ErrNotFound.Wrap("event not found")
	}
	if e.ConsumedAt != nil {
		return ErrInvalidState.Wrap("event already consumed or in-flight")
	}
	now := time.Now().UTC()
	e.ConsumedAt = &now
	return nil
}

func (f *fakeStore) UnlockEvent(_ context.Context, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.events[eventID]
	if !ok {
		return ErrNotFound.Wrap("event not found")
	}
	if e.TxID != nil {
		return nil // already settled, don't unlock
	}
	e.ConsumedAt = nil
	return nil
}

func (f *fakeStore) DeleteListenerWithEvents(_ context.Context, listenerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.listeners[listenerID]; ok {
		l.Active = false
	}
	for id, e := range f.events {
		if e.ListenerID == listenerID && e.ConsumedAt == nil {
			delete(f.events, id)
		}
	}
	return nil
}

func (f *fakeStore) ResetInFlightEvents(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.ConsumedAt != nil && e.TxID == nil {
			e.ConsumedAt = nil
		}
	}
	return nil
}

func (f *fakeStore) ListTraces(_ context.Context, processID string) ([]*Trace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Trace
	for _, t := range f.traces {
		if t.ProcessID == processID {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeStore) CreateAuthCode(_ context.Context, c *AuthCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *c
	f.authCodes[c.Code] = &cp
	return nil
}

func (f *fakeStore) ConsumeAuthCode(_ context.Context, code string) (*AuthCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ac, ok := f.authCodes[code]
	if !ok || ac.Used {
		return nil, ErrUnauthenticated.Wrap("invalid or used auth code")
	}
	ac.Used = true
	cp := *ac
	return &cp, nil
}

func (f *fakeStore) CreateRefreshToken(_ context.Context, t *RefreshToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *t
	f.refreshTokens[t.Token] = &cp
	return nil
}

func (f *fakeStore) RotateRefreshToken(_ context.Context, oldToken string) (*RefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.refreshTokens[oldToken]
	if !ok || rt.Revoked {
		return nil, ErrUnauthenticated.Wrap("invalid refresh token")
	}
	rt.Revoked = true
	// Issue a simple new token for tests.
	newRT := &RefreshToken{
		Token:     oldToken + "-rotated",
		UserID:    rt.UserID,
		ExpiresAt: rt.ExpiresAt,
		CreatedAt: rt.CreatedAt,
	}
	f.refreshTokens[newRT.Token] = newRT
	cp := *newRT
	return &cp, nil
}

func (f *fakeStore) RevokeRefreshToken(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.refreshTokens[token]
	if !ok || rt.Revoked {
		return ErrUnauthenticated.Wrap("invalid or already revoked refresh token")
	}
	rt.Revoked = true
	return nil
}

// ---- New methods (admin / grant-all / config) ----

func (f *fakeStore) ListUsers(_ context.Context, limit, offset int) ([]*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*User
	for _, u := range f.users {
		cp := *u
		result = append(result, &cp)
	}
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeStore) SuspendUser(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return ErrNotFound.Wrap("user not found")
	}
	now := time.Now().UTC()
	u.SuspendedAt = &now
	if h, ok2 := f.userByHandle[u.Handle]; ok2 {
		h.SuspendedAt = &now
	}
	return nil
}

func (f *fakeStore) UnsuspendUser(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return ErrNotFound.Wrap("user not found")
	}
	u.SuspendedAt = nil
	if h, ok2 := f.userByHandle[u.Handle]; ok2 {
		h.SuspendedAt = nil
	}
	return nil
}

func (f *fakeStore) ListAllActions(_ context.Context, limit, offset int) ([]*Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Action
	for _, a := range f.actions {
		cp := *a
		result = append(result, &cp)
	}
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeStore) ListProcesses(_ context.Context, ownerID string, limit, offset int) ([]*Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Process
	for _, p := range f.processes {
		if p.OwnerUserID == ownerID {
			cp := *p
			result = append(result, &cp)
		}
	}
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeStore) ListListenersByOwner(_ context.Context, ownerID string, limit, offset int) ([]*Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Listener
	for _, l := range f.listeners {
		if l.OwnerUserID == ownerID {
			cp := *l
			result = append(result, &cp)
		}
	}
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeStore) ListAllProcesses(_ context.Context, limit, offset int) ([]*Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Process
	for _, p := range f.processes {
		cp := *p
		result = append(result, &cp)
	}
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeStore) ListAllTransactions(_ context.Context, limit, offset int) ([]*Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Transaction
	for _, tx := range f.transactions {
		cp := *tx
		result = append(result, &cp)
	}
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// applyTraceLatency walks up the trace ancestor chain and updates cost and latency.
// Must be called with f.mu held.
func (f *fakeStore) applyTraceLatency(traceID string, grossDelta int64, endedAt time.Time) {
	cur := traceID
	for {
		t, ok := f.traces[cur]
		if !ok {
			break
		}
		latencyMS := endedAt.Sub(t.CreatedAt).Milliseconds()
		t.Cost += grossDelta
		if latencyMS > t.LatencyMS {
			t.LatencyMS = latencyMS
		}
		if t.ParentTraceID == cur {
			break
		}
		cur = t.ParentTraceID
	}
}

func (f *fakeStore) GetConfig(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.config[key]
	if !ok {
		return "", ErrNotFound.Wrapf("config key %q not found", key)
	}
	return v, nil
}

func (f *fakeStore) SetConfig(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config[key] = value
	return nil
}

func (f *fakeStore) InitFirstBoot(_ context.Context, u *User, configs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.userByHandle[u.Handle]; !exists {
		cp := *u
		f.users[u.ID] = &cp
		f.userByHandle[u.Handle] = &cp
	}
	for k, v := range configs {
		f.config[k] = v
	}
	return nil
}

func (f *fakeStore) ReadReceipt(_ context.Context, id string) (*Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.receipts {
		if r.ID == id {
			cp := *r
			return &cp, nil
		}
	}
	return nil, ErrNotFound.Wrap("receipt not found")
}

func (f *fakeStore) CreateDeposit(_ context.Context, d *Deposit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[d.TargetUserID]
	if !ok {
		return ErrNotFound.Wrap("user not found")
	}
	u.Available += d.Amount
	cp := *d
	f.deposits = append(f.deposits, &cp)
	return nil
}

func (f *fakeStore) ReadUserByPublicKey(_ context.Context, publicKey string) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.PublicKey == publicKey {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound.Wrap("user not found")
}

func (f *fakeStore) UpdateUser(_ context.Context, u *User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[u.ID]; !ok {
		return ErrNotFound.Wrap("user not found")
	}
	cp := *u
	f.users[u.ID] = &cp
	f.userByHandle[u.Handle] = &cp
	return nil
}

func (f *fakeStore) ReadReceiptByTxID(_ context.Context, txID string) (*Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.receipts[txID]
	if !ok {
		return nil, ErrNotFound.Wrap("receipt not found")
	}
	cp := *r
	return &cp, nil
}

func (f *fakeStore) CreateRatingAndUpdateStats(_ context.Context, r *Rating, actionID string, rating float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.ratings[r.RatedTxID]; exists {
		return ErrInvalidState.Wrap("already rated")
	}
	cp := *r
	f.ratings[r.RatedTxID] = &cp
	s, ok := f.stats[actionID]
	if ok {
		s.RatingCount++
		s.RatingMean = IncrementalMean(s.RatingMean, s.RatingCount-1, rating)
	}
	return nil
}

func (f *fakeStore) ReadRatingByTxID(_ context.Context, txID string) (*Rating, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.ratings[txID]
	if !ok {
		return nil, ErrNotFound.Wrap("rating not found")
	}
	cp := *r
	return &cp, nil
}

func (f *fakeStore) ListRatings(_ context.Context, actionID string, limit, offset int) ([]*Rating, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Rating
	for _, r := range f.ratings {
		tx, ok := f.transactions[r.RatedTxID]
		if ok && tx.ActionID == actionID {
			cp := *r
			result = append(result, &cp)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeStore) InsertPendingIdempotencyRecord(_ context.Context, r *IdempotencyRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.IdempotencyKey + ":" + r.CounterpartyUserID
	if _, exists := f.idempotencyRecords[key]; exists {
		return fmt.Errorf("UNIQUE constraint failed: idempotency_records.idempotency_key, idempotency_records.counterparty_user_id")
	}
	cp := *r
	cp.Status = "pending"
	cp.ResultJSON = ""
	f.idempotencyRecords[key] = &cp
	return nil
}

func (f *fakeStore) DeleteIdempotencyRecord(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, r := range f.idempotencyRecords {
		if r.ID == id {
			delete(f.idempotencyRecords, k)
			return nil
		}
	}
	return ErrNotFound.Wrap("idempotency record not found")
}

func (f *fakeStore) ReadIdempotencyRecord(_ context.Context, key, counterpartyUserID string) (*IdempotencyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key + ":" + counterpartyUserID
	r, ok := f.idempotencyRecords[k]
	if !ok {
		return nil, ErrNotFound.Wrap("idempotency record not found")
	}
	if time.Now().UTC().After(r.ExpiresAt) {
		return nil, ErrNotFound.Wrap("idempotency record expired")
	}
	cp := *r
	return &cp, nil
}
