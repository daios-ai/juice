package main

import (
	"context"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

func makeActiveAction(t *testing.T, env *testEnv, ownerID, name string) *kernel.Action {
	t.Helper()
	ctx := context.Background()
	a, err := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: ownerID,
		Name:        name,
		Kind:        kernel.KindHTTP,
		Source:      "http://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestListenerCreateAndDelete(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.User{
		ID: uuid.New().String(), Handle: "@evt-owner", Email: "evt@example.com",
		Available: 500, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	source, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@evt-source", Email: "esrc@example.com", Password: "p",
	})

	a := makeActiveAction(t, env, owner.ID, "/evt-handler")
	p, root, _ := env.k.StartProcess(ctx, owner.ID, 100)

	l, err := env.k.CreateListener(ctx, kernel.CreateListenerRequest{
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
		t.Error("expected active listener with non-empty ID")
	}

	if err := env.k.DeleteListener(ctx, owner.ID, l.ID); err != nil {
		t.Fatal(err)
	}
}

func TestEmitNoListeners(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	emitter, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@emitter", Email: "em@example.com", Password: "p",
	})

	txIDs, err := env.k.EmitEvent(ctx, emitter.ID, "no-listeners-event", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(txIDs) != 0 {
		t.Errorf("expected 0 tx for event with no listeners, got %d", len(txIDs))
	}
}

func TestListenerDeletedNotFired(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.User{
		ID: uuid.New().String(), Handle: "@del-lst-owner", Email: "dlo@example.com",
		Available: 500, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	source, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@del-lst-src", Email: "dls@example.com", Password: "p",
	})

	a := makeActiveAction(t, env, owner.ID, "/del-handler")
	p, root, _ := env.k.StartProcess(ctx, owner.ID, 100)

	l, _ := env.k.CreateListener(ctx, kernel.CreateListenerRequest{
		OwnerUserID:    owner.ID,
		SourceUserID:   source.ID,
		EventName:      "gone",
		ProcessID:      p.ID,
		TraceID:        root.ID,
		TargetActionID: a.ID,
	})
	_ = env.k.DeleteListener(ctx, owner.ID, l.ID)

	txIDs, _ := env.k.EmitEvent(ctx, source.ID, "gone", nil, "")
	if len(txIDs) != 0 {
		t.Errorf("deleted listener should not fire, got %d tx", len(txIDs))
	}
}
