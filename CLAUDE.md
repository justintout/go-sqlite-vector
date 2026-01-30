# Claude Code Instructions

This file governs how Claude Code works in this repository. These rules are mandatory.

## Reference Documents

- **SPEC.md**: the authoritative specification. All implementation decisions come from this document.
- **PLAN.md**: the step-by-step implementation plan. Work proceeds in plan part order.

## Plan Execution Rules

1. **Follow PLAN.md top-to-bottom.** Do not skip parts. Do not start a later part before the current part is fully validated.

2. **Validate before moving on.** Every plan part has a Validation section with specific commands and checks. Run every validation step. Only proceed to the next part when all validation checks pass.

3. **If validation fails:**
   - Do NOT silently fix and move on.
   - Record the failure in the part's **Failure Log** section in PLAN.md. Include:
     - The exact command that failed
     - The full error output
     - A brief description of the root cause
     - What was done to fix it
   - Fix the issue, then re-run all validation steps for the current part.
   - Update the Failure Log with the resolution.

4. **Update PLAN.md continuously.** As work progresses:
   - Check off completed items with `[x]`
   - Mark in-progress items with `[~]`
   - Mark failed items with `[!]` and add details to the Failure Log
   - The plan must always reflect the current true state of the implementation.

## Git and Commit Rules

1. **A git repository is already initialized.** All work happens on the default branch.

2. **Every plan part must end with a commit.** Commit even if validation initially failed — the failure and fix should both be in the history. The commit message is specified in each plan part.

3. **No major changes without a commit first.** Before starting any significant refactor, rewrite, or new part, ensure the current state is committed. This guarantees work can always be restored.

4. **Commit granularity:** one commit per plan part. Do not squash multiple parts into one commit. Do not split a single part across multiple commits unless a mid-part checkpoint is needed to preserve work before a risky change.

5. **Never amend previous commits.** Always create new commits. If something from a previous part needs fixing, fix it in the current part and note it in the Failure Log.

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
