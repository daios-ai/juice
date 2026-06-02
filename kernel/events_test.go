package kernel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func setupActiveWasmAction(t *testing.T, st *fakeStore, ownerID, name string) *Action {
	t.Helper()
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Name:        name,
		Kind:        KindWasm,
		Active:      true,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCreateListener(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 500)
	source := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, owner.ID, "/handler")

	l, err := k.CreateListener(ctx, owner.ID, CreateListenerRequest{
		SourceUserID:   source.ID,
		EventName:      "ping",
		TargetActionID: a.ID,
	})
	if err != nil {
		t.Fatalf("CreateListener: %v", err)
	}
	if l.ID == "" || !l.Active {
		t.Error("expected active listener with ID")
	}
}

func TestDeleteListener(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 100)
	source := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, owner.ID, "/handler")

	l, _ := k.CreateListener(ctx, owner.ID, CreateListenerRequest{
		SourceUserID: source.ID, EventName: "ping",
		TargetActionID: a.ID,
	})

	if err := k.DeleteListener(ctx, owner.ID, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteListener(ctx, source.ID, l.ID); err == nil {
		t.Error("expected authorization error for non-owner")
	}
}

// TestEmitQueuesNotFires verifies that EmitEvent creates event records without
// calling the target action. The action is only called on ConsumeEvent.
func TestEmitQueuesNotFires(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"fired":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, _, _ := k.StartProcess(ctx, alice.ID, alice.ID, 500)

	l, err := k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: bob.ID, EventName: "greet",
		TargetActionID: a.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	eventIDs, err := k.EmitEvent(ctx, bob.ID, bob.ID, "greet", map[string]any{"msg": "hello"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(eventIDs) != 1 {
		t.Errorf("expected 1 event ID from emit, got %d", len(eventIDs))
	}

	// No transactions should exist yet — emit only queues.
	txs, _ := st.ListTransactions(ctx, TxFilter{})
	if len(txs) != 0 {
		t.Errorf("expected 0 transactions after emit, got %d (action was fired prematurely)", len(txs))
	}

	// PollListener should return 1 pending event.
	events, err := k.PollListener(ctx, alice.ID, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 pending event, got %d", len(events))
	}
	if events[0].ID != eventIDs[0] {
		t.Errorf("poll event ID: got %q, want %q", events[0].ID, eventIDs[0])
	}
	_ = p
}

func TestEmitInactiveListenerNotQueued(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")

	l, _ := k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: bob.ID, EventName: "greet",
		TargetActionID: a.ID,
	})
	_ = k.DeleteListener(ctx, alice.ID, l.ID)

	eventIDs, _ := k.EmitEvent(ctx, bob.ID, bob.ID, "greet", nil, "")
	if len(eventIDs) != 0 {
		t.Errorf("expected 0 events queued for inactive listener, got %d", len(eventIDs))
	}
}

// TestConsumeEventSuccess verifies the full consume path: lock → call → settle.
func TestConsumeEventSuccess(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, _, _ := k.StartProcess(ctx, alice.ID, alice.ID, 500)

	l, _ := k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: bob.ID, EventName: "greet",
		TargetActionID: a.ID,
	})

	eventIDs, _ := k.EmitEvent(ctx, bob.ID, bob.ID, "greet", map[string]any{"msg": "hi"}, "")
	if len(eventIDs) != 1 {
		t.Fatal("emit failed")
	}

	reply, err := k.ConsumeEvent(ctx, alice.ID, eventIDs[0], p.ID, "")
	if err != nil {
		t.Fatalf("ConsumeEvent: %v", err)
	}
	if reply.TxID == "" {
		t.Error("expected non-empty tx ID")
	}

	// Event should now be settled (consumed_at set, tx_id set).
	e, err := st.ReadEvent(ctx, eventIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if e.ConsumedAt == nil {
		t.Error("event.consumed_at should be set after consume")
	}
	if e.TxID == nil || *e.TxID != reply.TxID {
		t.Errorf("event.tx_id: got %v, want %q", e.TxID, reply.TxID)
	}

	// PollListener should return 0 pending events.
	pending, _ := k.PollListener(ctx, alice.ID, l.ID)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending events after consume, got %d", len(pending))
	}
}

// TestConsumeEventAlreadyConsumed verifies that a second consume returns ErrInvalidState.
func TestConsumeEventAlreadyConsumed(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, _, _ := k.StartProcess(ctx, alice.ID, alice.ID, 500)
	_, _ = k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: bob.ID, EventName: "x",
		TargetActionID: a.ID,
	})

	eventIDs, _ := k.EmitEvent(ctx, bob.ID, bob.ID, "x", nil, "")
	_, err := k.ConsumeEvent(ctx, alice.ID, eventIDs[0], p.ID, "")
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	_, err = k.ConsumeEvent(ctx, alice.ID, eventIDs[0], p.ID, "")
	if err == nil {
		t.Error("expected ErrInvalidState on second consume, got nil")
	}
}

// TestConsumeEventUnauthorized verifies non-owners cannot consume.
func TestConsumeEventUnauthorized(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, _, _ := k.StartProcess(ctx, alice.ID, alice.ID, 500)
	_, _ = k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: bob.ID, EventName: "x",
		TargetActionID: a.ID,
	})

	eventIDs, _ := k.EmitEvent(ctx, bob.ID, bob.ID, "x", nil, "")
	_, err := k.ConsumeEvent(ctx, bob.ID, eventIDs[0], p.ID, "")
	if err == nil {
		t.Error("expected unauthorized error, got nil")
	}
}

// TestDeleteListenerPurgesEvents verifies pending events are removed when a listener is deleted.
func TestDeleteListenerPurgesEvents(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 500)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	l, _ := k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: bob.ID, EventName: "x",
		TargetActionID: a.ID,
	})

	// Queue two events.
	k.EmitEvent(ctx, bob.ID, bob.ID, "x", nil, "")
	k.EmitEvent(ctx, bob.ID, bob.ID, "x", nil, "")

	pending, _ := st.ListPendingEvents(ctx, l.ID)
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending events before delete, got %d", len(pending))
	}

	// Delete listener — must purge pending events.
	if err := k.DeleteListener(ctx, alice.ID, l.ID); err != nil {
		t.Fatal(err)
	}

	pending, _ = st.ListPendingEvents(ctx, l.ID)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending events after listener delete, got %d", len(pending))
	}
}

// TestResetInFlightEvents verifies that in-flight events (consumed_at set, tx_id null)
// are reset to pending by ResetInFlightEvents.
func TestResetInFlightEvents(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	// Create an event directly in the in-flight state.
	e := &Event{
		ID:         uuid.New().String(),
		ListenerID: "fake-listener",
		ArgsJSON:   "{}",
		CreatedAt:  time.Now().UTC(),
	}
	_ = st.CreateEvents(ctx, []*Event{e})
	_ = st.LockEvent(ctx, e.ID) // sets consumed_at, leaving tx_id nil

	// Verify it's in-flight.
	got, _ := st.ReadEvent(ctx, e.ID)
	if got.ConsumedAt == nil {
		t.Fatal("event should be in-flight before reset")
	}

	if err := k.ResetInFlightEvents(ctx); err != nil {
		t.Fatalf("ResetInFlightEvents: %v", err)
	}

	got, _ = st.ReadEvent(ctx, e.ID)
	if got.ConsumedAt != nil {
		t.Error("event should be pending (consumed_at=nil) after reset")
	}
}

// TestEmitEventCausalTraceID verifies the causing_trace_id is stored on the event.
func TestEmitEventCausalTraceID(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"fired":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, _, _ := k.StartProcess(ctx, alice.ID, alice.ID, 500)

	_, _ = k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: bob.ID, EventName: "ping",
		TargetActionID: a.ID,
	})

	causingTraceID := "some-emitting-trace-id"
	eventIDs, err := k.EmitEvent(ctx, bob.ID, bob.ID, "ping", nil, causingTraceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventIDs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(eventIDs))
	}

	// Event must carry the causing trace ID.
	e, err := st.ReadEvent(ctx, eventIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if e.CausingTraceID != causingTraceID {
		t.Errorf("event.causing_trace_id: got %q, want %q", e.CausingTraceID, causingTraceID)
	}

	// After ConsumeEvent, the resulting trace must carry the FOLLOWS_FROM link.
	reply, err := k.ConsumeEvent(ctx, alice.ID, eventIDs[0], p.ID, "")
	if err != nil {
		t.Fatalf("ConsumeEvent: %v", err)
	}
	tx, err := st.ReadTransaction(ctx, reply.TxID)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := st.ReadTrace(ctx, tx.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.CausedByTraceID == nil || *tr.CausedByTraceID != causingTraceID {
		got := "<nil>"
		if tr.CausedByTraceID != nil {
			got = *tr.CausedByTraceID
		}
		t.Errorf("trace.CausedByTraceID: got %q, want %q", got, causingTraceID)
	}
	if tr.CausedByTraceID != nil && *tr.CausedByTraceID == tr.ParentTraceID {
		t.Error("CausedByTraceID must not equal ParentTraceID")
	}
}

// TestEmitDirectCallHasNilCausalID verifies that a direct emit (no causing trace)
// results in a nil CausedByTraceID on the event and resulting trace.
func TestEmitDirectCallHasNilCausalID(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"fired":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, _, _ := k.StartProcess(ctx, alice.ID, alice.ID, 500)
	_, _ = k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: bob.ID, EventName: "ping",
		TargetActionID: a.ID,
	})

	eventIDs, err := k.EmitEvent(ctx, bob.ID, bob.ID, "ping", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(eventIDs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(eventIDs))
	}

	e, _ := st.ReadEvent(ctx, eventIDs[0])
	if e.CausingTraceID != "" {
		t.Errorf("direct emit event.causing_trace_id should be empty, got %q", e.CausingTraceID)
	}

	reply, err := k.ConsumeEvent(ctx, alice.ID, eventIDs[0], p.ID, "")
	if err != nil {
		t.Fatalf("ConsumeEvent: %v", err)
	}
	tx, _ := st.ReadTransaction(ctx, reply.TxID)
	tr, _ := st.ReadTrace(ctx, tx.TraceID)
	if tr.CausedByTraceID != nil {
		t.Errorf("direct emit trace.CausedByTraceID should be nil, got %q", *tr.CausedByTraceID)
	}
}

func TestPollListenerUnauthorized(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 100)
	source := setupUser(t, st, "@bob", 0)
	stranger := setupUser(t, st, "@carol", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	l, _ := k.CreateListener(ctx, alice.ID, CreateListenerRequest{
		SourceUserID: source.ID, EventName: "x",
		TargetActionID: a.ID,
	})

	if _, err := k.PollListener(ctx, stranger.ID, l.ID); err == nil {
		t.Error("expected unauthorized error for stranger polling")
	}
	if _, err := k.PollListener(ctx, source.ID, l.ID); err != nil {
		t.Errorf("source user should be able to poll: %v", err)
	}
}

func TestCreateListenerRequiresSourceUser(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 100)
	a := setupActiveWasmAction(t, st, owner.ID, "/handler")

	_, err := k.CreateListener(ctx, owner.ID, CreateListenerRequest{
		SourceUserID:   "",
		EventName:      "ping",
		TargetActionID: a.ID,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty SourceUserID, got %v", err)
	}
}
