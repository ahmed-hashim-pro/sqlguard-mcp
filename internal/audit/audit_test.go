package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogWritesOneJSONLinePerEntry(t *testing.T) {
	buf := &bytes.Buffer{}
	log := New(buf)

	for _, e := range []Entry{
		{Tool: "query", Decision: Allowed, Rows: 3},
		{Tool: "query", Decision: Refused, Reason: "write"},
	} {
		if err := log.Log(e); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for i, line := range lines {
		var got Entry
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i, err)
		}
	}
}

func TestTimestampIsFilledIn(t *testing.T) {
	buf := &bytes.Buffer{}
	log := New(buf)
	log.now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }

	log.Log(Entry{Tool: "query", Decision: Allowed})

	var got Entry
	json.Unmarshal(buf.Bytes(), &got)
	if got.At.Year() != 2026 {
		t.Errorf("At = %v, want the injected clock's time", got.At)
	}
}

func TestExplicitTimestampIsKept(t *testing.T) {
	buf := &bytes.Buffer{}
	when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	New(buf).Log(Entry{Tool: "query", Decision: Allowed, At: when})

	var got Entry
	json.Unmarshal(buf.Bytes(), &got)
	if !got.At.Equal(when) {
		t.Errorf("At = %v, want the supplied %v", got.At, when)
	}
}

// Concurrent tool calls must not interleave half-written lines. Run with -race.
func TestConcurrentLoggingProducesWholeLines(t *testing.T) {
	buf := &bytes.Buffer{}
	log := New(buf)

	const writers = 32
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			log.Log(Entry{Tool: "query", Decision: Allowed, Rows: i})
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != writers {
		t.Fatalf("got %d lines, want %d", len(lines), writers)
	}
	for i, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is torn: %q", i, line)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

var errWrite = &writeError{}

type writeError struct{}

func (*writeError) Error() string { return "disk full" }

func TestWriteFailureIsReported(t *testing.T) {
	// The caller decides what an unrecordable decision means; the logger's job
	// is not to hide it.
	if err := New(failingWriter{}).Log(Entry{Tool: "query", Decision: Allowed}); err == nil {
		t.Error("Log returned no error when the writer failed")
	}
}
