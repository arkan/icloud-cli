# iCloud CLI operational skill

## Goal

Create a model-invoked root `SKILL.md` that operates the `icloud` binary safely
and comprehensively without turning into a source-development skill.

## Contract

- Prefer the installed binary; use a temporary build only for explicit local-version work.
- Cover authentication, all supported reminder operations, and read-only CloudKit diagnosis.
- Inspect exact targets before writes and verify every result.
- Require action-time confirmation for deletion, bulk mutation, or ambiguous targets.
- Isolate experiments in new `CODX-<timestamp>-*` reminders and preserve them by default.
- Use macOS Reminders for protocol-sensitive verification and iCloud.com for sync/corruption diagnosis.
- Keep credentials, session files, and raw CloudKit records out of reports.
- Reuse `README.md` and `docs/API.md` as conditional references; add no duplicate resources.

## Implementation

1. Write concise model-invocation metadata and a deterministic operational loop.
2. Encode authorization, shared-list, bulk, diagnostic, and verification branches with checkable completion criteria.
3. Keep unsupported capability routing and protocol details behind pointers to existing documentation.

## Verification

- Run the bundled skill validator.
- Audit representative read, write, delete, bulk, assignment, authentication, and corruption scenarios without live account writes.
- Run Markdown/diff checks and inspect the final package for duplication or unfinished scaffolding.

## Unresolved questions

None.
