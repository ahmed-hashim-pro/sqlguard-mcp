// Package approval records write requests and the human decisions on them.
//
// The store is a directory of JSON files rather than a table in the database
// being guarded, and approval is granted by a separate process (the `sqlguard
// approve` command a human runs in their own terminal). Both choices exist for
// the same reason: the approval channel must not be one the agent can reach.
// An agent that could approve its own request has a logging system, not a
// boundary.
package approval

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound      = errors.New("no such approval request")
	ErrNotApproved   = errors.New("request has not been approved")
	ErrExpired       = errors.New("approval has expired")
	ErrAlreadyUsed   = errors.New("approval has already been used")
	ErrStatementDiff = errors.New("statement does not match the one that was approved")
)

// Request is one pending or decided write.
//
// The pointer fields are nil until the event happens, which is how a moment
// that has not occurred is distinguished from the zero time.
type Request struct {
	Token       string     `json:"token"`
	Statement   string     `json:"statement"`
	Fingerprint string     `json:"fingerprint"`
	Kind        string     `json:"kind"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	RedeemedAt  *time.Time `json:"redeemed_at,omitempty"`
}

func (r *Request) Approved() bool { return r.ApprovedAt != nil }
func (r *Request) Redeemed() bool { return r.RedeemedAt != nil }

// Store is a directory of request files.
//
// The mutex orders operations within this process. It deliberately does not
// pretend to order them across processes: the server and the approve command
// are separate programs, so redemption relies on an atomic filesystem
// operation instead. See Redeem.
type Store struct {
	dir string
	mu  sync.Mutex
	now func() time.Time // injectable so tests can move time without sleeping
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create approval store %s: %w", dir, err)
	}
	return &Store{dir: dir, now: time.Now}, nil
}

// Create records a pending request and returns it with a fresh token.
func (s *Store) Create(statement, kind string, ttl time.Duration) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := newToken()
	if err != nil {
		return nil, err
	}
	now := s.now()
	req := &Request{
		Token:       token,
		Statement:   statement,
		Fingerprint: Fingerprint(statement),
		Kind:        kind,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	}
	if err := s.write(req); err != nil {
		return nil, err
	}
	return req, nil
}

// Approve marks a request as approved. This is what the CLI calls, in a
// process the agent does not control.
func (s *Store) Approve(token string) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, err := s.read(token)
	if err != nil {
		return nil, err
	}
	if req.Redeemed() {
		return nil, fmt.Errorf("%w: redeemed at %s", ErrAlreadyUsed, req.RedeemedAt.Format(time.RFC3339))
	}
	if s.now().After(req.ExpiresAt) {
		return nil, fmt.Errorf("%w: expired at %s", ErrExpired, req.ExpiresAt.Format(time.RFC3339))
	}
	if req.Approved() {
		return req, nil // approving twice is not an error, it just does nothing
	}
	now := s.now()
	req.ApprovedAt = &now
	if err := s.write(req); err != nil {
		return nil, err
	}
	return req, nil
}

// Redeem consumes an approval for exactly the statement it was granted for.
//
// The single-use guarantee comes from creating a marker file with O_EXCL, which
// the kernel makes atomic: if two processes race, exactly one creation
// succeeds. A mutex could not provide this, because the racing parties are
// different programs.
func (s *Store) Redeem(token, statement string) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, err := s.read(token)
	if err != nil {
		return nil, err
	}
	if !req.Approved() {
		return nil, ErrNotApproved
	}
	if s.now().After(req.ExpiresAt) {
		return nil, fmt.Errorf("%w: expired at %s", ErrExpired, req.ExpiresAt.Format(time.RFC3339))
	}
	// An approval is for a statement, not for a capability. Without this, a
	// token granted for "UPDATE orders SET status='paid' WHERE id=1" would
	// redeem against "UPDATE orders SET status='paid'".
	if Fingerprint(statement) != req.Fingerprint {
		return nil, fmt.Errorf("%w: approved %q", ErrStatementDiff, req.Statement)
	}

	marker, err := os.OpenFile(s.markerPath(token), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrAlreadyUsed
		}
		return nil, fmt.Errorf("claim approval %s: %w", token, err)
	}
	marker.Close()

	now := s.now()
	req.RedeemedAt = &now
	if err := s.write(req); err != nil {
		return nil, err
	}
	return req, nil
}

// Get returns a request without changing it.
func (s *Store) Get(token string) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(token)
}

// Pending lists requests still awaiting a decision, so a human running the CLI
// can see what the agent has asked for.
func (s *Store) Pending() ([]*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list approval store: %w", err)
	}
	now := s.now()
	var pending []*Request
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		req, err := s.read(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue // a malformed file must not hide the rest
		}
		if !req.Approved() && !req.Redeemed() && now.Before(req.ExpiresAt) {
			pending = append(pending, req)
		}
	}
	return pending, nil
}

// Fingerprint identifies a statement for approval matching. Runs of whitespace
// collapse so reformatting does not invalidate an approval, but nothing else is
// normalised — a changed value is a different statement.
func Fingerprint(statement string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(statement), " ")))
	return hex.EncodeToString(sum[:])
}

func (s *Store) path(token string) string   { return filepath.Join(s.dir, token+".json") }
func (s *Store) markerPath(t string) string { return filepath.Join(s.dir, t+".used") }

func (s *Store) read(token string) (*Request, error) {
	if err := validToken(token); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path(token))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, token)
		}
		return nil, fmt.Errorf("read approval %s: %w", token, err)
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("parse approval %s: %w", token, err)
	}
	return &req, nil
}

// write replaces the file atomically: a reader either sees the old contents or
// the new ones, never a half-written file.
func (s *Store) write(req *Request) error {
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return fmt.Errorf("encode approval: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, req.Token+".*.tmp")
	if err != nil {
		return fmt.Errorf("stage approval: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename below has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write approval: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close approval: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path(req.Token)); err != nil {
		return fmt.Errorf("commit approval: %w", err)
	}
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// validToken keeps a token from being used as a path. Without it, a token of
// "../../etc/passwd" would read outside the store.
func validToken(token string) error {
	if token == "" {
		return fmt.Errorf("%w: empty token", ErrNotFound)
	}
	for _, c := range token {
		if (c < 'a' || c > 'f') && (c < '0' || c > '9') {
			return fmt.Errorf("%w: %q is not a token", ErrNotFound, token)
		}
	}
	return nil
}
