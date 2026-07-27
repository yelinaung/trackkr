 # Development Guide

## Build/Test/Lint Commands

- **Go version**: 1.26+
- **Build**: `mise build .` or `go run .`
- **Test**: `mise test`
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
- **Testing**: Use testify's `require` package, parallel tests with `t.Parallel()`,
  `t.SetEnv()` to set environment variables. Always use `t.Tempdir()` when in
  need of a temporary directory. This directory does not need to be removed.
- **JSON tags**: Use snake_case for JSON field names
- **File permissions**: Use octal notation (0o755, 0o644) for file permissions
- **Comments**: End comments in periods unless comments are at the end of the line.

## Testing with Mock Providers

When writing tests that involve provider configurations, use the mock providers to avoid API calls:

```go
func TestYourFunction(t *testing.T) {
    // Enable mock providers for testing
    originalUseMock := config.UseMockProviders
    config.UseMockProviders = true
    defer func() {
        config.UseMockProviders = originalUseMock
        config.ResetProviders()
    }()

    // Reset providers to ensure fresh mock data
    config.ResetProviders()

    // Your test code here - providers will now return mock data
    providers := config.Providers()
    // ... test logic
}
```
ALWAYS RUN these `mise` commands:
- test
- test-race

ENSURE that the test coverage stays at or above 50% (CI enforced).

## Test Patterns

### Unit Tests
- Use `t.Parallel()` for tests that don't need database.
- Use table-driven tests for pure functions.
- Use `testify/require` for assertions.
- Use `t.Helper()` in test setup functions.

### Database Tests
- Use `database.TestDB(t)` which skips if `TEST_DATABASE_URL` not set.
- Run with `-p 1` to avoid race conditions.
- Do NOT use `t.Parallel()` for database tests.

### Mocking External Dependencies
- Use interfaces for external SDK calls (e.g., Gemini API).
- Use adapter pattern to wrap SDK structs.
- Create separate constructors for testing (e.g., `NewClientWithGenerator`).
- See `internal/bot/mocks/` for Telegram bot mocks.

### Handler Testing
- Handlers take concrete `*bot.Bot` type, not interface.
- Use wrapper functions to test handler logic without calling real handlers.
- Callback handlers use `EditMessageText` instead of `SendMessage`.

### Edge Cases to Test
- nil/empty slices and maps.
- Whitespace-only inputs.
- Bot mention formats in commands.
- Non-existent IDs for update/delete operations.


## Formatting

- ALWAYS format any Go code you write with `mise format`

## Comments

- Comments that live on their own lines should start with capital letters and
  end with periods. Wrap comments at 78 columns.

## Committing

- ALWAYS run both unit and integraton tests before pushing
    - Especially, the fail tests with `mise test-integration 2&>1 | grep -w 'FAIL:'`
- ALWAYS use semantic commits (`fix:`, `feat:`, `chore:`, `refactor:`, `docs:`, `sec:`, etc).
- ALWAYS run prek hooks with `mise hooks` before pushing
- NEVER add attribution trailers to commit messages. No `Co-Authored-By:`,
  no "Generated with" lines, no tool or model names. This applies to agents
  whose defaults say otherwise.
- Try to keep commits to one line. Only use multi-line commits when additional
  context is truly necessary.
- Push to all remotes with `mise push`.

## Working on the TUI (UI)
Anytime you starts the work, read the AGENTS.md file

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
