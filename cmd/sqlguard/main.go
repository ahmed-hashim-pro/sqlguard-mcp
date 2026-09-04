// Command sqlguard is an MCP server that gives an LLM agent read access to a
// SQL database and refuses anything it cannot prove is a read. Writes run only
// after a human approves them through this same binary, in their own terminal.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/approval"
	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/audit"
	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/db"
	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/mcpsrv"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

// The sample schema is compiled into the binary so `sqlguard seed` needs no
// sqlite3 client and no network — a fresh clone can produce a database to guard
// with the tools it already has.
//
//go:embed sample.sql
var sampleSQL string

const usage = `sqlguard - a governed SQL gateway for LLM agents

usage:
  sqlguard serve   --db <path> [flags]   run the MCP server over stdio
  sqlguard approve <token> [flags]       approve one pending write
  sqlguard pending [flags]               list writes awaiting a decision
  sqlguard seed    <path>                write the sample database
  sqlguard version

Reads run immediately under a row cap. Writes are refused until a human
approves them with 'sqlguard approve', which is why that command exists in a
separate process from the server the agent talks to.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "sqlguard: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "approve":
		return runApprove(args[1:])
	case "pending":
		return runPending(args[1:])
	case "seed":
		return runSeed(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		os.Exit(2)
		return nil
	}
}

// storeFlags are shared by every command that touches the approval directory.
// The server and the approve command must agree on it or approvals will appear
// to vanish, so the default is the same in both.
func storeFlags(fs *flag.FlagSet) *string {
	return fs.String("approvals", "./approvals", "directory holding pending approvals")
}

// parsePositional parses flags that appear on either side of a single
// positional argument.
//
// Go's flag package stops at the first non-flag argument, so
// "approve <token> --approvals dir" would otherwise leave --approvals unparsed
// and silently look in the wrong directory. Parsing what is left after the
// positional is what makes both orderings work.
func parsePositional(fs *flag.FlagSet, args []string, what string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", fmt.Errorf("missing %s", what)
	}
	positional := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return positional, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to the SQLite database (required)")
	approvalsDir := storeFlags(fs)
	auditPath := fs.String("audit", "./audit.jsonl", "file to append decisions to")
	maxRows := fs.Int("max-rows", 500, "maximum rows returned by one query")
	timeout := fs.Duration("timeout", 30*time.Second, "per-statement timeout")
	ttl := fs.Duration("approval-ttl", 5*time.Minute, "how long an approval stays valid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return fmt.Errorf("--db is required\n\nRun 'sqlguard seed ./shop.db' first if you " +
			"just want something to point it at")
	}
	if _, err := os.Stat(*dbPath); err != nil {
		return fmt.Errorf("cannot open database %s: %w\n\nRun 'sqlguard seed %s' to create a sample one",
			*dbPath, err, *dbPath)
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		return err
	}
	defer database.Close()

	approvals, err := approval.NewStore(*approvalsDir)
	if err != nil {
		return err
	}

	auditFile, err := os.OpenFile(*auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log %s: %w", *auditPath, err)
	}
	defer auditFile.Close()

	guard := mcpsrv.New(database, approvals, audit.New(auditFile), mcpsrv.Config{
		MaxRows: *maxRows, QueryTimeout: *timeout, ApprovalTTL: *ttl,
	})

	server := mcp.NewServer(&mcp.Implementation{Name: "sqlguard", Version: version}, nil)
	guard.Register(server)

	// stdout is the protocol channel, so every human-facing message goes to
	// stderr. Anything printed to stdout would corrupt the JSON-RPC stream.
	fmt.Fprintf(os.Stderr, "sqlguard %s serving %s (approvals in %s, audit to %s)\n",
		version, *dbPath, *approvalsDir, *auditPath)

	// Ctrl-C cancels the context, which unwinds Run and closes the deferred
	// handles above rather than killing the process mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func runApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	approvalsDir := storeFlags(fs)
	token, err := parsePositional(fs, args, "token\n\nusage: sqlguard approve <token> [--approvals dir]")
	if err != nil {
		return err
	}

	store, storeErr := approval.NewStore(*approvalsDir)
	if storeErr != nil {
		return storeErr
	}
	req, err := store.Get(token)
	if err != nil {
		return err
	}

	// Show the operator exactly what they are agreeing to. An approval prompt
	// that does not display the statement is a rubber stamp.
	fmt.Printf("\n  statement: %s\n  requested: %s\n  expires:   %s\n\n",
		req.Statement,
		req.CreatedAt.Local().Format(time.RFC1123),
		req.ExpiresAt.Local().Format(time.RFC1123))

	if _, err := store.Approve(token); err != nil {
		return err
	}
	fmt.Printf("approved %s\n", token)
	return nil
}

func runPending(args []string) error {
	fs := flag.NewFlagSet("pending", flag.ExitOnError)
	approvalsDir := storeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := approval.NewStore(*approvalsDir)
	if err != nil {
		return err
	}
	pending, err := store.Pending()
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Println("nothing pending")
		return nil
	}
	for _, req := range pending {
		fmt.Printf("%s  expires %s\n  %s\n\n",
			req.Token, req.ExpiresAt.Local().Format(time.Kitchen), req.Statement)
	}
	fmt.Printf("approve one with: sqlguard approve <token> --approvals %s\n", *approvalsDir)
	return nil
}

func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite the file if it already exists")
	path, err := parsePositional(fs, args, "path\n\nusage: sqlguard seed <path> [--force]")
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s already exists; pass --force to replace it", path)
	} else if err == nil {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace %s: %w", path, err)
		}
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	database, openErr := db.Open(path)
	if openErr != nil {
		return openErr
	}
	defer database.Close()

	ctx := context.Background()
	for _, statement := range splitSQL(sampleSQL) {
		if _, err := database.Exec(ctx, statement); err != nil {
			return fmt.Errorf("seed %s: %w", path, err)
		}
	}
	fmt.Printf("wrote %s (customers, orders, order_items)\n", path)
	return nil
}

// splitSQL breaks the embedded schema into statements by splitting on ";".
//
// That is only correct because sample.sql contains no semicolon inside a string
// literal, which TestSampleSQLHasNoSemicolonsInLiterals enforces — the file is
// editable, so the assumption needs a test rather than a comment. Statements
// arriving from an agent go through internal/policy, which assumes nothing.
func splitSQL(script string) []string {
	var statements []string
	for _, chunk := range strings.Split(script, ";") {
		if trimmed := strings.TrimSpace(chunk); trimmed != "" {
			statements = append(statements, trimmed)
		}
	}
	return statements
}
