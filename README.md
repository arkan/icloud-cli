# iCloud CLI

A command-line interface for accessing iCloud data, starting with Reminders.

## Features

- 🔐 Apple ID authentication with 2FA support
- 📋 List all reminder lists
- ✅ View, add, complete, and delete reminders
- 💾 Persistent session (no re-login required)

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

### Authentication

```bash
# Login with Apple ID
icloud login user@example.com

# Check session status
icloud status

# Logout
icloud logout
```

### Reminders

```bash
# List all reminder lists
icloud reminders lists
# or: icloud r ll

# List all reminders
icloud reminders ls

# List reminders in a specific list
icloud reminders ls "My List"

# Add a reminder
icloud reminders add "Buy milk"

# Add with due date
icloud reminders add "Meeting" --due "tomorrow 14:00"

# Add to specific list with description
icloud reminders add "Call mom" -l "Family" -d "Don't forget birthday gift"

# Mark as done (using first 8 chars of ID)
icloud reminders done abc12345

# Delete a reminder
icloud reminders rm abc12345
```

### Due Date Formats

- `today` - Today at 6 PM
- `tomorrow` - Tomorrow at 9 AM
- `tomorrow 14:00` - Tomorrow at 2 PM
- `2024-01-20` - Specific date
- `2024-01-20 15:30` - Specific date and time
- `Jan 20` - January 20 (current year)

## Configuration

Session data is stored in `~/.icloud-cli/session.json`.

## API Documentation

See [docs/API.md](docs/API.md) for reverse-engineered iCloud API documentation.

## Project Structure

```
icloud-cli/
├── cmd/
│   └── main.go          # CLI entry point with Kong
├── internal/
│   ├── api/
│   │   └── client.go    # Base HTTP client
│   ├── auth/
│   │   └── auth.go      # Authentication flow
│   ├── config/
│   │   └── config.go    # Session persistence
│   └── reminders/
│       └── service.go   # Reminders service
├── docs/
│   └── API.md           # API documentation
└── README.md
```

## Future Plans

- [ ] Calendar service
- [ ] Notes service
- [ ] Find My service
- [ ] Drive service
- [ ] Photos service

## Credits

API reverse-engineered from [pyicloud](https://github.com/picklepete/pyicloud).

## License

MIT
