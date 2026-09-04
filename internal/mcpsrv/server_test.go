package mcpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/approval"
	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/audit"
	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/db"
)

type harness struct {
	*Server
	approvals *approval.Store
	auditBuf  *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	ctx := context.Background()
	for _, s := range []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, status TEXT NOT NULL, total REAL)`,
		`INSERT INTO orders (id, status, total) VALUES
			(1,'paid',10),(2,'pending',20),(3,'paid',30),(4,'shipped',40),(5,'paid',50)`,
	} {
		if _, err := database.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	store, err := approval.NewStore(filepath.Join(dir, "approvals"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	buf := &bytes.Buffer{}
	cfg := DefaultConfig()
	cfg.MaxRows = 3 // small, so truncation is easy to exercise

	return &harness{
		Server:    New(database, store, audit.New(buf), cfg),
		approvals: store,
		auditBuf:  buf,
	}
}

func (h *harness) auditDecisions(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(h.auditBuf.String()), "\n") {
		if line == "" {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line is not JSON: %q", line)
		}
		out = append(out, e.Tool+":"+e.Decision)
	}
	return out
}

func TestQueryAllowsReads(t *testing.T) {
	h := newHarness(t)

	_, out, err := h.Query(context.Background(), nil, QueryInput{SQL: "SELECT count(*) FROM orders"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Decision != "allowed" {
		t.Fatalf("Decision = %q (%s), want allowed", out.Decision, out.Reason)
	}
	if out.RowCount != 1 {
		t.Errorf("RowCount = %d, want 1", out.RowCount)
	}
}

func TestQueryRefusesEverythingThatIsNotARead(t *testing.T) {
	tests := []struct {
		name        string
		sql         string
		wantKind    string
		wantNextHas string
	}{
		{"update", "UPDATE orders SET status = 'paid'", "write", "request_write_approval"},
		{"delete", "DELETE FROM orders", "write", "request_write_approval"},
		{"write hidden in a CTE",
			"WITH gone AS (DELETE FROM orders RETURNING *) SELECT * FROM gone", "write", "request_write_approval"},
		{"drop", "DROP TABLE orders", "ddl", "out of scope"},
		{"pragma", "PRAGMA journal_mode = WAL", "ddl", "out of scope"},
		{"transaction control", "BEGIN", "unknown", "plain SELECT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			_, out, err := h.Query(context.Background(), nil, QueryInput{SQL: tt.sql})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if out.Decision != "refused" {
				t.Fatalf("Decision = %q, want refused", out.Decision)
			}
			if out.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", out.Kind, tt.wantKind)
			}
			// A refusal that does not say what would work gets retried verbatim.
			if !strings.Contains(out.NextStep, tt.wantNextHas) {
				t.Errorf("NextStep = %q, want it to mention %q", out.NextStep, tt.wantNextHas)
			}
		})
	}
}

func TestQueryRefusesUnclassifiableStatements(t *testing.T) {
	h := newHarness(t)
	for _, sql := range []string{"SELECT 1; DROP TABLE orders", "SELECT 1 -- hide", ""} {
		_, out, _ := h.Query(context.Background(), nil, QueryInput{SQL: sql})
		if out.Decision != "refused" {
			t.Errorf("Query(%q) decision = %q, want refused", sql, out.Decision)
		}
	}
}

func TestReadsAreCappedAndSaySo(t *testing.T) {
	h := newHarness(t) // MaxRows is 3, the table holds 5

	_, out, _ := h.Query(context.Background(), nil, QueryInput{SQL: "SELECT * FROM orders"})
	if !out.Truncated {
		t.Error("Truncated = false, want true")
	}
	if out.RowCount != 3 {
		t.Errorf("RowCount = %d, want 3", out.RowCount)
	}
	if !strings.Contains(out.NextStep, "Narrow") {
		t.Errorf("NextStep = %q, want advice about narrowing", out.NextStep)
	}
}

// The whole point, end to end.
func TestApprovalFlow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const write = "UPDATE orders SET status = 'refunded' WHERE id = 1"

	_, req, err := h.RequestApproval(ctx, nil, ApprovalInput{SQL: write})
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if req.Decision != "pending" || req.Token == "" {
		t.Fatalf("RequestApproval = %+v, want a pending token", req)
	}
	if !strings.Contains(req.OperatorRuns, "sqlguard approve "+req.Token) {
		t.Errorf("OperatorRuns = %q, want the approve command", req.OperatorRuns)
	}

	// Holding a token is not being approved.
	_, early, _ := h.ExecuteApproved(ctx, nil, ExecuteInput{Token: req.Token, SQL: write})
	if early.Decision != "refused" {
		t.Fatalf("execute before approval = %q, want refused", early.Decision)
	}

	// The operator approves, out of band.
	if _, err := h.approvals.Approve(req.Token); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	_, done, _ := h.ExecuteApproved(ctx, nil, ExecuteInput{Token: req.Token, SQL: write})
	if done.Decision != "executed" {
		t.Fatalf("execute after approval = %q (%s), want executed", done.Decision, done.Reason)
	}
	if done.RowsAffected != 1 {
		t.Errorf("RowsAffected = %d, want 1", done.RowsAffected)
	}

	// Single use.
	_, again, _ := h.ExecuteApproved(ctx, nil, ExecuteInput{Token: req.Token, SQL: write})
	if again.Decision != "refused" {
		t.Errorf("second execute = %q, want refused", again.Decision)
	}

	// The write actually landed.
	_, check, _ := h.Query(ctx, nil, QueryInput{SQL: "SELECT status FROM orders WHERE id = 1"})
	if got := check.Rows[0][0]; got != "refunded" {
		t.Errorf("status = %v, want refunded", got)
	}
}

func TestApprovalCannotBeWidened(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	narrow := "UPDATE orders SET status = 'refunded' WHERE id = 1"
	_, req, _ := h.RequestApproval(ctx, nil, ApprovalInput{SQL: narrow})
	h.approvals.Approve(req.Token)

	broad := "UPDATE orders SET status = 'refunded'"
	_, out, _ := h.ExecuteApproved(ctx, nil, ExecuteInput{Token: req.Token, SQL: broad})
	if out.Decision != "refused" {
		t.Fatalf("widened execute = %q, want refused", out.Decision)
	}
	if !strings.Contains(out.NextStep, "exact statement") {
		t.Errorf("NextStep = %q, want it to explain the binding", out.NextStep)
	}

	// nothing changed
	_, check, _ := h.Query(ctx, nil, QueryInput{SQL: "SELECT count(*) FROM orders WHERE status = 'refunded'"})
	if got := check.Rows[0][0]; got != int64(0) {
		t.Errorf("%v rows were refunded, want 0", got)
	}
}

func TestOnlyWritesCanBeApproved(t *testing.T) {
	h := newHarness(t)
	for _, sql := range []string{"SELECT 1", "DROP TABLE orders"} {
		_, out, _ := h.RequestApproval(context.Background(), nil, ApprovalInput{SQL: sql})
		if out.Decision != "refused" {
			t.Errorf("RequestApproval(%q) = %q, want refused", sql, out.Decision)
		}
		if out.Token != "" {
			t.Errorf("RequestApproval(%q) issued a token", sql)
		}
	}
}

func TestListTables(t *testing.T) {
	h := newHarness(t)
	_, out, err := h.ListTables(context.Background(), nil, TablesInput{})
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if len(out.Tables) != 1 || out.Tables[0] != "orders" {
		t.Errorf("Tables = %v, want [orders]", out.Tables)
	}
}

func TestDescribeTable(t *testing.T) {
	h := newHarness(t)
	_, out, err := h.DescribeTable(context.Background(), nil, DescribeInput{Table: "orders"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(out.Columns) != 3 {
		t.Fatalf("got %d columns, want 3", len(out.Columns))
	}
	if !out.Columns[0].PrimaryKey {
		t.Error("id is not reported as the primary key")
	}
	if !out.Columns[1].NotNull {
		t.Error("status is not reported as NOT NULL")
	}
}

// The table name reaches a PRAGMA, which cannot bind parameters. It is matched
// against real tables rather than escaped, so a crafted name is simply unknown.
func TestDescribeTableRejectsInjection(t *testing.T) {
	h := newHarness(t)
	attacks := []string{
		`orders"); DROP TABLE orders; --`,
		`orders" OR "1"="1`,
		"nonexistent",
	}
	for _, name := range attacks {
		t.Run(name, func(t *testing.T) {
			_, out, err := h.DescribeTable(context.Background(), nil, DescribeInput{Table: name})
			if err != nil {
				t.Fatalf("DescribeTable: %v", err)
			}
			if out.Decision != "refused" {
				t.Errorf("Decision = %q, want refused", out.Decision)
			}
		})
	}
	// the table survived
	_, out, _ := h.ListTables(context.Background(), nil, TablesInput{})
	if len(out.Tables) != 1 {
		t.Errorf("Tables = %v, want orders to still exist", out.Tables)
	}
}

func TestEveryDecisionIsAudited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.Query(ctx, nil, QueryInput{SQL: "SELECT 1"})
	h.Query(ctx, nil, QueryInput{SQL: "DELETE FROM orders"})
	h.Query(ctx, nil, QueryInput{SQL: "DROP TABLE orders"})

	got := h.auditDecisions(t)
	want := []string{"query:allowed", "query:refused", "query:refused"}
	if len(got) != len(want) {
		t.Fatalf("audit entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audit[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAuditRecordsTheRefusedStatement(t *testing.T) {
	h := newHarness(t)
	h.Query(context.Background(), nil, QueryInput{SQL: "DELETE FROM orders"})

	if !strings.Contains(h.auditBuf.String(), "DELETE FROM orders") {
		t.Error("the refused statement is not in the audit log")
	}
}

func TestApprovalTTLIsConfigured(t *testing.T) {
	h := newHarness(t)
	_, out, _ := h.RequestApproval(context.Background(), nil,
		ApprovalInput{SQL: "UPDATE orders SET status = 'x' WHERE id = 1"})

	expires, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("ExpiresAt %q is not RFC3339: %v", out.ExpiresAt, err)
	}
	if d := time.Until(expires); d > DefaultConfig().ApprovalTTL+time.Minute {
		t.Errorf("approval expires in %v, want about %v", d, DefaultConfig().ApprovalTTL)
	}
}
