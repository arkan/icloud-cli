# iCloud Reminders web corruption

## Goal

Restore valid Reminders records and prevent the CLI from producing data that breaks iCloud.com.

## Plan

1. Reproduce iCloud.com load failure; capture failing response/console signal.
2. Identify malformed test record/field; minimise to one mutation.
3. Add regression test at CloudKit payload boundary.
4. Fix serialization/mutation semantics with minimal scope.
5. Verify tests, CLI ↔ macOS, then iCloud.com recovery.

## Safety

- Do not delete any reminder.
- Use only the two `CODX-SYNC-20260824-2037-*` test records for repair probes.
- Normalize only `Reminder/B6ADFC31-1DCE-4173-A114-14E4D9F21688` and
  `Reminder/D4066EFA-B890-48F6-9280-0080F45804E7`, after the user explicitly
  authorized those two records.
- Inspect before mutation; preserve before/after evidence.

## Open questions

- None.

## Findings

- The iCloud.com Reminders app loads and the observed CloudKit requests return
  HTTP 200. The failing surface is the iCloud.com home-page Reminders tile.
- The tile logs `Corrupt mergeable string, length of substrings !=
  attributedString length`, followed by `RemindersDecodingError: Failed to
  parse Reminder title` in `asyncRemindersConverter`.
- Two pre-existing integration reminders contain invalid title documents:
  `Reminder/B6ADFC31-1DCE-4173-A114-14E4D9F21688` has a concatenated CRDT
  title, and `Reminder/D4066EFA-B890-48F6-9280-0080F45804E7` counts an emoji as
  one character instead of two UTF-16 code units.
- Sequential property updates replaced the entire resolution-token map. That
  left the campaign reminder with only completion tokens and made later native
  conflict resolution unreliable.
- A new CRDT replica incorrectly inherited the largest character clock from
  another replica. Reminders consequently treated replacement text as a
  concurrent branch and concatenated old and new titles.

## Implemented safeguards

- Encode CRDT lengths in UTF-16 code units and start each new replica's
  character clock at zero.
- Preserve native resolution tokens, recover missing core tokens above the
  largest surviving counter, and update only the requested token plus the
  modification token.
- Emit the native scalar fields and resolution tokens on creation; request
  early-alert baseline data during sync.
- Reject unknown list names, preflight account-dependent early/Urgent options,
  reject recurrence or early alerts without a numeric due date, and compensate
  late creation failures by deleting only the newly-created record.

## Verification evidence

- Before repair, the campaign reminder lacked title, notes, priority, flag,
  date, all-day, timezone, list, and creation resolution tokens.
- After repair, `CODX-SYNC-20260824-2037-A CLI final` appears verbatim in
  Reminders for macOS with its notes, medium priority, flag, timed due date,
  recurrence, tags, location, URL, and completed state intact.
- The reverse path remains valid: the native title
  `CODX-SYNC-20260824-2037-B APP native edited` is read verbatim by the CLI.
- With explicit permission, the two malformed records were re-encoded without
  deletion. `B6ADFC31` now resolves consistently to `Native promoted baseline`
  with notes `Native notes bridge`; its completed state, completion date,
  timed due date, timezone, list, priority, flag, and other business fields
  remain intact. `D4066EFA` retains its exact emoji title, notes, list, and
  incomplete state.
- The CLI reads both normalized records verbatim. Reminders for macOS shows the
  same titles and notes after an application relaunch, including the emoji and
  the completed/due state on `B6ADFC31`.
- After a fresh reload, the iCloud.com home-page Reminders tile renders its
  reminder grid again and the direct Reminders app loads its list tree. Neither
  surface logs `Corrupt mergeable string`, `RemindersDecodingError`, or
  `Failed to parse Reminder title`.
- The direct Apple web client still emits unrelated framework warnings about
  circular model properties and a missing `MeCardServiceToChildProtocol`
  JSON-RPC registration. They do not occur on the home-page tile, do not block
  Reminders, and are outside the CLI data format or repository code.
- Regression coverage includes a captured native edited-title fixture, both
  captured corrupt title documents (UTF-16 emoji mismatch and concatenated
  branch), per-replica clocks, partial token-map recovery, token preservation,
  unknown-list rejection, account preflights, and late-failure rollback for
  due, early, and Urgent paths.
