# sqlguard-mcp

[![CI](https://github.com/ahmed-hashim-pro/sqlguard-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/ahmed-hashim-pro/sqlguard-mcp/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/ahmed-hashim-pro/sqlguard-mcp.svg)](https://pkg.go.dev/github.com/ahmed-hashim-pro/sqlguard-mcp) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

An MCP server that lets an LLM agent query a real database, and refuses anything
it cannot prove is a read.

People want an agent that can answer *"how many orders were refunded last
month"* against the actual data. They do not want it holding write access to
find out. `sqlguard` runs the reads immediately, refuses the writes, and gives
writes a path that ends at a human typing a command in their own terminal.

```
agent: SELECT count(*) FROM orders WHERE status = 'refunded'
       -> 1 row, 3ms

agent: UPDATE orders SET status = 'refunded'
       -> refused: this statement changes rows
          next: call request_write_approval, then ask the operator to approve

agent: SELECT 1; DROP TABLE orders
       -> refused: statement contains more than one statement
```

## Quickstart

Go 1.25+. No database server, no cgo, no API key.

```bash
git clone https://github.com/ahmed-hashim-pro/sqlguard-mcp.git
cd sqlguard-mcp

make build             # -> ./bin/sqlguard
./bin/sqlguard seed ./shop.db
make check             # vet, gofmt and the full suite with -race
```

`seed` writes a small sample shop — customers, orders, order items — from a
schema compiled into the binary, so it needs no `sqlite3` client and no network.

Then point an MCP client at it:

```json
{
  "mcpServers": {
    "sqlguard": {
      "command": "/absolute/path/to/sqlguard-mcp/bin/sqlguard",
      "args": ["serve", "--db", "/absolute/path/to/sqlguard-mcp/shop.db"]
    }
  }
}
```

## The security model

### Reads run. Everything else has to earn it.

| Statement | Decision |
| --- | --- |
| `SELECT`, read-only `WITH`, `VALUES`, `EXPLAIN` of a read | **runs**, under a row cap and a timeout |
| `INSERT`, `UPDATE`, `DELETE`, `REPLACE`, data-modifying CTEs | **refused** until a human approves that exact statement |
| `CREATE`, `DROP`, `ALTER`, `PRAGMA`, `ATTACH`, `VACUUM` | **refused**, always — schema changes are out of scope |
| More than one statement, or any comment | **refused** — either can hide a statement |
| Anything else | **refused** — it was not proved to be a read |

### Ambiguity is a refusal

The classifier never fully parses SQL. Engine-accurate parsers are
engine-specific and the good ones need cgo, which would cost the zero-setup
install. A conservative classifier that refuses what it cannot prove is a read
has a smaller failure surface than a parser that is wrong in ways nobody has
enumerated.

So a statement is a read only when it **starts** like one *and* contains no
write or DDL keyword. That ordering is the whole point:

```sql
WITH gone AS (DELETE FROM orders RETURNING *) SELECT * FROM gone
```

That begins with `WITH` and would pass any check keyed on the first word. It
deletes every row. Keywords are matched against bare words from a scan that
skips quoted sections, so `SELECT 'DELETE FROM orders'` is a read — the verb is
data — and `deleted_at` is one identifier rather than a verb.

### The engine enforces it too

Reads go through a connection opened `query_only(1)`. If a write ever reached
the read path, SQLite refuses it. The classifier and the engine both have to
fail for a write to land there, which turns a classifier bug into a contained
failure rather than an exploited one. There is a test that fires `DELETE`,
`UPDATE`, `INSERT` and `DROP` at the read connection and asserts the row count
is unchanged.

### Approval arrives on a channel the agent does not control

```mermaid
sequenceDiagram
    participant A as Agent
    participant S as sqlguard serve
    participant H as Operator's terminal
    participant D as Database

    A->>S: query "UPDATE orders SET ..."
    S-->>A: refused - writes need approval
    A->>S: request_write_approval
    S-->>A: token a1b2c3, run "sqlguard approve a1b2c3"
    A->>S: execute_approved_write
    S-->>A: refused - not approved yet
    Note over H: human reads the statement, decides
    H->>S: sqlguard approve a1b2c3
    A->>S: execute_approved_write
    S->>D: UPDATE ...
    S-->>A: executed, 1 row
    A->>S: execute_approved_write (again)
    S-->>A: refused - single use
```

Three properties make this a boundary rather than a formality:

- **There is no tool that approves.** The agent has five tools and none of them
  grants anything. Approval happens in a different process, started by a human.
  A protocol-level test asserts no such tool is advertised, so adding one later
  fails the build.
- **An approval is bound to its statement, not to a capability.** The token
  records a fingerprint of the statement; redemption recomputes it. A token
  granted for `UPDATE orders SET status='paid' WHERE id=1` cannot be spent on
  the same statement with the predicate removed.
- **An approval is single-use, across processes.** That rests on `O_EXCL`
  rather than a mutex — the server and the CLI are different programs, so a
  mutex orders nothing between them. An atomic file creation does.

`sqlguard approve` prints the statement before granting. A prompt that does not
show what is being agreed to is a rubber stamp.

### What this does not do

- It is **not a sandbox**. An approved write executes as written.
- Reads are capped at 500 rows and 30s by default, not by cost. A legal `SELECT`
  can still be expensive.
- There is no row- or column-level access control. Anything readable is
  readable.
- SQLite only. The policy layer is engine-agnostic; the driver is not.
- The classifier is conservative, which means it refuses some legitimate reads.
  That is the intended direction to be wrong in.

## Tools

| Tool | Does |
| --- | --- |
| `query` | Run a read. Refuses anything else, and says what would work instead. |
| `list_tables` | Names of the tables. |
| `describe_table` | Columns, types, nullability, primary key. |
| `request_write_approval` | Ask a human to approve one write. Returns a token. |
| `execute_approved_write` | Run a write a human approved. Single use, statement-bound. |

`describe_table` takes a table name, and `PRAGMA` cannot bind parameters.
Rather than escaping the name into SQL, it is matched against the tables that
actually exist and the real one is used, so an attacker-controlled string never
reaches a statement.

## Commands

```
sqlguard serve   --db <path> [--approvals dir] [--audit file]
                 [--max-rows 500] [--timeout 30s] [--approval-ttl 5m]
sqlguard approve <token> [--approvals dir]
sqlguard pending [--approvals dir]
sqlguard seed    <path> [--force]
```

Every decision, allowed or refused, is appended to `audit.jsonl`:

```json
{"at":"2026-09-04T12:00:03Z","tool":"query","statement":"DELETE FROM orders","kind":"write","decision":"refused","reason":"this statement changes rows, and writes are not executed without human approval"}
```

## Layout

```
cmd/sqlguard/        CLI: serve, approve, pending, seed
internal/policy/     statement classification    - no dependencies at all
internal/db/         execution under caps        - read connection is query_only
internal/approval/   tokens, TTL, single use     - O_EXCL, not a mutex
internal/audit/      JSONL decision log
internal/mcpsrv/     the five tools
```

`internal/` is enforced by the Go compiler, not by convention: nothing outside
this module can import these packages.

## Tests

127 tests, no database server and no credentials required.

```bash
make check      # go vet, gofmt -l, go test -race ./...
```

The suite is layered deliberately:

- `internal/policy` is table-driven over ~40 statements, including the ones a
  naive classifier gets wrong.
- `internal/mcpsrv` runs a real MCP client against the server over in-memory
  transports, so it exercises the generated schemas and JSON-RPC.
- `cmd/sqlguard` builds the binary, starts it as a subprocess, and drives the
  whole refuse → request → approve → execute → reuse cycle over stdio.

## Why this exists

I had already built an MCP server that handed an agent a local shell. Writing
it made the real problem obvious: an MCP server is a set of capabilities given
to a non-deterministic caller, and the usual answer — document the risk, trust
the operator — does not survive that, because the operator is not the one
choosing when the dangerous call happens.

That server's answer was binary: a tool is on or off, set by an environment
variable. This one is the graded version. Some statements run, some are refused
outright, and some are refused *pending a decision made somewhere the agent
cannot reach*. Getting that last category right is most of the design — which is
why the approval is bound to a statement rather than a capability, why it is
single-use through an atomic filesystem operation rather than a mutex, and why
the thing that grants it is a different process entirely.

It is also my first Go project, which is why the commit history builds it one
package at a time.

## License

MIT — see [LICENSE](LICENSE).
