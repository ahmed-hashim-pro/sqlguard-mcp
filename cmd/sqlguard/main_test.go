package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSeedCreatesAUsableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shop.db")

	if err := run([]string{"seed", path}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("seed produced no database: %v", err)
	}
}

func TestSeedRefusesToClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shop.db")
	run([]string{"seed", path})

	if err := run([]string{"seed", path}); err == nil {
		t.Error("seeding over an existing file returned no error")
	}
	if err := run([]string{"seed", path, "--force"}); err != nil {
		t.Errorf("seed --force: %v", err)
	}
}

func TestServeRequiresADatabase(t *testing.T) {
	if err := run([]string{"serve"}); err == nil {
		t.Error("serve without --db returned no error")
	}
	if err := run([]string{"serve", "--db", "/nonexistent/nope.db"}); err == nil {
		t.Error("serve with a missing database returned no error")
	}
}

func TestApproveRejectsAnUnknownToken(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"approve", "deadbeefcafe", "--approvals", dir}); err == nil {
		t.Error("approving an unknown token returned no error")
	}
}

// End to end through the real binary: build it, start it as a subprocess the
// way an MCP client would, and drive it over stdio. Nothing here shares memory
// with the server.
func TestEndToEndOverStdio(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "sqlguard")
	dbPath := filepath.Join(dir, "shop.db")
	approvalsDir := filepath.Join(dir, "approvals")

	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if out, err := exec.Command(binary, "seed", dbPath).CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command: exec.Command(binary, "serve",
			"--db", dbPath,
			"--approvals", approvalsDir,
			"--audit", filepath.Join(dir, "audit.jsonl")),
	}, nil)
	if err != nil {
		t.Fatalf("connect to the server process: %v", err)
	}
	defer session.Close()

	call := func(name string, args map[string]any, into any) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("call %s: %v", name, err)
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal %s result: %v", name, err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("decode %s result: %v", name, err)
		}
	}

	// A read runs.
	var read struct {
		Decision string  `json:"decision"`
		Rows     [][]any `json:"rows"`
	}
	call("query", map[string]any{"sql": "SELECT count(*) FROM orders"}, &read)
	if read.Decision != "allowed" {
		t.Fatalf("read decision = %q, want allowed", read.Decision)
	}
	if got := read.Rows[0][0]; got != float64(8) { // JSON numbers decode as float64
		t.Errorf("order count = %v, want 8", got)
	}

	// A write does not.
	const write = "UPDATE orders SET status = 'refunded' WHERE id = 3"
	var refused struct {
		Decision string `json:"decision"`
		NextStep string `json:"next_step"`
	}
	call("query", map[string]any{"sql": write}, &refused)
	if refused.Decision != "refused" {
		t.Fatalf("write decision = %q, want refused", refused.Decision)
	}

	// Ask for approval, and confirm the agent cannot grant it.
	var pending struct {
		Decision string `json:"decision"`
		Token    string `json:"token"`
	}
	call("request_write_approval", map[string]any{"sql": write}, &pending)
	if pending.Token == "" {
		t.Fatal("no approval token was issued")
	}

	var tooSoon struct {
		Decision string `json:"decision"`
	}
	call("execute_approved_write", map[string]any{"token": pending.Token, "sql": write}, &tooSoon)
	if tooSoon.Decision != "refused" {
		t.Fatalf("execute before approval = %q, want refused", tooSoon.Decision)
	}

	// The operator approves in a separate process, exactly as documented.
	out, err := exec.Command(binary, "approve", pending.Token, "--approvals", approvalsDir).CombinedOutput()
	if err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), write) {
		t.Errorf("approve did not show the operator the statement:\n%s", out)
	}

	var executed struct {
		Decision     string `json:"decision"`
		RowsAffected int    `json:"rows_affected"`
	}
	call("execute_approved_write", map[string]any{"token": pending.Token, "sql": write}, &executed)
	if executed.Decision != "executed" {
		t.Fatalf("execute after approval = %q, want executed", executed.Decision)
	}
	if executed.RowsAffected != 1 {
		t.Errorf("RowsAffected = %d, want 1", executed.RowsAffected)
	}

	// Single use, across processes.
	var reused struct {
		Decision string `json:"decision"`
	}
	call("execute_approved_write", map[string]any{"token": pending.Token, "sql": write}, &reused)
	if reused.Decision != "refused" {
		t.Errorf("reusing the token = %q, want refused", reused.Decision)
	}

	// And the change actually landed.
	var check struct {
		Rows [][]any `json:"rows"`
	}
	call("query", map[string]any{"sql": "SELECT status FROM orders WHERE id = 3"}, &check)
	if got := check.Rows[0][0]; got != "refunded" {
		t.Errorf("status = %v, want refunded", got)
	}

	// The audit log recorded both the refusal and the redemption.
	log, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	for _, want := range []string{"refused", "approval_requested", "approval_redeemed", "allowed"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("audit log has no %q entry", want)
		}
	}
}

// Go's flag package stops at the first non-flag argument, so a flag written
// after the positional was silently ignored — approve looked in ./approvals
// regardless of what --approvals said. Both orderings must work.
func TestFlagsParseOnEitherSideOfThePositional(t *testing.T) {
	seedIn := func(t *testing.T, args []string) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "shop.db")
		full := append([]string{"seed"}, args...)
		for i, a := range full {
			if a == "PATH" {
				full[i] = path
			}
		}
		if err := run(full); err != nil {
			t.Fatalf("run(%v): %v", full, err)
		}
		return path
	}

	t.Run("flag after the positional", func(t *testing.T) {
		path := seedIn(t, []string{"PATH"})
		if err := run([]string{"seed", path, "--force"}); err != nil {
			t.Errorf("seed <path> --force: %v", err)
		}
	})

	t.Run("flag before the positional", func(t *testing.T) {
		path := seedIn(t, []string{"PATH"})
		if err := run([]string{"seed", "--force", path}); err != nil {
			t.Errorf("seed --force <path>: %v", err)
		}
	})

	t.Run("approvals directory is honoured after the token", func(t *testing.T) {
		dir := t.TempDir()
		// An unknown token in a real directory must fail as "not found" rather
		// than because the directory flag was dropped.
		err := run([]string{"approve", "aabbccddeeff", "--approvals", dir})
		if err == nil {
			t.Fatal("approving an unknown token returned no error")
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Errorf("the --approvals directory was not used: %v", statErr)
		}
	})

	t.Run("too many arguments is an error", func(t *testing.T) {
		if err := run([]string{"seed", "a", "b"}); err == nil {
			t.Error("two positional arguments returned no error")
		}
	})
}

// splitSQL splits the embedded schema on ";", which is only safe while
// sample.sql keeps no semicolon inside a string literal. The file is editable,
// so the assumption is pinned here rather than asserted in a comment.
func TestSampleSQLHasNoSemicolonsInLiterals(t *testing.T) {
	inLiteral := false
	for i, r := range sampleSQL {
		switch {
		case r == '\'':
			inLiteral = !inLiteral
		case r == ';' && inLiteral:
			t.Fatalf("sample.sql has a semicolon inside a string literal at byte %d; "+
				"splitSQL would cut the statement in half", i)
		}
	}
	if inLiteral {
		t.Error("sample.sql has an unclosed string literal")
	}
}

func TestSplitSQLYieldsEveryStatement(t *testing.T) {
	statements := splitSQL(sampleSQL)

	// 3 CREATE TABLE + 3 INSERT
	if len(statements) != 6 {
		t.Errorf("splitSQL produced %d statements, want 6", len(statements))
	}
	for i, s := range statements {
		if strings.TrimSpace(s) == "" {
			t.Errorf("statement %d is blank", i)
		}
	}
}
