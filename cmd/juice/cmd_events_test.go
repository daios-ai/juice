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
	a, err := env.k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID:  ownerID,
		Name:         name,
		Kind:         kernel.KindHTTP,
		Source:       "http://example.com",
		Description:  "test action",
		InputSchema:  minSchema,
		OutputSchema: minSchema,
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

	l, err := env.k.CreateListener(ctx, kernel.CreateListenerRequest{
		OwnerUserID:    owner.ID,
		SourceUserID:   source.ID,
		EventName:      "ping",
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

	txIDs, err := env.k.EmitEvent(ctx, emitter.ID, emitter.ID, "no-listeners-event", nil, "")
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

	l, _ := env.k.CreateListener(ctx, kernel.CreateListenerRequest{
		OwnerUserID:    owner.ID,
		SourceUserID:   source.ID,
		EventName:      "gone",
		TargetActionID: a.ID,
	})
	_ = env.k.DeleteListener(ctx, owner.ID, l.ID)

	txIDs, _ := env.k.EmitEvent(ctx, source.ID, source.ID, "gone", nil, "")
	if len(txIDs) != 0 {
		t.Errorf("deleted listener should not fire, got %d tx", len(txIDs))
	}
}

func TestListListeners(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@list-lst-owner", Email: "llo@example.com", Password: "p",
	})
	other, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@list-lst-other", Email: "llot@example.com", Password: "p",
	})

	a := makeActiveAction(t, env, owner.ID, "/list-lst-action")

	env.k.CreateListener(ctx, kernel.CreateListenerRequest{
		OwnerUserID: owner.ID, SourceUserID: other.ID,
		EventName: "ev1", TargetActionID: a.ID,
	})
	env.k.CreateListener(ctx, kernel.CreateListenerRequest{
		OwnerUserID: owner.ID, SourceUserID: other.ID,
		EventName: "ev2", TargetActionID: a.ID,
	})
	env.k.CreateListener(ctx, kernel.CreateListenerRequest{
		OwnerUserID: other.ID, SourceUserID: owner.ID,
		EventName: "ev3", TargetActionID: a.ID,
	})

	listeners, err := env.k.ListListeners(ctx, owner.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listeners) != 2 {
		t.Errorf("ListListeners: got %d, want 2", len(listeners))
	}
	for _, l := range listeners {
		if l.OwnerUserID != owner.ID {
			t.Errorf("unexpected owner %s", l.OwnerUserID)
		}
	}
}
