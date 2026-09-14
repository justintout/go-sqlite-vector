# Claude Code Instructions

This file governs how Claude Code works in this repository. These rules are mandatory.

## Reference Documents

- **SPEC.md**: the authoritative specification. All implementation decisions come from this document.

## Git and Commit Rules

1. **A git repository is already initialized.** All work happens on the default branch.

2. **No major changes without a commit first.** Before starting any significant refactor or rewrite, ensure the current state is committed. This guarantees work can always be restored.

3. **Never amend previous commits.** Always create new commits.

## Code Quality Standards

- Follow the SPEC.md exactly. Do not add features, functions, or behaviors not specified.
- Do not add comments or docstrings beyond what is necessary for exported symbols.
- Do not add error handling for impossible conditions.
- Use standard library only (plus zombiezen.com/go/sqlite). No other dependencies.
- Keep all code in the single `vector` package at the repository root.

## Testing Standards

- All tests are table-driven integration tests run against real SQLite connections.
- Tests must be deterministic — no random inputs in test cases (benchmarks may use random data for setup).
- Test names should clearly describe what they verify.
- Use `go test -race ./...` as the final validation to check for data races.
