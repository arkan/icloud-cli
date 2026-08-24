# iCloud API Documentation

Reverse-engineered from pyicloud and icloud.com network analysis.

## Endpoints

### Base URLs
- **Auth**: `https://idmsa.apple.com/appleauth/auth`
- **Home**: `https://www.icloud.com`
- **Setup**: `https://setup.icloud.com/setup/ws/1`
- **China**: Replace `.com` with `.com.cn`

### Authentication Flow

#### 1. Initial Sign-In
```
POST https://idmsa.apple.com/appleauth/auth/signin?isRememberMeEnabled=true
Content-Type: application/json

{
  "accountName": "user@example.com",
  "password": "password",
  "rememberMe": true,
  "trustTokens": ["<saved_trust_token>"]
}
```

**Required Headers:**
```
Accept: */*
Content-Type: application/json
X-Apple-OAuth-Client-Id: d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d
X-Apple-OAuth-Client-Type: firstPartyAuth
X-Apple-OAuth-Redirect-URI: https://www.icloud.com
X-Apple-OAuth-Require-Grant-Code: true
X-Apple-OAuth-Response-Mode: web_message
X-Apple-OAuth-Response-Type: code
X-Apple-OAuth-State: auth-<uuid>
X-Apple-Widget-Key: d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d
Origin: https://www.icloud.com
Referer: https://www.icloud.com/
```

**Response Headers to Capture:**
- `X-Apple-ID-Account-Country` → account_country
- `X-Apple-ID-Session-Id` → session_id
- `X-Apple-Session-Token` → session_token
- `X-Apple-TwoSV-Trust-Token` → trust_token
- `scnt` → scnt

#### 2. Account Login (with session token)
```
POST https://setup.icloud.com/setup/ws/1/accountLogin
Content-Type: application/json

{
  "accountCountryCode": "<account_country>",
  "dsWebAuthToken": "<session_token>",
  "extended_login": true,
  "trustToken": "<trust_token>"
}
```

**Response:** JSON with `webservices` map containing service URLs.

#### 3. Validate Session
```
POST https://setup.icloud.com/setup/ws/1/validate
Content-Type: application/json

null
```

Returns session info and webservices if valid.

#### 4. Trigger the 2FA notification

Apple's current authentication flow requires an explicit push trigger before
the code is displayed on trusted devices:

```http
PUT https://idmsa.apple.com/appleauth/auth/verify/trusteddevice/securitycode
```

The request has no body and includes the current `scnt` and
`X-Apple-ID-Session-Id` headers. HTTP `204` indicates that the trigger was
accepted; `200` and `202` are retained for compatibility with older behavior.

#### 5. Verify the 2FA code
```
POST https://idmsa.apple.com/appleauth/auth/verify/trusteddevice/securitycode
Content-Type: application/json

{
  "securityCode": {
    "code": "123456"
  }
}
```

**Required Headers:** Same as sign-in + `scnt` + `X-Apple-ID-Session-Id`

#### 6. Trust Session
```
GET https://idmsa.apple.com/appleauth/auth/2sv/trust
```

**Required Headers:** Same as 2FA verification.

---

## Reminders CloudKit API

The authenticated `ckdatabasews` URL from the account-login response is used as
the base. All calls target container `com.apple.reminders`, environment
`production`, database `private`, and custom zone `Reminders`.

This is an undocumented Apple service. The examples describe observed behavior,
not a stable public contract.

### Resolve the zone owner

```http
POST /database/1/com.apple.reminders/production/private/zones/list
```

Select the zone named `Reminders` and preserve its `ownerRecordName`. Every
subsequent zone request must use that exact pair.

### Fetch zone changes

```http
POST /database/1/com.apple.reminders/production/private/changes/zone
Content-Type: application/json

{
  "zones": [{
    "zoneID": {
      "zoneName": "Reminders",
      "ownerRecordName": "<owner>"
    },
    "desiredKeys": [
      "TitleDocument", "NotesDocument", "Name", "Completed",
      "CompletionDate", "DueDate", "List", "Deleted", "Priority",
      "ParentReminder", "Flagged", "CreationDate", "LastModifiedDate",
      "ResolutionTokenMap", "UrgentPresentationAlarmsAsData",
      "UrgentPresentationAlarmsChecksum", "DueDateDeltaAlertsData"
    ],
    "syncToken": "<previous-token>"
  }]
}
```

The response contains one entry in `zones`, with `records`, `syncToken`, and
`moreComing`. Continue paging while `moreComing` is true. Preserve full record
names and `recordChangeTag` values exactly as returned.

### Create a reminder

```http
POST /database/1/com.apple.reminders/production/private/records/modify
Content-Type: application/json

{
  "zoneID": {
    "zoneName": "Reminders",
    "ownerRecordName": "<owner>"
  },
  "operations": [{
    "operationType": "create",
    "record": {
      "recordType": "Reminder",
      "recordName": "Reminder/<UPPERCASE-UUID>",
      "fields": {
        "TitleDocument": {"value": "<base64-gzip-crdt>"},
        "Completed": {"value": 0, "type": "NUMBER_INT64"},
        "CreationDate": {"value": 1787601000000, "type": "TIMESTAMP"},
        "LastModifiedDate": {"value": 1787601000000, "type": "TIMESTAMP"},
        "Priority": {"value": 0, "type": "NUMBER_INT64"},
        "Flagged": {"value": 0, "type": "NUMBER_INT64"},
        "AllDay": {"value": 0, "type": "NUMBER_INT64"},
        "List": {"value": {
          "recordName": "<exact-list-record-name>",
          "action": "NONE"
        }},
        "ResolutionTokenMap": {
          "value": "<JSON map containing titleDocument, completed, creationDate, lastModifiedDate, priority, flagged, allDay, list, minimumSupportedVersion, and icsDisplayOrder tokens>",
          "type": "STRING"
        }
      }
    }
  }]
}
```

`TitleDocument` and `NotesDocument` use the Reminders CRDT protobuf structure;
they are not plain strings and do not use the legacy minimal protobuf encoding.
New reminder records use the native `Reminder/<UUID>` format. This prefix is
required for parent/subtask relationships to synchronize correctly. Preserve
exact record names returned by the server, preserve exact list record names,
and follow native typing: integer state fields use `NUMBER_INT64`, dates use
`TIMESTAMP`, and `ResolutionTokenMap` uses `STRING`. Document and reference
fields (`TitleDocument`, `NotesDocument`, `List`, and `ParentReminder`) retain
their native inferred representation without a forced type.

To create a subtask, add a `ParentReminder` reference to a parent in the same
list:

```json
"ParentReminder": {"value": {
  "recordName": "Reminder/<PARENT-UUID>",
  "action": "NONE"
}}
```

### Update scalar fields

First use `records/lookup` to obtain the current `recordChangeTag`, then send an
`update` operation containing only the changed fields:

```json
{
  "operationType": "update",
  "record": {
    "recordType": "Reminder",
    "recordName": "<exact-record-name>",
    "recordChangeTag": "<current-change-tag>",
    "fields": {
      "Priority": {"value": 5, "type": "NUMBER_INT64"},
      "DueDate": {"value": 1787487524290, "type": "TIMESTAMP"},
      "LastModifiedDate": {"value": 1787601000000, "type": "TIMESTAMP"},
      "ResolutionTokenMap": {
        "value": "<preserved map with priority, dueDate, and lastModifiedDate advanced>",
        "type": "STRING"
      }
    }
  }
}
```

Priority, due-date, title, and notes updates are live-tested. Text updates must
edit the existing CRDT document rather than submit a new snapshot. The writer
uses a stable client replica, tombstones the previous live substring, preserves
the existing operation history, and maintains two distinct clocks. Character
clocks are local to each replica: a new replica starts at character clock zero,
while a known replica continues from its own character vector clock. Tombstone
timestamps are logical/vector timestamps and advance beyond the other observed
replicas. Borrowing another replica's character clock or submitting a fresh
snapshot creates a concurrent branch that Reminders can ignore or concatenate
with the native value.

Every scalar or document update preserves the complete existing
`ResolutionTokenMap`, advances the token for each changed property, and advances
`lastModifiedDate`. If a legacy partial map is encountered, missing tokens for
present core fields are recovered above the largest surviving counter before
the requested property is advanced.

Completion is live-tested with `Completed` as `NUMBER_INT64` and
`CompletionDate` and `LastModifiedDate` as `TIMESTAMP`. The same update advances
the `completed`, `completionDate`, and `lastModifiedDate` resolution tokens.

Urgent alarms use a three-step asset write: request an upload target, upload the
JSON account-state envelope, then update `UrgentPresentationAlarmsAsData` as an
`ASSETID` alongside the encrypted string checksum in
`UrgentPresentationAlarmsChecksum`. The corresponding resolution-token key is
`urgentPresentationAlarmsChecksum`. The account's private person identifier is
discovered from existing synchronized records rather than fabricated.

### Add or remove a native tag

Tag creation atomically updates the reminder and creates a linked child record.
The `Name` field must be a `STRING` with `isEncrypted: true`; sending raw or
Base64-wrapped `ENCRYPTED_BYTES` is accepted by CloudKit but is not rendered as
a native tag by the iPhone app.

```json
{
  "atomic": true,
  "operations": [
    {
      "operationType": "update",
      "record": {
        "recordType": "Reminder",
        "recordName": "Reminder/<UUID>",
        "recordChangeTag": "<current-change-tag>",
        "fields": {
          "HashtagIDs": {"type": "STRING_LIST", "value": ["<TAG-UUID>"]},
          "ResolutionTokenMap": {"type": "STRING", "value": "<tokens>"},
          "LastModifiedDate": {"type": "TIMESTAMP", "value": 1787514558862}
        }
      }
    },
    {
      "operationType": "create",
      "record": {
        "recordType": "Hashtag",
        "recordName": "Hashtag/<TAG-UUID>",
        "parent": {"recordName": "Reminder/<UUID>"},
        "fields": {
          "Name": {"type": "STRING", "value": "work", "isEncrypted": true},
          "Reminder": {"value": {
            "recordName": "Reminder/<UUID>",
            "action": "VALIDATE"
          }}
        }
      }
    }
  ]
}
```

Removal uses the same atomic reminder update and a native CloudKit `delete`
operation for the Hashtag record. A soft `Deleted = 1` update remains visible
in the iPhone app and can prevent deletion of the parent reminder.

Global tag renaming is intentionally out of scope and will not be supported.
Clients should remove the old tag from affected reminders and add the
replacement tag instead.

### Assign a shared reminder

Assignment is available only when the reminder's list is the root of a
`cloudkit.share`. Resolve a user from the share's accepted `participants`; the
value stored by Reminders is the participant's `participantId`, not the email
address or CloudKit `userRecordName`. The current participant is the
assignment originator.

Creation atomically updates the reminder's `AssignmentIDs` and creates a linked
`Assignment` child:

```json
{
  "atomic": true,
  "operations": [
    {
      "operationType": "update",
      "record": {
        "recordType": "Reminder",
        "recordName": "Reminder/<REMINDER-UUID>",
        "recordChangeTag": "<current-change-tag>",
        "fields": {
          "AssignmentIDs": {
            "type": "STRING_LIST",
            "value": ["<ASSIGNMENT-UUID>"]
          },
          "ResolutionTokenMap": {"type": "STRING", "value": "<tokens>"},
          "LastModifiedDate": {"type": "TIMESTAMP", "value": 1787523904000}
        }
      }
    },
    {
      "operationType": "create",
      "record": {
        "recordType": "Assignment",
        "recordName": "Assignment/<ASSIGNMENT-UUID>",
        "parent": {"recordName": "Reminder/<REMINDER-UUID>"},
        "fields": {
          "AssignedDate": {"type": "TIMESTAMP", "value": 1787523904000},
          "EncryptedAssigneeIdentifier": {
            "type": "STRING",
            "value": "<TARGET-PARTICIPANT-ID>",
            "isEncrypted": true
          },
          "EncryptedOriginatorIdentifier": {
            "type": "STRING",
            "value": "<CURRENT-PARTICIPANT-ID>",
            "isEncrypted": true
          },
          "OwningReminderIdentifier": {
            "type": "STRING",
            "value": "<REMINDER-UUID>"
          },
          "Reminder": {"value": {
            "recordName": "Reminder/<REMINDER-UUID>",
            "action": "VALIDATE"
          }},
          "Status": {"type": "NUMBER_INT64", "value": 1}
        }
      }
    }
  ]
}
```

Reassignment deletes the previous `Assignment` child and creates the new child
in the same atomic request. Unassignment writes an empty `AssignmentIDs` list
and uses a native CloudKit `delete` for every previous assignment record. The
production container accepts this contract, and both assignment and
unassignment are verified in the native iPhone app.

### Add, replace, or remove a location alarm

A location alarm is represented by two linked children: an `Alarm` below the
reminder and an `AlarmTrigger` below the alarm. The reminder stores the raw
alarm UUID in `AlarmIDs`. Creation updates all three records atomically.

The request-side numeric type names are `NUMBER_INT64` and `NUMBER_DOUBLE`.
The alarm's `DueDateResolutionTokenAsNonce` is `0`, and the trigger includes
`Imported = 0`. These details were captured from a location created by the
native iPhone app; the superficially similar `INT64`/`DOUBLE` payload used by
some third-party clients is rejected by the production container.

The location trigger contains encrypted `Title`, `Address`, `Latitude`,
`Longitude`, and `ReferenceFrameString` fields, plus the unencrypted radius and
proximity. Proximity `1` means arriving and `2` means leaving. Although iOS
does not expose a radius control, it persists a numeric radius chosen by the
native UI; the CLI therefore accepts the radius explicitly in meters.

Replacement and removal first inspect all linked alarms. Only triggers whose
`Type` is `Location` are deleted, so time-based alarms remain attached. Child
records are removed with native CloudKit `delete` operations and their current
change tags. Creation, replacement, and removal are verified in the native
iPhone app.

### Add, replace, or remove a URL

A URL is an `Attachment` child linked through the reminder's `AttachmentIDs`
string list. The child uses `Type = URL`, `UTI = public.url`, a validating
`Reminder` reference, and an encrypted `URL` string. `Deleted` and `Imported`
are numeric zero values. The child and reminder are created or updated in one
atomic request.

Replacement and removal look up every linked attachment and delete only those
whose `Type` is `URL`; images and other attachment types remain linked. The
parent update also refreshes `LastModifiedDate` but leaves
`ResolutionTokenMap` unchanged, matching a record captured after editing the
URL in the native iPhone app. Creation, replacement, and removal are verified
on iPhone.

### Add, update, or remove recurrence

A recurrence is a `RecurrenceRule/<UUID>` child whose UUID is stored without
the record-type prefix in the reminder's `RecurrenceRuleIDs` string list. The
child has a CloudKit parent and a validating `Reminder` reference. Its numeric
fields include `Frequency` (`0` daily, `1` weekly, `2` monthly, `3` yearly),
`Interval`, `OccurrenceCount`, `FirstDayOfTheWeek`, `Imported`, and `Deleted`.

Creation is order-sensitive even inside an atomic modification. The child must
be created before the reminder is updated to reference its UUID. Writing the
parent first produces a structurally valid CloudKit graph that the native iOS
app does not materialize.

An existing rule is updated in place with its current change tag. Replacing it
with a new Web-created child leaves iOS displaying the previous device-local
rule. Clearing recurrence first writes an empty `RecurrenceRuleIDs` list on the
parent and then uses the native CloudKit `delete` operation for the child. A
soft deletion using `Deleted = 1` is accepted by the server but does not clear
the recurrence in the iOS app. These three transitions were verified on a
native iPhone.

### Add, replace, or remove an Early Reminder

Early Reminders are not relative `AlarmTrigger` children. The reminder stores
one Base64-encoded JSON envelope in the encrypted-bytes field
`DueDateDeltaAlertsData`. The envelope contains the reminder UUID, a private
account UUID, `minimumSupportedVersion = 20230430`, and a
`dueDateDeltaAlerts` array. Each alert contains an uppercase UUID, a Core Data
creation timestamp, `minimumSupportedAppVersion = 0`, a unit, and a negative
count representing an offset before the due date.

The unit mapping is `0` minutes, `1` hours, `2` days, `3` weeks, and `4`
months. For example, 15 minutes before the due date is encoded as unit `0` and
count `-15`. Replacement writes exactly one array entry. Removal preserves the
envelope metadata but writes an empty array. Every transition increments the
`dueDateDeltaAlertsData` entry in `ResolutionTokenMap` and updates
`LastModifiedDate`.

The private account UUID is not present on the CloudKit `Account`, `List`, or
ordinary reminder records observed during research. The client discovers it
from any existing `DueDateDeltaAlertsData` envelope in the synchronized zone;
if none exists, it asks the user to create one native Early Reminder first.
Creation, replacement, and removal were verified in the native iPhone app.

### Permanently unsupported: When Messaging trigger

The native app stores the selected contact's complete set of email addresses
and phone numbers as encrypted JSON in the Reminder field `ContactHandles`.
Its resolution-token key is `contactHandles`. This field is readable through
CloudKit, but it is not a sufficient write contract: accepted direct updates
did not activate the trigger on iPhone, and removing the field did not reliably
clear the device-local state even with an incremented CRDT token.

EventKit publicly documents only time- and location-based reminder alarms, and
the tested open-source clients only read this private Messages property. The
CLI deliberately does not expose mutation flags for it. This property is
permanently out of scope and will not be supported.

### Permanently unsupported: image attachments

The native data model links a Reminder to an `Attachment` child through
`AttachmentIDs`. Image children expose fields such as `Type`, `Reminder`,
`FileAsset`, `FileName`, `FileSize`, `Width`, `Height`, and `UTI`. Reading this
shape is not enough to create one safely: CloudKit assets require a separate
three-stage upload flow before the returned asset receipt can be written into
the child record.

The only device-verified open-source implementation found creates images
through Apple's private ReminderKit framework on macOS. Other CloudKit clients
only map attachment records for reading, and no cross-platform writer with a
native-device-verified record contract was found. Image attachment mutation is
permanently out of scope and will not be supported.

### Delete a reminder

Use the native operation and current change tag:

```json
{
  "operationType": "delete",
  "record": {
    "recordName": "<exact-record-name>",
    "recordChangeTag": "<current-change-tag>"
  }
}
```

Every entry in a `records/modify` response must be inspected for
`serverErrorCode` and `reason`; an HTTP 200 alone does not prove success.

---

## Session Persistence

Store in `~/.icloud-cli/`:
- `session.json` - Session tokens and data
- `cookies.json` - HTTP cookies

```json
{
  "client_id": "auth-<uuid>",
  "session_id": "...",
  "session_token": "...",
  "trust_token": "...",
  "scnt": "...",
  "account_country": "FRA"
}
```
