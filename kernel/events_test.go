package kernel

import (
	"context"
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
	p, root, _ := k.StartProcess(ctx, owner.ID, 100)

	l, err := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID:    owner.ID,
		SourceUserID:   source.ID,
		EventName:      "ping",
		ProcessID:      p.ID,
		TraceID:        root.ID,
		TargetActionID: a.ID,
	})
	if err != nil {
		t.Fatalf("CreateListener: %v", err)
	}
	if l.ID == "" || !l.Active {
		t.Error("expected active listener with ID")
	}
}

func TestCreateListenerClosedProcessFails(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 100)
	source := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, owner.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, owner.ID, 100)
	_ = k.EndProcess(ctx, owner.ID, p.ID)

	_, err := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID:    owner.ID,
		SourceUserID:   source.ID,
		EventName:      "ping",
		ProcessID:      p.ID,
		TraceID:        root.ID,
		TargetActionID: a.ID,
	})
	if err == nil {
		t.Error("expected error creating listener on closed process")
	}
}

func TestDeleteListener(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 100)
	source := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, owner.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, owner.ID, 100)

	l, _ := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID: owner.ID, SourceUserID: source.ID, EventName: "ping",
		ProcessID: p.ID, TraceID: root.ID, TargetActionID: a.ID,
	})

	if err := k.DeleteListener(ctx, owner.ID, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteListener(ctx, source.ID, l.ID); err == nil {
		t.Error("expected authorization error for non-owner")
	}
}

func TestEmitEvent(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"fired":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	l, err := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID: alice.ID, SourceUserID: bob.ID, EventName: "greet",
		ProcessID: p.ID, TraceID: root.ID, TargetActionID: a.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	txIDs, err := k.EmitEvent(ctx, bob.ID, "greet", map[string]any{"msg": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(txIDs) != 1 {
		t.Errorf("expected 1 tx from emit, got %d", len(txIDs))
	}

	queued, err := k.PollListener(ctx, alice.ID, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0] != txIDs[0] {
		t.Errorf("poll: got %v, want [%s]", queued, txIDs[0])
	}
}

func TestEmitInactiveListenerNotFired(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	l, _ := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID: alice.ID, SourceUserID: bob.ID, EventName: "greet",
		ProcessID: p.ID, TraceID: root.ID, TargetActionID: a.ID,
	})
	_ = k.DeleteListener(ctx, alice.ID, l.ID)

	txIDs, _ := k.EmitEvent(ctx, bob.ID, "greet", nil)
	if len(txIDs) != 0 {
		t.Errorf("expected 0 fired for inactive listener, got %d", len(txIDs))
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
	p, root, _ := k.StartProcess(ctx, alice.ID, 100)
	l, _ := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID: alice.ID, SourceUserID: source.ID, EventName: "x",
		ProcessID: p.ID, TraceID: root.ID, TargetActionID: a.ID,
	})

	if _, err := k.PollListener(ctx, stranger.ID, l.ID); err == nil {
		t.Error("expected unauthorized error for stranger polling")
	}
	if _, err := k.PollListener(ctx, source.ID, l.ID); err != nil {
		t.Errorf("source user should be able to poll: %v", err)
	}
}
