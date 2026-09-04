package policy

import (
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	// A table-driven test: one slice of cases, one loop, one subtest each.
	// `go test -run 'TestClassify/a_write_hidden_in_a_CTE'` runs exactly one.
	tests := []struct {
		name string
		sql  string
		want Kind
	}{
		// Reads.
		{"trivial select", "SELECT 1", KindRead},
		{"aggregate", "SELECT count(*) FROM orders", KindRead},
		{"lowercase", "select * from orders where status = 'paid'", KindRead},
		{"trailing semicolon is fine", "SELECT 1;", KindRead},
		{"trailing semicolon and whitespace", "SELECT 1;  \n", KindRead},
		{"read-only CTE", "WITH recent AS (SELECT * FROM orders) SELECT * FROM recent", KindRead},
		{"values", "VALUES (1, 2)", KindRead},
		{"explain of a read", "EXPLAIN SELECT * FROM orders", KindRead},

		// Reads that a naive keyword scan would misread. The verb-looking text
		// is data or part of a longer identifier, not a statement.
		{"semicolon inside a string", "SELECT ';'", KindRead},
		{"write keyword inside a string", "SELECT 'DELETE FROM orders' AS note", KindRead},
		{"write keyword as a column prefix", "SELECT deleted_at FROM users", KindRead},
		{"write keyword as a table name", "SELECT * FROM updates", KindRead},
		{"write keyword as a quoted identifier", `SELECT "delete" FROM t`, KindRead},
		{"escaped quote inside a string", "SELECT 'it''s fine' AS note", KindRead},

		// Writes.
		{"insert", "INSERT INTO orders (id) VALUES (1)", KindWrite},
		{"update", "UPDATE orders SET status = 'paid'", KindWrite},
		{"delete", "DELETE FROM orders", KindWrite},
		{"replace", "REPLACE INTO orders (id) VALUES (1)", KindWrite},

		// The case the whole classifier exists for: it starts with WITH, so
		// anything keying on the first word calls it a read. It is a write.
		{"a write hidden in a CTE",
			"WITH gone AS (DELETE FROM orders RETURNING *) SELECT * FROM gone", KindWrite},
		{"explain of a write is still a write", "EXPLAIN DELETE FROM orders", KindWrite},

		// DDL.
		{"create", "CREATE TABLE t (a INTEGER)", KindDDL},
		{"drop", "DROP TABLE orders", KindDDL},
		{"alter", "ALTER TABLE orders ADD COLUMN note TEXT", KindDDL},
		{"vacuum", "VACUUM", KindDDL},
		{"attach", "ATTACH DATABASE 'other.db' AS other", KindDDL},

		// PRAGMA reads and writes differ by an argument, so the family is not
		// classified as a read even when this particular one only reads.
		{"pragma that writes", "PRAGMA journal_mode = WAL", KindDDL},
		{"pragma that reads", "PRAGMA table_info(orders)", KindDDL},

		// Not provably anything: the default arm, which callers refuse.
		{"transaction control", "BEGIN", KindUnknown},
		{"commit", "COMMIT", KindUnknown},
		{"session setting", "SET timezone = 'UTC'", KindUnknown},
		{"nonsense", "wibble wobble", KindUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(tt.sql)
			if err != nil {
				t.Fatalf("Classify(%q) returned error %v, want kind %v", tt.sql, err, tt.want)
			}
			if got != tt.want {
				t.Errorf("Classify(%q) = %v, want %v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestClassifyRefuses(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want error
	}{
		{"empty", "", ErrEmpty},
		{"only whitespace", "   \n\t ", ErrEmpty},
		{"only a semicolon", ";", ErrEmpty},

		{"two statements", "SELECT 1; DROP TABLE orders", ErrMultipleStatements},
		{"two statements, no space", "SELECT 1;DROP TABLE orders", ErrMultipleStatements},

		// A comment can hide a second statement from whoever reads the log, so
		// the presence of one is refused rather than stripped.
		{"line comment", "SELECT 1 -- note", ErrComment},
		{"line comment hiding a statement", "SELECT 1 -- \n; DROP TABLE orders", ErrComment},
		{"block comment", "SELECT /* note */ 1", ErrComment},
		{"block comment hiding a statement", "SELECT 1 /* ; DROP TABLE orders */", ErrComment},

		{"unterminated string", "SELECT 'oops", ErrUnterminatedQuote},
		{"unterminated identifier", `SELECT "oops`, ErrUnterminatedQuote},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Classify(tt.sql)
			// errors.Is unwraps, so a sentinel wrapped with %w still matches
			// while carrying extra context in its message.
			if !errors.Is(err, tt.want) {
				t.Errorf("Classify(%q) error = %v, want %v", tt.sql, err, tt.want)
			}
		})
	}
}

func TestRefusalIsAlwaysUnknown(t *testing.T) {
	// A refused statement must not also come back with a usable Kind — a caller
	// that ignored the error would otherwise get a classification to act on.
	for _, sql := range []string{"", "SELECT 1; DROP TABLE t", "SELECT 1 -- x", "SELECT 'x"} {
		if kind, err := Classify(sql); err != nil && kind != KindUnknown {
			t.Errorf("Classify(%q) = %v with error %v, want KindUnknown", sql, kind, err)
		}
	}
}

func TestKindString(t *testing.T) {
	for kind, want := range map[Kind]string{
		KindUnknown: "unknown", KindRead: "read", KindWrite: "write", KindDDL: "ddl",
	} {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}

func TestZeroKindIsUnknown(t *testing.T) {
	// Fails closed: a Kind nobody assigned is the one callers refuse.
	var k Kind
	if k != KindUnknown {
		t.Errorf("zero Kind = %v, want KindUnknown", k)
	}
}
