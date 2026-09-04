package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestDB builds a seeded database in a directory the test framework removes
// afterwards. t.Helper() keeps failure line numbers pointing at the caller.
func newTestDB(t *testing.T) *DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	ctx := context.Background()
	stmts := []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, status TEXT, total REAL, note BLOB)`,
		`INSERT INTO orders (id, status, total, note) VALUES
			(1, 'paid',    10.5, 'first'),
			(2, 'pending', 20.0, 'second'),
			(3, 'paid',    30.0, 'third'),
			(4, 'shipped', 40.0, 'fourth'),
			(5, 'paid',    50.0, 'fifth')`,
	}
	for _, s := range stmts {
		if _, err := database.Exec(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	return database
}

func TestQueryReturnsRows(t *testing.T) {
	database := newTestDB(t)

	got, err := database.Query(context.Background(), "SELECT id, status FROM orders ORDER BY id", 100)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if want := []string{"id", "status"}; len(got.Columns) != 2 || got.Columns[0] != want[0] {
		t.Errorf("Columns = %v, want %v", got.Columns, want)
	}
	if got.RowCount != 5 {
		t.Errorf("RowCount = %d, want 5", got.RowCount)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false: 5 rows fit under a cap of 100")
	}
	if got.Elapsed == "" {
		t.Error("Elapsed is empty")
	}
}

func TestRowCap(t *testing.T) {
	database := newTestDB(t)

	tests := []struct {
		name          string
		maxRows       int
		wantRows      int
		wantTruncated bool
	}{
		{"cap below the result", 2, 2, true},
		{"cap one short", 4, 4, true},
		{"cap exactly the result size", 5, 5, false},
		{"cap above the result", 50, 5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := database.Query(context.Background(), "SELECT id FROM orders", tt.maxRows)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got.RowCount != tt.wantRows {
				t.Errorf("RowCount = %d, want %d", got.RowCount, tt.wantRows)
			}
			if got.Truncated != tt.wantTruncated {
				t.Errorf("Truncated = %v, want %v", got.Truncated, tt.wantTruncated)
			}
		})
	}
}

func TestRowCapMustBePositive(t *testing.T) {
	database := newTestDB(t)
	for _, maxRows := range []int{0, -1} {
		if _, err := database.Query(context.Background(), "SELECT 1", maxRows); err == nil {
			t.Errorf("Query with maxRows=%d returned no error", maxRows)
		}
	}
}

// The point of the read connection: even a statement that reached it by mistake
// cannot change anything, because the engine refuses. This is what makes a bug
// in the policy layer a contained failure rather than an exploited one.
func TestReadConnectionCannotWrite(t *testing.T) {
	database := newTestDB(t)

	writes := []string{
		"DELETE FROM orders",
		"UPDATE orders SET status = 'x'",
		"INSERT INTO orders (id) VALUES (99)",
		"DROP TABLE orders",
	}
	for _, statement := range writes {
		t.Run(statement, func(t *testing.T) {
			_, err := database.Query(context.Background(), statement, 10)
			if err == nil {
				t.Fatalf("read connection executed %q", statement)
			}
			if !strings.Contains(err.Error(), "readonly") {
				t.Errorf("error = %v, want the engine's readonly refusal", err)
			}
		})
	}

	// and the data is untouched
	got, err := database.Query(context.Background(), "SELECT count(*) FROM orders", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if n := got.Rows[0][0]; n != int64(5) {
		t.Errorf("row count after refused writes = %v, want 5", n)
	}
}

func TestContextCancellationStopsAQuery(t *testing.T) {
	database := newTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the query starts

	if _, err := database.Query(ctx, "SELECT 1", 10); err == nil {
		t.Error("Query on a cancelled context returned no error")
	}
}

func TestContextDeadlineIsRespected(t *testing.T) {
	database := newTestDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // make sure it has expired

	if _, err := database.Query(ctx, "SELECT 1", 10); err == nil {
		t.Error("Query past its deadline returned no error")
	}
}

// BLOB columns arrive as []byte, which would base64-encode into the JSON tool
// result. They are converted to string on the way out.
func TestBlobsBecomeStrings(t *testing.T) {
	database := newTestDB(t)

	got, err := database.Query(context.Background(), "SELECT note FROM orders WHERE id = 1", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if _, isString := got.Rows[0][0].(string); !isString {
		t.Errorf("note = %T, want string", got.Rows[0][0])
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("Open(\"\") returned no error")
	}
}

func TestOpenRejectsAnUnreachablePath(t *testing.T) {
	if _, err := Open("/nonexistent-directory-xyz/test.db"); err == nil {
		t.Error("Open on an unreachable path returned no error")
	}
}
