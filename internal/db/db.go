// Package db executes statements under limits the caller cannot opt out of.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver; pure Go, no cgo
)

// DB holds two handles to the same file. Reads go through a connection the
// engine itself refuses to write on, so a bug in statement classification is
// contained rather than exploited — the policy layer and the engine have to
// fail together for a write to land on the read path.
type DB struct {
	read  *sql.DB
	write *sql.DB
}

// Result is one query's output, already materialised. Rows are held in memory
// because they are about to become a JSON tool result, and the row cap is what
// keeps that bounded.
type Result struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
	Elapsed   string   `json:"elapsed"`
}

// Open connects to a SQLite file. The path is a filename, not a DSN — query
// parameters are added here so a caller cannot pass ?_pragma=query_only(0).
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is empty")
	}

	read, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)")
	if err != nil {
		return nil, fmt.Errorf("open read connection: %w", err)
	}
	write, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		read.Close()
		return nil, fmt.Errorf("open write connection: %w", err)
	}

	// sql.Open is lazy — it does not touch the file. Ping is what turns a bad
	// path into an error here rather than on the first query.
	if err := read.Ping(); err != nil {
		read.Close()
		write.Close()
		return nil, fmt.Errorf("connect to %s: %w", path, err)
	}
	return &DB{read: read, write: write}, nil
}

func (d *DB) Close() error {
	err := d.read.Close()
	if writeErr := d.write.Close(); err == nil {
		err = writeErr
	}
	return err
}

// Query runs a read on the read-only connection and stops after maxRows.
//
// It asks the engine for maxRows+1 rows' worth of work so that Truncated
// reports whether more existed, without reading an unbounded result set.
func (d *DB) Query(ctx context.Context, statement string, maxRows int) (*Result, error) {
	if maxRows <= 0 {
		return nil, fmt.Errorf("maxRows must be positive, got %d", maxRows)
	}
	started := time.Now()

	rows, err := d.read.QueryContext(ctx, statement)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	// defer runs when the function returns, on every path including panics.
	// Without it, an early return below would leak the connection.
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}

	result := &Result{Columns: columns, Rows: [][]any{}}
	for rows.Next() {
		if len(result.Rows) == maxRows {
			result.Truncated = true
			break
		}
		row, err := scanRow(rows, len(columns))
		if err != nil {
			return nil, err
		}
		result.Rows = append(result.Rows, row)
	}
	// rows.Next() returning false is ambiguous: end of data, or a failure
	// mid-stream. rows.Err() is what tells them apart, and a context deadline
	// surfaces here rather than from QueryContext.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}

	result.RowCount = len(result.Rows)
	result.Elapsed = time.Since(started).Round(time.Millisecond).String()
	return result, nil
}

// Exec runs a statement on the writable connection. Nothing calls this without
// a redeemed approval; the policy layer decides, this only carries it out.
func (d *DB) Exec(ctx context.Context, statement string) (int64, error) {
	res, err := d.write.ExecContext(ctx, statement)
	if err != nil {
		return 0, fmt.Errorf("exec: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return affected, nil
}

// scanRow reads one row into plain Go values.
//
// database/sql scans into pointers, so this builds a slice of pointers to the
// slice it is filling. []byte becomes string because the destination is JSON,
// where a byte slice would base64-encode.
func scanRow(rows *sql.Rows, columnCount int) ([]any, error) {
	values := make([]any, columnCount)
	targets := make([]any, columnCount)
	for i := range values {
		targets[i] = &values[i]
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, fmt.Errorf("scan row: %w", err)
	}
	for i, v := range values {
		if b, ok := v.([]byte); ok {
			values[i] = string(b)
		}
	}
	return values, nil
}
