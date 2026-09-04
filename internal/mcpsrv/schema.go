package mcpsrv

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/audit"
)

// stderr is a variable so tests can capture what the server reports.
var stderr io.Writer = os.Stderr

const tableListSQL = `SELECT name FROM sqlite_master
WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
ORDER BY name`

type TablesInput struct{}

type TablesOutput struct {
	Tables []string `json:"tables"`
}

func (s *Server) ListTables(ctx context.Context, _ *mcp.CallToolRequest, _ TablesInput) (*mcp.CallToolResult, TablesOutput, error) {
	names, err := s.tableNames(ctx)
	if err != nil {
		s.record(audit.Entry{Tool: "list_tables", Decision: audit.Errored, Reason: err.Error()})
		return nil, TablesOutput{}, err
	}
	s.record(audit.Entry{Tool: "list_tables", Decision: audit.Allowed, Rows: len(names)})
	return nil, TablesOutput{Tables: names}, nil
}

type DescribeInput struct {
	Table string `json:"table" jsonschema:"the name of a table returned by list_tables"`
}

type Column struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	NotNull    bool   `json:"not_null"`
	PrimaryKey bool   `json:"primary_key"`
}

type DescribeOutput struct {
	Table    string   `json:"table"`
	Columns  []Column `json:"columns,omitempty"`
	Decision string   `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
}

// DescribeTable reports a table's columns.
//
// PRAGMA cannot take a bound parameter, so the name would have to be
// interpolated into SQL. Rather than escape it, the name is checked against the
// tables that actually exist and the real one is used — an attacker-controlled
// string never reaches the statement.
func (s *Server) DescribeTable(ctx context.Context, _ *mcp.CallToolRequest, in DescribeInput) (*mcp.CallToolResult, DescribeOutput, error) {
	names, err := s.tableNames(ctx)
	if err != nil {
		return nil, DescribeOutput{}, err
	}
	matched := ""
	for _, name := range names {
		if strings.EqualFold(name, in.Table) {
			matched = name
			break
		}
	}
	if matched == "" {
		reason := fmt.Sprintf("no table named %q; known tables are %s", in.Table, strings.Join(names, ", "))
		s.record(audit.Entry{Tool: "describe_table", Decision: audit.Refused, Reason: reason})
		return nil, DescribeOutput{Table: in.Table, Decision: "refused", Reason: reason}, nil
	}

	rows, err := s.db.Query(ctx, `PRAGMA table_info("`+matched+`")`, 1000)
	if err != nil {
		s.record(audit.Entry{Tool: "describe_table", Decision: audit.Errored, Reason: err.Error()})
		return nil, DescribeOutput{}, err
	}

	out := DescribeOutput{Table: matched, Decision: "allowed"}
	for _, row := range rows.Rows {
		// PRAGMA table_info yields: cid, name, type, notnull, dflt_value, pk
		if len(row) < 6 {
			continue
		}
		out.Columns = append(out.Columns, Column{
			Name:       asString(row[1]),
			Type:       asString(row[2]),
			NotNull:    asInt(row[3]) == 1,
			PrimaryKey: asInt(row[5]) > 0,
		})
	}
	s.record(audit.Entry{Tool: "describe_table", Decision: audit.Allowed, Rows: len(out.Columns)})
	return nil, out, nil
}

func (s *Server) tableNames(ctx context.Context) ([]string, error) {
	result, err := s.db.Query(ctx, tableListSQL, 1000)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	names := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) > 0 {
			names = append(names, asString(row[0]))
		}
	}
	return names, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func asInt(v any) int64 {
	if n, ok := v.(int64); ok {
		return n
	}
	return 0
}

// Register wires every tool onto an MCP server. The generic AddTool derives each
// tool's JSON schema from its input struct, so the schema cannot drift from the
// handler that receives it.
func (s *Server) Register(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "query",
		Description: "Run a read-only SQL query. Reads execute immediately under a row cap. " +
			"Statements that write, change the schema, or cannot be proved to be reads are refused.",
	}, s.Query)

	mcp.AddTool(m, &mcp.Tool{
		Name:        "list_tables",
		Description: "List the tables in the database.",
	}, s.ListTables)

	mcp.AddTool(m, &mcp.Tool{
		Name:        "describe_table",
		Description: "Show a table's columns, types, nullability and primary key.",
	}, s.DescribeTable)

	mcp.AddTool(m, &mcp.Tool{
		Name: "request_write_approval",
		Description: "Ask a human to approve one write statement. Returns a token and the command " +
			"the operator must run themselves. You cannot approve your own request.",
	}, s.RequestApproval)

	mcp.AddTool(m, &mcp.Tool{
		Name: "execute_approved_write",
		Description: "Run a write that a human has approved. The token is single-use and is bound " +
			"to the exact statement it was issued for.",
	}, s.ExecuteApproved)
}
