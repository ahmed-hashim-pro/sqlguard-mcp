package approval

import (
	"errors"
	"sync"
	"testing"
	"time"
)

const stmt = "UPDATE orders SET status = 'paid' WHERE id = 1"

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// at fixes the clock so TTL behaviour is tested without sleeping.
func (s *Store) at(when time.Time) { s.now = func() time.Time { return when } }

func TestCreateThenApproveThenRedeem(t *testing.T) {
	s := newStore(t)

	req, err := s.Create(stmt, "write", time.Minute)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if req.Approved() || req.Redeemed() {
		t.Fatal("a new request is already approved or redeemed")
	}

	if _, err := s.Approve(req.Token); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	got, err := s.Redeem(req.Token, stmt)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if !got.Redeemed() {
		t.Error("redeemed request does not report Redeemed()")
	}
}

func TestRedeemRequiresApproval(t *testing.T) {
	s := newStore(t)
	req, _ := s.Create(stmt, "write", time.Minute)

	// The agent holds the token the moment it asks. Holding it must not be
	// enough — otherwise requesting and being granted are the same act.
	if _, err := s.Redeem(req.Token, stmt); !errors.Is(err, ErrNotApproved) {
		t.Errorf("Redeem before approval = %v, want ErrNotApproved", err)
	}
}

func TestApprovalIsSingleUse(t *testing.T) {
	s := newStore(t)
	req, _ := s.Create(stmt, "write", time.Minute)
	s.Approve(req.Token)

	if _, err := s.Redeem(req.Token, stmt); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if _, err := s.Redeem(req.Token, stmt); !errors.Is(err, ErrAlreadyUsed) {
		t.Errorf("second Redeem = %v, want ErrAlreadyUsed", err)
	}
}

// An approval is for a statement, not a capability. This is the failure that
// would matter most in practice: a token granted for a narrow update being
// spent on a broad one.
func TestApprovalIsBoundToItsStatement(t *testing.T) {
	s := newStore(t)
	req, _ := s.Create("UPDATE orders SET status = 'paid' WHERE id = 1", "write", time.Minute)
	s.Approve(req.Token)

	widened := "UPDATE orders SET status = 'paid'"
	if _, err := s.Redeem(req.Token, widened); !errors.Is(err, ErrStatementDiff) {
		t.Errorf("Redeem with a widened statement = %v, want ErrStatementDiff", err)
	}
	// and the approval survives the attempt, rather than being burned by it
	if _, err := s.Redeem(req.Token, "UPDATE orders SET status = 'paid' WHERE id = 1"); err != nil {
		t.Errorf("Redeem with the approved statement after a mismatch: %v", err)
	}
}

func TestFingerprintIgnoresOnlyWhitespace(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{"identical", stmt, stmt, true},
		{"reformatted", "UPDATE orders\n  SET status = 'paid'\n  WHERE id = 1",
			"UPDATE orders SET status = 'paid' WHERE id = 1", true},
		{"different value", "UPDATE orders SET status = 'paid' WHERE id = 1",
			"UPDATE orders SET status = 'paid' WHERE id = 2", false},
		{"missing predicate", "UPDATE orders SET status = 'paid' WHERE id = 1",
			"UPDATE orders SET status = 'paid'", false},
		{"different case", "update orders set status = 'paid' where id = 1",
			"UPDATE orders SET status = 'paid' WHERE id = 1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Fingerprint(tt.a) == Fingerprint(tt.b); got != tt.same {
				t.Errorf("Fingerprint equality = %v, want %v", got, tt.same)
			}
		})
	}
}

func TestExpiry(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	t.Run("redeem after expiry", func(t *testing.T) {
		s := newStore(t)
		s.at(base)
		req, _ := s.Create(stmt, "write", 5*time.Minute)
		s.Approve(req.Token)

		s.at(base.Add(6 * time.Minute))
		if _, err := s.Redeem(req.Token, stmt); !errors.Is(err, ErrExpired) {
			t.Errorf("Redeem after expiry = %v, want ErrExpired", err)
		}
	})

	t.Run("approve after expiry", func(t *testing.T) {
		s := newStore(t)
		s.at(base)
		req, _ := s.Create(stmt, "write", 5*time.Minute)

		s.at(base.Add(6 * time.Minute))
		if _, err := s.Approve(req.Token); !errors.Is(err, ErrExpired) {
			t.Errorf("Approve after expiry = %v, want ErrExpired", err)
		}
	})

	t.Run("redeem just inside the window", func(t *testing.T) {
		s := newStore(t)
		s.at(base)
		req, _ := s.Create(stmt, "write", 5*time.Minute)
		s.Approve(req.Token)

		s.at(base.Add(4*time.Minute + 59*time.Second))
		if _, err := s.Redeem(req.Token, stmt); err != nil {
			t.Errorf("Redeem inside the window: %v", err)
		}
	})
}

// The server and the approve command are separate processes, so the single-use
// guarantee cannot rest on a mutex — each process has its own.
//
// Every racer therefore gets its OWN Store over the SAME directory, which is
// what two programs look like. Racing goroutines through one Store would only
// prove the mutex works: its serialisation makes even a plain read-modify-write
// safe, so that version of this test passes with the atomic claim deleted.
func TestConcurrentRedeemAcrossProcessesHasExactlyOneWinner(t *testing.T) {
	dir := t.TempDir()

	creator, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	req, _ := creator.Create(stmt, "write", time.Minute)
	creator.Approve(req.Token)

	const racers = 16
	stores := make([]*Store, racers)
	for i := range stores {
		s, err := NewStore(dir) // an independent Store, with an independent mutex
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		stores[i] = s
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		start     = make(chan struct{})
	)
	wg.Add(racers)
	for _, s := range stores {
		go func() {
			defer wg.Done()
			<-start // line every goroutine up so they contend for real
			if _, err := s.Redeem(req.Token, stmt); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d independent stores redeemed the same approval, want exactly 1", succeeded)
	}
}

func TestApproveIsIdempotent(t *testing.T) {
	s := newStore(t)
	req, _ := s.Create(stmt, "write", time.Minute)

	first, err := s.Approve(req.Token)
	if err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	second, err := s.Approve(req.Token)
	if err != nil {
		t.Fatalf("second Approve: %v", err)
	}
	if !first.ApprovedAt.Equal(*second.ApprovedAt) {
		t.Error("approving twice moved the approval time")
	}
}

func TestApproveAfterRedeemIsRefused(t *testing.T) {
	s := newStore(t)
	req, _ := s.Create(stmt, "write", time.Minute)
	s.Approve(req.Token)
	s.Redeem(req.Token, stmt)

	if _, err := s.Approve(req.Token); !errors.Is(err, ErrAlreadyUsed) {
		t.Errorf("Approve after redeem = %v, want ErrAlreadyUsed", err)
	}
}

func TestUnknownToken(t *testing.T) {
	s := newStore(t)
	for _, token := range []string{"", "deadbeefcafe"} {
		if _, err := s.Get(token); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", token, err)
		}
	}
}

// A token becomes a filename, so it must not be able to escape the directory.
func TestTokenCannotTraversePaths(t *testing.T) {
	s := newStore(t)
	for _, token := range []string{"../../etc/passwd", "..", "a/b", "abc.json", "ABC"} {
		if _, err := s.Get(token); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", token, err)
		}
	}
}

func TestPendingListsOnlyUndecidedRequests(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	s.at(base)

	waiting, _ := s.Create("UPDATE a SET x = 1", "write", time.Hour)
	approved, _ := s.Create("UPDATE b SET x = 1", "write", time.Hour)
	s.Approve(approved.Token)
	expired, _ := s.Create("UPDATE c SET x = 1", "write", time.Minute)

	s.at(base.Add(2 * time.Minute)) // the third has now lapsed

	pending, err := s.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("Pending returned %d requests, want 1", len(pending))
	}
	if pending[0].Token != waiting.Token {
		t.Errorf("Pending returned %q, want the undecided %q", pending[0].Token, waiting.Token)
	}
	_ = expired
}

func TestTokensAreDistinct(t *testing.T) {
	s := newStore(t)
	seen := map[string]bool{}
	for range 200 {
		req, err := s.Create(stmt, "write", time.Minute)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if seen[req.Token] {
			t.Fatalf("token %q issued twice", req.Token)
		}
		seen[req.Token] = true
	}
}
