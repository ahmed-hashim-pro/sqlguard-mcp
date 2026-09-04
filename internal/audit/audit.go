// Package audit records every decision the server makes, allowed or refused.
//
// A refusal is the more interesting record: it is the evidence that the
// boundary was exercised. The log is JSON Lines so it can be tailed live and
// parsed later without a schema migration.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Entry is one decision.
type Entry struct {
	At        time.Time `json:"at"`
	Tool      string    `json:"tool"`
	Statement string    `json:"statement,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
	Token     string    `json:"token,omitempty"`
	Rows      int       `json:"rows,omitempty"`
	Elapsed   string    `json:"elapsed,omitempty"`
}

// Decisions. Kept as constants so a typo cannot invent a category that never
// shows up in a search of the log.
const (
	Allowed       = "allowed"
	Refused       = "refused"
	ApprovalAsked = "approval_requested"
	ApprovalUsed  = "approval_redeemed"
	Errored       = "error"
)

// Logger serialises writes so concurrent tool calls cannot interleave
// half-written lines.
type Logger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

func New(w io.Writer) *Logger {
	return &Logger{w: w, now: time.Now}
}

// Log writes one entry. A failure to record is returned rather than swallowed:
// the caller decides whether an unrecordable decision should still proceed.
func (l *Logger) Log(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e.At.IsZero() {
		e.At = l.now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode audit entry: %w", err)
	}
	if _, err := fmt.Fprintf(l.w, "%s\n", line); err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}
	return nil
}
