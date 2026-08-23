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
      "ParentReminder", "CreationDate", "LastModifiedDate"
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
      "recordName": "<UPPERCASE-UUID>",
      "fields": {
        "TitleDocument": {"value": "<base64-gzip-crdt>"},
        "Completed": {"value": 0},
        "List": {"value": {
          "recordName": "<exact-list-record-name>",
          "action": "NONE"
        }}
      }
    }
  }]
}
```

`TitleDocument` and `NotesDocument` use the Reminders CRDT protobuf structure;
they are not plain strings and do not use the legacy minimal protobuf encoding.
Do not invent `Reminder/` or `List/` prefixes and do not force CloudKit field
types in the request.

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
      "Priority": {"value": 5},
      "DueDate": {"value": 1787487524290}
    }
  }
}
```

Priority and due-date updates are live-tested. Title and notes replacements are
experimental: the server accepts a newly encoded CRDT snapshot but may later
reconcile the old text back into the record.

Completion is also experimental: send numeric `Completed` and
`CompletionDate`, but be aware that live tests observed CloudKit reconciling
both fields back to the incomplete state.

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
