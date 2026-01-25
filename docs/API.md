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

#### 4. 2FA Verification
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

#### 5. Trust Session
```
GET https://idmsa.apple.com/appleauth/auth/2sv/trust
```

**Required Headers:** Same as 2FA verification.

---

## Reminders API

### Get All Reminders and Lists
```
GET https://<reminders_service_url>/rd/startup
```

**Query Params:**
- `clientVersion=4.0`
- `lang=en-us`
- `usertz=Europe/Paris`
- `dsid=<dsid>` (from webservices)

**Response:**
```json
{
  "Collections": [
    {
      "guid": "tasks",
      "title": "Reminders",
      "ctag": "...",
      "order": 1
    }
  ],
  "Reminders": [
    {
      "guid": "reminder-uuid",
      "pGuid": "tasks",
      "title": "Buy milk",
      "description": "",
      "priority": 0,
      "dueDate": [20260125, 2026, 1, 25, 14, 30],
      "completedDate": null,
      "createdDateExtended": 1706123456789
    }
  ]
}
```

### Create Reminder
```
POST https://<reminders_service_url>/rd/reminders/tasks
Content-Type: application/json

{
  "Reminders": {
    "title": "New reminder",
    "description": "",
    "pGuid": "tasks",
    "etag": null,
    "order": null,
    "priority": 0,
    "recurrence": null,
    "alarms": [],
    "startDate": null,
    "startDateTz": null,
    "startDateIsAllDay": false,
    "completedDate": null,
    "dueDate": [20260125, 2026, 1, 25, 14, 30],
    "dueDateIsAllDay": false,
    "lastModifiedDate": null,
    "createdDate": null,
    "isFamily": null,
    "createdDateExtended": 1706123456789,
    "guid": "<new-uuid>"
  },
  "ClientState": {
    "Collections": [{"guid": "tasks", "ctag": "..."}]
  }
}
```

### Complete Reminder
```
POST https://<reminders_service_url>/rd/reminders/tasks
Content-Type: application/json

{
  "Reminders": {
    "guid": "<reminder-guid>",
    "pGuid": "<list-guid>",
    "completedDate": [20260125, 2026, 1, 25, 14, 30],
    ...other fields...
  },
  "ClientState": {...}
}
```

### Delete Reminder
```
POST https://<reminders_service_url>/rd/reminders/tasks
Content-Type: application/json

{
  "Reminders": {
    "guid": "<reminder-guid>",
    "pGuid": "<list-guid>",
    "deleted": true
  },
  "ClientState": {...}
}
```

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
