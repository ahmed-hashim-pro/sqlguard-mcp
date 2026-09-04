// Package mcpsrv exposes the guarded database as MCP tools.
//
// Every tool answers with a decision the agent can act on rather than a bare
// error, because a refusal that does not say what would work next just gets
// retried verbatim.
package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/approval"
	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/audit"
	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/db"
	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/policy"
)

type Config struct {
	MaxRows      int
	QueryTimeout time.Duration
	ApprovalTTL  time.Duration
}

func DefaultConfig() Config {
	return Config{MaxRows: 500, QueryTimeout: 30 * time.Second, ApprovalTTL: 5 * time.Minute}
}

type Server struct {
	db        *db.DB
	approvals *approval.Store
	log       *audit.Logger
	cfg       Config
}

func New(database *db.DB, approvals *approval.Store, log *audit.Logger, cfg Config) *Server {
	return &Server{db: database, approvals: approvals, log: log, cfg: cfg}
}

// ---------------------------------------------------------------------------
// query
// ---------------------------------------------------------------------------

type QueryInput struct {
	SQL string `json:"sql" jsonschema:"a single SQL statement; reads run immediately, writes must be approved first"`
}

type QueryOutput struct {
	Decision  string   `json:"decision"`
	Kind      string   `json:"kind,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	NextStep  string   `json:"next_step,omitempty"`
	Columns   []string `json:"columns,omitempty"`
	Rows      [][]any  `json:"rows,omitempty"`
	RowCount  int      `json:"row_count,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Elapsed   string   `json:"elapsed,omitempty"`
}

func (s *Server) Query(ctx context.Context, _ *mcp.CallToolRequest, in QueryInput) (*mcp.CallToolResult, QueryOutput, error) {
	kind, err := policy.Classify(in.SQL)
	if err != nil {
		out := QueryOutput{
			Decision: "refused",
			Reason:   err.Error(),
			NextStep: "Send one complete statement with no comments.",
		}
		s.record(audit.Entry{Tool: "query", Statement: in.SQL, Decision: audit.Refused, Reason: err.Error()})
		return nil, out, nil
	}

	if kind != policy.KindRead {
		out := QueryOutput{Decision: "refused", Kind: kind.String()}
		switch kind {
		case policy.KindWrite:
			out.Reason = "this statement changes rows, and writes are not executed without human approval"
			out.NextStep = "Call request_write_approval with this exact statement, then ask the operator to approve it."
		case policy.KindDDL:
			out.Reason = "this statement changes the schema or the database itself, which this server never executes"
			out.NextStep = "Schema changes are out of scope. Ask the operator to make them directly."
		default:
			out.Reason = "this statement could not be proved to be a read"
			out.NextStep = "Rewrite it as a plain SELECT."
		}
		s.record(audit.Entry{Tool: "query", Statement: in.SQL, Kind: kind.String(),
			Decision: audit.Refused, Reason: out.Reason})
		return nil, out, nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout)
	defer cancel()

	result, err := s.db.Query(ctx, in.SQL, s.cfg.MaxRows)
	if err != nil {
		s.record(audit.Entry{Tool: "query", Statement: in.SQL, Kind: kind.String(),
			Decision: audit.Errored, Reason: err.Error()})
		return nil, QueryOutput{Decision: "error", Kind: kind.String(), Reason: err.Error()}, nil
	}

	s.record(audit.Entry{Tool: "query", Statement: in.SQL, Kind: kind.String(),
		Decision: audit.Allowed, Rows: result.RowCount, Elapsed: result.Elapsed})

	out := QueryOutput{
		Decision: "allowed", Kind: kind.String(),
		Columns: result.Columns, Rows: result.Rows, RowCount: result.RowCount,
		Truncated: result.Truncated, Elapsed: result.Elapsed,
	}
	if result.Truncated {
		out.NextStep = fmt.Sprintf("Only the first %d rows are shown. Narrow the query or aggregate it.", s.cfg.MaxRows)
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// request_write_approval
// ---------------------------------------------------------------------------

type ApprovalInput struct {
	SQL string `json:"sql" jsonschema:"the exact write statement you want a human to approve"`
}

type ApprovalOutput struct {
	Decision     string `json:"decision"`
	Token        string `json:"token,omitempty"`
	Statement    string `json:"statement,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	OperatorRuns string `json:"operator_must_run,omitempty"`
	Reason       string `json:"reason,omitempty"`
	NextStep     string `json:"next_step,omitempty"`
}

func (s *Server) RequestApproval(_ context.Context, _ *mcp.CallToolRequest, in ApprovalInput) (*mcp.CallToolResult, ApprovalOutput, error) {
	kind, err := policy.Classify(in.SQL)
	if err != nil {
		s.record(audit.Entry{Tool: "request_write_approval", Statement: in.SQL,
			Decision: audit.Refused, Reason: err.Error()})
		return nil, ApprovalOutput{Decision: "refused", Reason: err.Error()}, nil
	}
	// Reads need no approval, and DDL is never approvable — offering a token
	// for either would imply this server would run it.
	if kind != policy.KindWrite {
		reason := fmt.Sprintf("only writes can be approved; this statement is %s", kind)
		next := "Run reads with the query tool."
		if kind == policy.KindDDL {
			next = "Schema changes are out of scope for this server."
		}
		s.record(audit.Entry{Tool: "request_write_approval", Statement: in.SQL, Kind: kind.String(),
			Decision: audit.Refused, Reason: reason})
		return nil, ApprovalOutput{Decision: "refused", Reason: reason, NextStep: next}, nil
	}

	req, err := s.approvals.Create(in.SQL, kind.String(), s.cfg.ApprovalTTL)
	if err != nil {
		return nil, ApprovalOutput{Decision: "error", Reason: err.Error()}, nil
	}
	s.record(audit.Entry{Tool: "request_write_approval", Statement: in.SQL, Kind: kind.String(),
		Decision: audit.ApprovalAsked, Token: req.Token})

	return nil, ApprovalOutput{
		Decision:     "pending",
		Token:        req.Token,
		Statement:    req.Statement,
		ExpiresAt:    req.ExpiresAt.Format(time.RFC3339),
		OperatorRuns: "sqlguard approve " + req.Token,
		NextStep: "Ask the operator to run that command in their own terminal, then call " +
			"execute_approved_write with this token and the identical statement. You cannot approve this yourself.",
	}, nil
}

// ---------------------------------------------------------------------------
// execute_approved_write
// ---------------------------------------------------------------------------

type ExecuteInput struct {
	Token string `json:"token" jsonschema:"the token returned by request_write_approval"`
	SQL   string `json:"sql" jsonschema:"the statement the token was issued for, unchanged"`
}

type ExecuteOutput struct {
	Decision     string `json:"decision"`
	RowsAffected int64  `json:"rows_affected,omitempty"`
	Reason       string `json:"reason,omitempty"`
	NextStep     string `json:"next_step,omitempty"`
}

func (s *Server) ExecuteApproved(ctx context.Context, _ *mcp.CallToolRequest, in ExecuteInput) (*mcp.CallToolResult, ExecuteOutput, error) {
	// Re-classify rather than trusting that the statement is still the one that
	// was classified when the token was issued.
	kind, err := policy.Classify(in.SQL)
	if err != nil || kind != policy.KindWrite {
		reason := "statement is not a write"
		if err != nil {
			reason = err.Error()
		}
		s.record(audit.Entry{Tool: "execute_approved_write", Statement: in.SQL,
			Decision: audit.Refused, Reason: reason, Token: in.Token})
		return nil, ExecuteOutput{Decision: "refused", Reason: reason}, nil
	}

	req, err := s.approvals.Redeem(in.Token, in.SQL)
	if err != nil {
		out := ExecuteOutput{Decision: "refused", Reason: err.Error()}
		switch {
		case errors.Is(err, approval.ErrNotApproved):
			out.NextStep = "The operator has not approved this yet. Wait, and do not retry in a loop."
		case errors.Is(err, approval.ErrStatementDiff):
			out.NextStep = "Approval is bound to the exact statement. Request a new one for the statement you actually want."
		case errors.Is(err, approval.ErrExpired):
			out.NextStep = "Request a fresh approval."
		case errors.Is(err, approval.ErrAlreadyUsed):
			out.NextStep = "Each approval runs once. Request a new one."
		}
		s.record(audit.Entry{Tool: "execute_approved_write", Statement: in.SQL, Kind: kind.String(),
			Decision: audit.Refused, Reason: err.Error(), Token: in.Token})
		return nil, out, nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout)
	defer cancel()

	affected, err := s.db.Exec(ctx, in.SQL)
	if err != nil {
		s.record(audit.Entry{Tool: "execute_approved_write", Statement: in.SQL, Kind: kind.String(),
			Decision: audit.Errored, Reason: err.Error(), Token: req.Token})
		return nil, ExecuteOutput{Decision: "error", Reason: err.Error()}, nil
	}

	s.record(audit.Entry{Tool: "execute_approved_write", Statement: in.SQL, Kind: kind.String(),
		Decision: audit.ApprovalUsed, Token: req.Token, Rows: int(affected)})
	return nil, ExecuteOutput{Decision: "executed", RowsAffected: affected}, nil
}

func (s *Server) record(e audit.Entry) {
	// A failure to record is not a reason to refuse a call that policy allowed,
	// but it must not be silent either.
	if err := s.log.Log(e); err != nil {
		fmt.Fprintf(stderr, "sqlguard: audit write failed: %v\n", err)
	}
}
