# iCloud CLI

A command-line interface for Apple iCloud Reminders using the undocumented
CloudKit web service used by iCloud.com.

> **Experimental write support** — the previous E2EE blocker was a protocol
> misdiagnosis. The current implementation writes the complete Reminders CRDT
> document format and uses account-specific CloudKit zone metadata. Automated
> contract tests pass, but a live Apple-device sync must still be verified for
> each relevant account configuration before relying on it.

## Features

- Apple ID authentication with 2FA support
- Persistent authenticated sessions
- List and search reminders and lists, with subtasks rendered as a tree
- Add reminders with notes, due dates, and priorities
- Create subtasks, manage native tags, set or clear flags, assign shared reminders, and manage location alarms
- Set or clear due dates and native daily, weekly, monthly, or yearly recurrence
- Add, replace, or clear native Early Reminders relative to a due date
- Enable or disable native Urgent alarms
- Edit titles, notes, due dates, and priorities
- Mark reminders complete
- Delete reminders
- CloudKit delta synchronization with optimistic locking

## Installation

```bash
go install github.com/arkan/icloud-cli/cmd@latest
```

Or build from source:

```bash
git clone https://github.com/arkan/icloud-cli.git
cd icloud-cli
go build -o icloud ./cmd/
```

## Usage

```bash
# Authenticate and inspect the session
icloud login user@example.com
icloud status

# Read reminders
icloud reminders lists
icloud reminders ls
icloud reminders ls "Shopping"
icloud reminders ls --flat "Shopping"
icloud reminders show ABC12345
icloud reminders show ABC12345 --json
icloud reminders show ABC12345 --raw

# Create and mutate reminders
icloud reminders add "Buy milk" -l "Shopping"
icloud reminders add "Call mom" --due "tomorrow 14:00" --priority high
icloud reminders add "Buy detergent" --parent ABC12345
icloud reminders edit ABC12345 --title "Buy oat milk" --description "Get two cartons"
icloud reminders edit ABC12345 --due "2026-09-01" --priority medium
icloud reminders edit ABC12345 --flagged
icloud reminders edit ABC12345 --no-flagged
icloud reminders edit ABC12345 --tag work --tag urgent
icloud reminders edit ABC12345 --remove-tag work
icloud reminders sharees "Shared Shopping"
icloud reminders edit ABC12345 --assign alex@example.com
icloud reminders edit ABC12345 --assign me
icloud reminders edit ABC12345 --unassign
icloud reminders edit ABC12345 --location-title "Office" --address "1 Infinite Loop" \
  --latitude 37.3318 --longitude -122.0312 --radius 150 --proximity arriving
icloud reminders edit ABC12345 --clear-location
icloud reminders edit ABC12345 --url "https://example.com"
icloud reminders edit ABC12345 --clear-url
icloud reminders edit ABC12345 --repeat daily
icloud reminders edit ABC12345 --repeat weekly --repeat-interval 2
icloud reminders edit ABC12345 --repeat monthly --repeat-until 2026-12-31
icloud reminders edit ABC12345 --clear-repeat
icloud reminders edit ABC12345 --early-reminder 15m
icloud reminders edit ABC12345 --early-reminder clear
icloud reminders edit ABC12345 --urgent
icloud reminders edit ABC12345 --no-urgent
icloud reminders done ABC12345
icloud reminders rm ABC12345
```

Reminder commands accept a full CloudKit record name or a unique prefix shown
by `icloud reminders ls`. Subtasks are displayed below their parent by default;
use `--flat` to preserve the server order without hierarchy.

`reminders show` displays every supported property of one reminder: list,
parent, completion state, priority, flag, due date and timezone, URL, tags,
assignment, location, recurrence, Early Reminder, Urgent state, timestamps,
notes, and immediate subtasks. Missing values are shown as `—`. `--json`
provides a stable representation for scripts, including empty or null fields.
`--raw` prints the underlying CloudKit record, including private undocumented
fields, and should be handled as sensitive account data. Use `--no-color` for
deterministic human-readable text.

## How writes work

Reminders titles and notes are not plain strings. They are CRDT documents
encoded as protobuf, gzip, and Base64. Creation supplies the full document
structure, including operations, positions, replica metadata, Unicode character
counts, and a document UUID. Updates preserve the document history, tombstone
the previous live substring, and advance a stable client replica beyond every
observed replica timestamp. This prevents concurrent branches from being
concatenated or an Apple device's older value from winning reconciliation.

CloudKit operations also use:

- native `Reminder/<UUID>` names for newly created reminders, which are required
  for parent/subtask relationships, and exact server-provided names thereafter;
- exact list record names returned by the server;
- the Reminders zone's real `ownerRecordName` from `zones/list`;
- `changes/zone` and its delta token for synchronization;
- `recordChangeTag` for conflict-safe updates and deletes;
- the native CloudKit `delete` operation rather than a synthetic `Deleted`
  field update.

The `chainProtectionInfo`, `chainParentKey`, and `chainPrivateKey` fields seen on
existing records are not generated by this client. Current evidence indicates
that they are not client-supplied prerequisites for this CloudKit web write
path. This API remains undocumented and Apple may change that behavior.

## Verification

The default suite is read-only and uses local HTTP test servers:

```bash
go test ./...
go vet ./...
```

The live lifecycle test creates, reads, updates, and deletes a uniquely named
reminder using the session in `~/.icloud-cli/session.json`. It is deliberately
opt-in because it writes to the authenticated account:

```bash
ICLOUD_INTEGRATION=1 go test ./internal/cloudkit \
  -run TestIntegrationReminderLifecycle -v
```

After the create step, also confirm on an Apple device that the reminder appears
and renders correctly. A dedicated test account is recommended.

## Known limitations

- The CloudKit API and Reminders CRDT format are undocumented.
- Live behavior may differ with Advanced Data Protection, shared lists, or
  future Apple server changes.
- Title and notes replacement is verified across repeated CLI edits and native
  Apple edits. It depends on an undocumented CRDT format and may require future
  maintenance if Apple changes that format.
- Due-date creation, replacement, and removal are verified. Timed due dates use
  the configured IANA timezone; date-only values remain all-day reminders.
- Tag creation and removal are verified. Global tag renaming is intentionally
  out of scope and will not be supported; remove the old tag and add the
  replacement instead.
- Shared-list assignment and unassignment are verified for accepted participants.
- Location alarm creation, replacement, and removal are verified in the native
  iPhone app. Coordinates are required; `--proximity` accepts `arriving` or
  `leaving`. iOS chooses a radius in its own UI but does not expose a radius
  control; the CLI accepts an explicit radius in meters.
- Native URL attachment creation, replacement, and removal are verified in the
  iPhone app. Setting a URL preserves attachments of other types.
- Daily, weekly, monthly, and yearly recurrence creation, modification, and
  removal are verified in the native iPhone app. Creation must write the child
  rule before the parent relation; modification preserves the existing native
  rule identity; removal uses a native CloudKit delete rather than `Deleted=1`.
- Image attachments are permanently out of scope and will not be supported.
  They require a multi-stage CloudKit
  asset upload followed by an undocumented `Attachment` record mutation. The
  only verified writer found uses Apple's private ReminderKit on macOS, so no
  cross-platform contract has been validated on an Apple device.
- A timed due date creates its native date alarm. Early Reminder creation,
  replacement, and removal are verified in the native iPhone app. Accepted
  units are minutes (`m`), hours (`h`), days (`d`), weeks (`w`), and months
  (`mo`). Apple stores a private account UUID only inside this metadata; if an
  account has never synchronized a native Early Reminder, create one once in
  Reminders before the CLI can discover that identifier.
- Urgent alarm creation and removal are verified in the native Reminders apps.
  The private CloudKit contract stores the per-account state in an uploaded
  asset. If the account has never synchronized an Urgent alarm, enable it once
  in Reminders so the CLI can discover the private person identifier.
- The “When Messaging” trigger is permanently out of scope and will not be
  supported. CloudKit exposes its encrypted
  `ContactHandles` field, but direct writes and removals did not materialize
  reliably in the native iPhone app; the state also depends on local
  Contacts/Messages resolution that is unavailable to this cross-platform CLI.
- Marking a reminder complete is verified in the native Reminders apps. It uses
  the native integer completion state, completion timestamp, last-modified
  timestamp, and matching resolution tokens.
- The local CloudKit cache contains reminder metadata and is specific to the
  authenticated zone owner. Remove it to force a full resynchronization.

## Configuration

Session data is stored with mode `0600` in `~/.icloud-cli/session.json`. The
derived CloudKit record cache is stored with the same permissions in
`~/.icloud-cli/cloudkit-cache.json`.

## API documentation

See [docs/API.md](docs/API.md) for the reverse-engineered protocol notes.

## License

MIT
