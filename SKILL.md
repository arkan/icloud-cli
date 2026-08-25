---
name: icloud-cli
description: Operate the iCloud CLI to authenticate, manage iCloud Reminders, or diagnose Reminders synchronization and CloudKit record corruption. Use for reminder operations through the `icloud-cli` binary; route source-code development to the repository's normal engineering process.
---

# iCloud CLI Operations

Manage the authenticated account through the CLI with an inspect-mutate-verify loop. Treat Apple Reminders as live user data.

## 1. Resolve the executable and capability

1. Use an executable explicitly supplied by the user. Otherwise, resolve `icloud-cli` from `PATH`.
2. Build the current repository into a temporary directory only when the user explicitly requests the local version or local validation; reuse that binary for the run.
3. Read `<icloud-cli> --help` and the relevant subcommand help before choosing flags. The running binary is the command-syntax authority.
4. For a requested capability absent from help, read the [README known limitations](README.md#known-limitations) before responding.

This step is complete when the executable, supported command branch, and relevant flags are known. Route requests to change the Go source to the repository's engineering process rather than this skill.

## 2. Establish authenticated state

Run `<icloud-cli> status` before an account-dependent operation. If authentication is required, launch interactive `<icloud-cli> login [apple-id]` and hand password and 2FA entry to the user. Verify success with `status`.

Use the session through the CLI. Keep the session and cache files unread, and keep credentials out of commands, logs, and reports.

Run `<icloud-cli> logout` only when the user explicitly requests it, then report that the local session was cleared.

This step is complete when the CLI reports a valid session or the exact authentication blocker is reported.

## 3. Inspect and authorize

Resolve lists with `reminders lists`, candidates with `reminders ls`, and every mutation target with `reminders show <id> --json`. Prefer a full record name or GUID after discovery; use a prefix only when it is unique. Capture the target's relevant state before changing it.

Apply these authorization rules:

- Read-only operations and diagnostics proceed immediately.
- A creation, edit, or completion proceeds when the user explicitly requested that mutation.
- An existing reminder is never an experiment. Validation probes use a newly created title prefixed `CODX-<YYYYMMDD-HHMM>-<purpose>` and remain in place unless the user explicitly authorizes deletion.
- A deletion requires confirmation immediately before `reminders rm`, even when discussed earlier.
- A bulk mutation requires presenting the exact IDs and planned changes, then obtaining one confirmation. Process one reminder at a time, verify it, and stop at the first failure.
- An ambiguous target requires disambiguation before mutation.
- For assignment, resolve accepted participants with `reminders sharees <list>`. Proceed only when the user's request explicitly identifies the participant; never infer an identity.

This step is complete when every target is exact, its pre-change state is captured, and the planned mutation satisfies the applicable authorization rule.

## 4. Execute the smallest supported mutation

Use the `reminders` command surface for ordinary work. Combine requested `edit` flags in one command when the CLI supports the combination, preserving properties outside the request. Use `add --parent` for subtasks, `done` for completion, and `rm` only after its action-time confirmation.

Cover the full feature surface exposed by the running binary, including notes, timed or all-day due dates, timezone, priority, flags, tags, URL, recurrence, early alerts, Urgent alarms, location, shared-list assignment, and subtasks. Consult the [README](README.md) when an advanced property has account-specific prerequisites or native-app limitations.

This step is complete when the command succeeds once, or when execution stops with the original error preserved. Never continue a bulk operation past a failed item.

## 5. Verify the result

Capture the returned ID after creation. After every creation, edit, completion, or assignment, run `reminders show <id> --json` and compare the requested fields plus important untouched fields against the pre-change state. After deletion, verify that an exact lookup no longer resolves the record.

Also verify through Reminders for macOS when the operation changes a title, notes, emoji-bearing text, recurrence, early alert, Urgent alarm, location, assignment, or subtask relationship, or whenever the user requests native verification. Use an authorized macOS UI tool and read the displayed state without modifying unrelated reminders.

Use iCloud.com only to diagnose synchronization, decoding, or corruption failures. Confirm both the Reminders application and the home-page tile when the reported failure involves either surface.

This step is complete when the requested state agrees across every required surface, or when the report names the precise divergent surface and observed value.

## 6. Diagnose CloudKit only when needed

Enter this branch for unexplained sync failures, malformed records, or iCloud.com decoding errors. Read [docs/API.md](docs/API.md) before using `cloudkit zones`, `records`, `lookup`, or `dump`. Keep these operations read-only and narrow them to the affected zone and record whenever possible.

Use `reminders show --raw` only when stable JSON and native UI evidence cannot localize the problem. Treat raw records as sensitive: inspect them locally and report only the fields, invariants, and identifiers needed to support the conclusion. Do not reproduce the full payload.

Before repairing an existing record, identify the exact record, snapshot its business fields, explain the proposed normalization, and obtain explicit authorization for that record. Repair through supported reminder mutations, then repeat the verification step.

This branch is complete when the failure is localized to a command, record field, or external surface; the authorized repair is verified; or a concrete blocker is reported with reproduction evidence.

## 7. Report

Finish with a concise audit containing:

- affected reminder IDs and lists;
- properties read or changed;
- CLI, macOS, and iCloud.com verification actually performed;
- divergences, unsupported capabilities, or blockers;
- whether any test reminders remain.

Include detailed commands or raw CloudKit excerpts only when the user asks for them, and keep sensitive values redacted.
