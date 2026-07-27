 # Development Guide

## Build/Test/Lint Commands

- **Go version**: 1.26+
- **Build**: `mise build` (server → `trackkr-backend`), `mise build-daemon`
  (client → `trackkrd`)
- **Run**: `mise run` (server), `mise run-daemon` (daemon), `mise db` (Postgres
  via docker compose)
- **Test**: `mise test`, `mise test-race`, `mise test-coverage`
- **Lint**:
    - Run `mise lint` and fix the issues
- **Format**: `mise format`
- **Hooks**: `mise hooks`
- `grep` is an alias to `rg`.

## Code Style Guidelines

- **Imports**: Use goimports formatting, group stdlib, external, internal packages
- **Formatting**: Use gofumpt (stricter than gofmt), enabled in golangci-lint with `mise format`.
- **Naming**: Standard Go conventions - PascalCase for exported, camelCase for unexported
- **Types**: Prefer explicit types, use type aliases for clarity (e.g., `type AgentName string`)
- **Error handling**: Return errors explicitly, use `fmt.Errorf` for wrapping
- **Context**: Always pass context.Context as first parameter for operations
- **Interfaces**: Define interfaces in consuming packages, keep them small and focused
- **Structs**: Use struct embedding for composition, group related fields
- **Constants**: Use typed constants with iota for enums, group in const blocks
- **Testing**: Standard library `testing` only — no testify. Use `t.Parallel()`,
  `t.Setenv()`, and `t.TempDir()` (temp dirs are cleaned up automatically).
- **JSON tags**: Use snake_case for JSON field names
- **File permissions**: Use octal notation (0o755, 0o644) for file permissions
- **Comments**: End comments in periods unless comments are at the end of the line.

## Testing

ALWAYS run `mise test` and `mise test-race` before committing. Keep total
coverage at or above 50% — CI fails below that.

### Unit Tests
- Standard library `testing` only. Assert with `if got != want { t.Errorf(...) }`;
  use `t.Fatal` when the test cannot continue. The repo has no assertion
  library and should not grow one for a handful of tests.
- Use `t.Parallel()` for anything that does not touch the database or the
  environment.
- Table-driven tests for pure functions (`parseWMClass`, `parseIdleMs`,
  `Config.Validate`).
- `t.Helper()` in setup helpers.

### Environment Variables
- `t.Setenv` and `t.Parallel` are mutually exclusive — `t.Setenv` panics in a
  parallel test.
- A test asserting on config-file values must neutralise the `TRACKKR_*`
  overrides first, or it passes locally and fails in a shell that exports
  them. `internal/tracker/config_test.go` has `clearTrackkrEnv(t)` for this.
- Prefer restructuring so no env var is needed: a malformed config file, for
  example, fails parsing before any override is read, so that test can stay
  parallel.

### Database Tests
- Live in `internal/db`. `testPool(t)` (see `testhelper_test.go`) connects,
  runs migrations, and calls `t.Skipf` when Postgres is unreachable — so
  `mise test` passes on a machine with no database.
- Override the DSN with `TRACKKR_TEST_DSN`; the default targets the
  `mise db` compose service on port 5455.
- Do NOT use `t.Parallel()` in database tests, and clean up rows you create
  (`cleanupUser`).

### Fakes and Interfaces
- No mock framework. Define a small interface in the consuming package and
  write a struct that implements it.
- Existing examples: `mockQuerier` in `internal/server/testhelper_test.go`
  (implements `Querier`), and `HTTPPoster`, `WindowDetector`, `IdleDetector`
  in `internal/tracker` — all satisfied by hand-written test doubles.
- Use `httptest.NewServer` for reporter/HTTP tests rather than faking the
  transport when the real request path matters.

### Edge Cases to Test
- Zero and negative durations for anything reaching `time.NewTicker` — it
  panics on non-positive intervals.
- Empty and whitespace-only inputs; nil/empty slices and maps.
- URLs with and without trailing slashes (the reporter concatenates paths).
- Missing external binaries (`xdotool`, `xprintidle`) and unsupported
  platforms.


## Formatting

- ALWAYS format any Go code you write with `mise format`

## Comments

- Comments that live on their own lines should start with capital letters and
  end with periods. Wrap comments at 78 columns.

## Committing

- ALWAYS run `mise test` and `mise test-race` before pushing. Database-backed
  tests skip silently without Postgres, so start it with `mise db` when the
  change touches `internal/db`.
- ALWAYS use semantic commits (`fix:`, `feat:`, `chore:`, `refactor:`, `docs:`, `sec:`, etc).
- ALWAYS run prek hooks with `mise hooks` before pushing
- NEVER add attribution trailers to commit messages. No `Co-Authored-By:`,
  no "Generated with" lines, no tool or model names. This applies to agents
  whose defaults say otherwise.
- Try to keep commits to one line. Only use multi-line commits when additional
  context is truly necessary.
- Push to all remotes with `mise push-all`.

## Starting Work

Read this file at the start of every session.

Refer to @CLAUDE.md for additional guide

<!-- code-review-graph MCP tools -->
## MCP Tools: code-review-graph

**IMPORTANT: This project has a knowledge graph. ALWAYS use the
code-review-graph MCP tools BEFORE using Grep/Glob/Read to explore
the codebase.** The graph is faster, cheaper (fewer tokens), and gives
you structural context (callers, dependents, test coverage) that file
scanning cannot.

### When to use graph tools FIRST

- **Exploring code**: `semantic_search_nodes` or `query_graph` instead of Grep
- **Understanding impact**: `get_impact_radius` instead of manually tracing imports
- **Code review**: `detect_changes` + `get_review_context` instead of reading entire files
- **Finding relationships**: `query_graph` with callers_of/callees_of/imports_of/tests_for
- **Architecture questions**: `get_architecture_overview` + `list_communities`

Fall back to Grep/Glob/Read **only** when the graph doesn't cover what you need.

### Key Tools

| Tool | Use when |
|------|----------|
| `detect_changes` | Reviewing code changes — gives risk-scored analysis |
| `get_review_context` | Need source snippets for review — token-efficient |
| `get_impact_radius` | Understanding blast radius of a change |
| `get_affected_flows` | Finding which execution paths are impacted |
| `query_graph` | Tracing callers, callees, imports, tests, dependencies |
| `semantic_search_nodes` | Finding functions/classes by name or keyword |
| `get_architecture_overview` | Understanding high-level codebase structure |
| `refactor_tool` | Planning renames, finding dead code |

### Workflow

1. The graph auto-updates on file changes (via hooks).
2. Use `detect_changes` for code review.
3. Use `get_affected_flows` to understand impact.
4. Use `query_graph` pattern="tests_for" to check coverage.
