package kernel

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRecursiveFeedbackSingleCall(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/svc",
		Kind: KindWasm, Active: true, Price: 50,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	reply, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	fb, err := k.RecursiveFeedback(ctx, p.ID, reply.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if fb.RecursiveCost != 50 {
		t.Errorf("recursive cost: got %d, want 50", fb.RecursiveCost)
	}
	if fb.RecursiveLatency < 0 {
		t.Errorf("recursive latency should be non-negative: %f", fb.RecursiveLatency)
	}
}

func TestRecursiveFeedbackNoCalls(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 100)
	p, root, _ := k.StartProcess(ctx, alice.ID, 100)

	fb, err := k.RecursiveFeedback(ctx, p.ID, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fb.RecursiveCost != 0 {
		t.Errorf("expected 0 cost for empty trace, got %d", fb.RecursiveCost)
	}
}

func TestCollectSubtree(t *testing.T) {
	children := map[string][]string{"root": {"a", "b"}, "a": {"c"}}
	sub := collectSubtree("root", children)
	for _, id := range []string{"root", "a", "b", "c"} {
		if !sub[id] {
			t.Errorf("expected %q in subtree", id)
		}
	}
	if sub["nonexistent"] {
		t.Error("unexpected node in subtree")
	}
}

func TestPropagateRatings(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	for _, name := range []string{"/parent", "/child"} {
		_ = st.CreateAction(ctx, &Action{
			ID: uuid.New().String(), OwnerUserID: alice.ID, Name: name,
			Kind: KindWasm, Active: true, Price: 0,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
	}
	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	replyParent, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/parent", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	replyChild, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: replyParent.TraceID,
		TargetUserID: alice.ID, ActionName: "/child", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Rate only the parent transaction.
	rating := 1.0
	parentTx, _ := st.ReadTransaction(ctx, replyParent.TxID)
	parentTx.Rating = &rating
	_ = st.UpdateTransaction(ctx, parentTx)

	if err := k.PropagateRatings(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	// Child should have inherited the parent's rating.
	childTx, _ := st.ReadTransaction(ctx, replyChild.TxID)
	if childTx.Rating == nil {
		t.Error("expected child transaction to inherit parent rating")
	} else if *childTx.Rating != 1.0 {
		t.Errorf("propagated rating: got %f, want 1.0", *childTx.Rating)
	}
}

func TestPropagateRatingsUnratedParentNotPropagated(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	_ = st.CreateAction(ctx, &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/svc",
		Kind: KindWasm, Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	reply, _ := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/svc", Args: map[string]any{},
	})

	if err := k.PropagateRatings(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	tx, _ := st.ReadTransaction(ctx, reply.TxID)
	if tx.Rating != nil {
		t.Error("unrated transaction should not get a propagated rating if no rated ancestor exists")
	}
}
