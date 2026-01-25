# iCloud CLI

> **PROJECT ARCHIVED** - This project has been discontinued due to fundamental technical limitations with Apple's end-to-end encryption. See [Technical Limitations](#technical-limitations) below.

A command-line interface for accessing iCloud Reminders via CloudKit.

## What Works

- **Reading** reminders and lists via CloudKit API
- Authentication with 2FA support
- Session persistence

## What Doesn't Work (and never will)

- **Creating** reminders
- **Completing** reminders
- **Deleting** reminders

## Technical Limitations

### The Problem

Apple Reminders uses **end-to-end encryption (E2EE)** via CloudKit. Every reminder record requires:

- `chainProtectionInfo` - ASN.1 blob with EC P-256 signatures
- `chainParentKey` - Reference to parent list's encryption key
- `chainPrivateKey` - Record's private key, encrypted with parent's key

### Why It Can't Be Fixed

The encryption keys are stored in **iCloud Keychain**, which:

1. Is only accessible from trusted Apple devices
2. Uses Hardware Security Modules (HSM) with destroyed admin cards
3. Requires SRP protocol authentication (password never leaves device)
4. Needs 2FA verification on a trusted device

This is not a bug or missing feature - Apple explicitly designed the system to prevent third-party access to write operations.

### What We Tried

| Approach | Result |
|----------|--------|
| Create records without encryption | Records created but invisible to other devices |
| Reverse-engineer protobuf format | Solved (TitleDocument format) |
| Analyze ASN.1 encryption structure | Identified EC P-256, but keys inaccessible |
| Search for keys in CloudKit zones | Keys only exist in Keychain |

### Possible Workarounds (Not Implemented)

If you need to create reminders from Linux:

1. **Mac mini proxy** - SSH to a Mac running AppleScript
2. **iPhone Shortcuts webhook** - HTTP request triggers native Shortcut
3. **GitHub Actions macOS runner** - Legal macOS VM in the cloud

## Installation (Read-Only Use)

```bash
go install github.com/arkan/icloud-cli/cmd@latest
```

## Usage

```bash
# Login
icloud login user@example.com

# List reminder lists (works)
icloud reminders lists

# List reminders (works)
icloud reminders ls
icloud reminders ls "Shopping"

# These commands exist but WILL FAIL due to E2EE:
icloud reminders add "Test"    # Creates record, won't sync
icloud reminders done abc123   # May work locally, won't sync
icloud reminders rm abc123     # May work locally, won't sync
```

## Project Status

**ARCHIVED** - No further development planned.

The fundamental limitation (E2EE keys in Keychain) cannot be solved without:
- An Apple device in the loop, OR
- Apple providing a write API (unlikely)

## References

- [Apple Security Guide - CloudKit E2EE](https://support.apple.com/guide/security/cloudkit-end-to-end-encryption-sec3cac31735/web)
- [Apple Security Guide - iCloud Keychain Escrow](https://support.apple.com/guide/security/escrow-security-for-icloud-keychain-sec3e341e75d/web)

## License

MIT
