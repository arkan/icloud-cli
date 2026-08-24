# Bidirectional iCloud CLI ↔ Reminders validation

## Goal

Manually confirm bidirectional synchronization for every reminder property the
CLI actually supports, without modifying code or touching existing reminders.

## Safety rules

- Use only newly-created reminders whose titles start with a unique campaign prefix.
- Never delete an existing reminder.
- Derive syntax and supported properties from the CLI documentation and help before mutation.
- Verify CLI → Reminders and Reminders → CLI separately.

## Scenarios

1. Inventory commands, properties, validations, and limits.
2. Create and edit new reminders with the CLI; verify each state in Reminders.
3. Create and edit new reminders in Reminders; verify each state with the CLI.
4. Cover title, notes, timed due date, priority, list, completion, and every other exposed property.
5. Record results, divergences, limits, and reproduction steps.

## Supported surface

- Subtasks, URL, recurrence, flag, tags, location, assignment, early alerts,
  and Urgent are exposed. Images and “When Messaging” are not.
- Priorities are `high`, `medium`, `low`, and `none`. Dates accept forms
  including `YYYY-MM-DD HH:MM` with an IANA timezone.
- A list is selected at creation; the CLI does not move an existing reminder.

## Executed result

- Only two new reminders were used in list FP:
  `CODX-SYNC-20260824-2037-A` (CLI origin) and
  `CODX-SYNC-20260824-2037-B` (Reminders origin). No pre-existing reminder was
  modified or deleted.
- CLI → Reminders confirmed: creation, read, title, notes, list, timed due date
  and timezone, medium priority, flag, multiple tags, URL, recurrence, location,
  and completion.
- Reminders → CLI confirmed: native creation, title edit, list, and completion.
- Assignment was not mutated to avoid notifying a third party. Subtasks,
  clearing properties, moving lists in Reminders, and the inverse matrix for
  advanced details were not completed because the authorized macOS UI path did
  not persist those detail-panel changes reliably.
- Early and Urgent account-dependent paths could not be exercised end-to-end
  without an existing native account baseline. Their no-partial-write behavior
  is covered by implementation tests in the subsequent repair campaign.
