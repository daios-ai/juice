package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice/log"
)

// owedStore is the slice of the store a reveal touches: the peer it came from, the obligation it
// names, the one write that closes it, and the payment that write was handed. The rest of the
// interface stays nil, so any accidental use of it fails loudly.
type owedStore struct {
	Store
	peer    *Account
	row     *Owed
	payment *RailTransfer
}

// ReconcileDeposits is what a recorded payment is followed by; it writes nothing here.
func (s *owedStore) ReconcileDeposits(context.Context, string, int) ([]*LedgerEntry, error) {
	return nil, nil
}

func (s *owedStore) ReadAccountByKernelKey(context.Context, string) (*Account, error) {
	return s.peer, nil
}

func (s *owedStore) ReadOwed(context.Context, string, string) (*Owed, error) {
	return s.row, nil
}

func (s *owedStore) ApplyReveal(_ context.Context, _, _ string, amount int64, txHash string, payment *RailTransfer) error {
	s.payment = payment
	s.row.Amount, s.row.TxHash, s.row.Status = amount, txHash, OwedAnnounced
	if amount == 0 {
		s.row.TxHash, s.row.Status = "", OwedCancelled
	}
	return nil
}

// stubRail is the slice of the rail a reveal touches: whether this world has addresses at all, and
// what a named payment turns out to be. Everything else fails loudly if a reveal reaches for it.
type stubRail struct {
	Rail
	addr string
}

func (r stubRail) Address() string { return r.addr }

func (stubRail) Witness(_ context.Context, ref string, amount int64) (RailDeposit, error) {
	return RailDeposit{Key: "rail:ref:" + ref, TxHash: ref, Amount: amount}, nil
}

// revealFixture builds a seller holding one obligation, and the buyer's key to sign with. The terms
// the draw runs under come from the call's frozen record, exactly as the store hands them over; an
// unsettled call is one whose commit has not happened yet.
func revealFixture(t *testing.T, obligation, lottery int64, secret, status string, settled bool) (*Kernel, *owedStore, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerKey := base64.RawURLEncoding.EncodeToString(pub)
	st := &owedStore{
		peer: &Account{ID: "peer-1", KernelPublicKey: peerKey},
		row: &Owed{
			ID: "call-1", PeerUserID: "peer-1", UserID: "seller-1", TraceID: "tr-1", Settled: settled,
			Terms:      *marshalServing(500, lottery, obligation, "0a0b", commitmentOf(secret)),
			Obligation: obligation, Status: status, CreatedAt: time.Now().UTC(),
		},
	}
	k := &Kernel{store: st, log: log.Discard(), cfg: Config{Network: playNet}}
	return k, st, priv, peerKey
}

// reveal signs and delivers one reveal, exactly as the buyer's kernel would.
func reveal(t *testing.T, k *Kernel, priv ed25519.PrivateKey, peerKey string, p RevealPayload) (*Owed, error) {
	t.Helper()
	sig, err := k.cfg.Network.sign(priv, sigDomainReveal, p)
	if err != nil {
		t.Fatal(err)
	}
	return k.HandleReveal(context.Background(), peerKey, p, sig)
}

func payload(peerKey, secret string) RevealPayload {
	return RevealPayload{Counterparty: peerKey, Recipient: "seller", Secret: secret, TicketID: "call-1",
		Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

// Everything about a reveal is recomputed rather than believed: who sent it, what it committed to,
// and whether the call it names has settled at all. A buyer cannot report a loss on a draw that won,
// because the secret it must present is the one whose hash the seller stored before the work.
func TestARevealIsRecomputedNotBelieved(t *testing.T) {
	for _, c := range []struct {
		name    string
		settled bool
		wrongPK bool
		secret  string
		want    error
	}{
		{"a secret that is not the committed one", true, false, "ccdd", ErrUnauthorized},
		{"a signature from anyone but the buyer", true, true, "aabb", nil},
		{"a call that has not committed yet", false, false, "aabb", ErrInvalidState},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _, priv, peerKey := revealFixture(t, 40, 0, "aabb", "", c.settled)
			if c.wrongPK {
				_, priv, _ = ed25519.GenerateKey(rand.Reader)
			}
			_, err := reveal(t, k, priv, peerKey, payload(peerKey, c.secret))
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// The two outcomes are one decision with two answers. A draw that owes nothing closes the obligation
// outright and names no payment — accepted however late, since a deadline past which a loss became a
// win would turn any outage into a payment neither party drew. A draw that owes must name the
// payment that carries it, and the amount recorded is the draw's, never the buyer's claim.
func TestARevealCancelsOrAnnouncesWhatTheDrawDecided(t *testing.T) {
	// At a face value of 1000 an obligation of 1 loses on almost every secret, so each branch is
	// reached deliberately rather than by whichever way today's hash happens to fall.
	losing := losingSecret(t, "call-1", "0a0b", 1, 1000)
	for _, c := range []struct {
		name          string
		obligation    int64
		lottery       int64
		secret        string
		txHash        string
		wantStatus    string
		wantAmount    int64
		wantOutcome   string
		wantRefusedAs error
	}{
		{"a losing draw owes nothing", 1, 1000, losing, "", OwedCancelled, 0, "cancel", nil},
		{"a paying draw records what it decided", 40, 0, "aabb", "0xpaid", OwedAnnounced, 40, "exact", nil},
		{"a paying draw must name its payment", 40, 0, "aabb", "", "", 0, "", ErrInvalidInput},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, st, priv, peerKey := revealFixture(t, c.obligation, c.lottery, c.secret, "", true)
			st.row.CreatedAt = time.Now().UTC().Add(-72 * time.Hour) // however late it arrives
			p := payload(peerKey, c.secret)
			p.TxHash = c.txHash
			got, err := reveal(t, k, priv, peerKey, p)
			if c.wantRefusedAs != nil {
				if !errors.Is(err, c.wantRefusedAs) {
					t.Fatalf("got %v, want %v", err, c.wantRefusedAs)
				}
				return
			}
			if err != nil {
				t.Fatalf("HandleReveal: %v", err)
			}
			if got.Status != c.wantStatus || got.Amount != c.wantAmount || got.Outcome() != c.wantOutcome {
				t.Fatalf("reveal = %s/%d/%s, want %s/%d/%s",
					got.Status, got.Amount, got.Outcome(), c.wantStatus, c.wantAmount, c.wantOutcome)
			}
			if got.TxHash != c.txHash {
				t.Errorf("payment named = %q, want %q", got.TxHash, c.txHash)
			}
			if st.row.Amount != c.wantAmount {
				t.Errorf("the row must carry what the draw decided: %d", st.row.Amount)
			}
		})
	}
}

// A repeat of a reveal already applied is answered from the row rather than re-decided: the buyer
// may be resending only because our reply was lost, and a second decision could contradict the
// first. So a garbage secret on a closed obligation is safe, and changes nothing.
func TestARepeatedRevealIsAnsweredFromTheRow(t *testing.T) {
	k, st, priv, peerKey := revealFixture(t, 40, 0, "aabb", OwedCancelled, true)
	got, err := reveal(t, k, priv, peerKey, payload(peerKey, "wrong-secret-entirely"))
	if err != nil {
		t.Fatalf("a resend of a closed obligation must be safe: %v", err)
	}
	if got.Status != OwedCancelled || st.row.Amount != 0 {
		t.Errorf("a repeat re-decided the draw: %s/%d", got.Status, st.row.Amount)
	}
}

// losingSecret finds a secret whose draw loses, so a test can exercise that branch deliberately.
func losingSecret(t *testing.T, id, nonce string, d, lottery int64) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		s, err := newSecret()
		if err != nil {
			t.Fatal(err)
		}
		if Draw(id, mustHex(s), mustHex(nonce), d, lottery) == 0 {
			return s
		}
	}
	t.Fatal("no losing secret in 200 tries, which is impossible at these odds")
	return ""
}

// The commitment binds a secret the buyer cannot change afterwards, and the outcome is read off the
// amount rather than stored, so the three cases must be distinguishable from the amount alone.
func TestACommitmentBindsItsSecretAndTheAmountNamesTheOutcome(t *testing.T) {
	s, err := newSecret()
	if err != nil {
		t.Fatal(err)
	}
	if c := commitmentOf(s); len(c) != 64 || c == commitmentOf("aabb") {
		t.Errorf("a commitment is a SHA-256 in hex, distinct per secret: %q", c)
	}
	if commitmentOf("") != "" || commitmentOf("not hex at all") != "" {
		t.Error("a call that commits to nothing must have no commitment")
	}
	for _, c := range []struct {
		amount, obligation int64
		want               string
	}{{0, 40, "cancel"}, {40, 40, "exact"}, {1000, 40, "pay"}} {
		if got := (&Owed{Amount: c.amount, Obligation: c.obligation}).Outcome(); got != c.want {
			t.Errorf("amount %d against %d = %q, want %q", c.amount, c.obligation, got, c.want)
		}
	}
}

// Where the world has no addresses the buyer's signed reveal is the finalized payment (D23), so the
// seller records it with the reveal that names it and nothing waits on an operator. Where there are
// addresses it records nothing: the money is observed on the chain, and a buyer's word is not a
// payment however well signed.
func TestARevealIsThePaymentOnlyWhereTheWorldHasNoAddresses(t *testing.T) {
	losing := losingSecret(t, "call-1", "0a0b", 1, 1000)
	for _, c := range []struct {
		name       string
		addr       string
		obligation int64
		lottery    int64
		secret     string
		txHash     string
		wantPaid   int64 // zero means no payment may be recorded at all
	}{
		{"no addresses, a draw that pays", "", 40, 0, "aabb", "manual:call-1", 40},
		{"no addresses, a draw that pays nothing", "", 1, 1000, losing, "", 0},
		{"addresses, where the chain is the witness", "0xvault", 40, 0, "aabb", "0xpaid", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, st, priv, peerKey := revealFixture(t, c.obligation, c.lottery, c.secret, "", true)
			k.rail = stubRail{addr: c.addr}
			p := payload(peerKey, c.secret)
			p.TxHash = c.txHash
			if _, err := reveal(t, k, priv, peerKey, p); err != nil {
				t.Fatalf("the reveal was refused: %v", err)
			}
			if c.wantPaid == 0 {
				if st.payment != nil {
					t.Fatalf("a payment was recorded on a buyer's word alone: %+v", st.payment)
				}
				return
			}
			if st.payment == nil {
				t.Fatal("no payment was recorded, so nothing could ever close the obligation")
			}
			if st.payment.Amount != c.wantPaid || st.payment.TxHash != c.txHash ||
				st.payment.Status != RailStatusHeld || st.payment.Party != "" {
				t.Errorf("recorded %+v, want %d held under %s from no sender", st.payment, c.wantPaid, c.txHash)
			}
		})
	}
}
