package cloudkit

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	RemindersContainer = "com.apple.reminders"
	RemindersEnv       = "production"
	RemindersDB        = "private"
	RemindersZone      = "Reminders"
)

// RemindersService provides access to Reminders via CloudKit
type RemindersService struct {
	client *Client
	zoneID ZoneID
}

// ReminderList represents a reminder list
type ReminderList struct {
	ID    string
	Title string
}

// ReminderItem represents a single reminder
type ReminderItem struct {
	ID           string
	ListID       string
	Title        string
	Notes        string
	Priority     int
	Completed    bool
	DueDate      *time.Time
	CreatedDate  time.Time
	ModifiedDate time.Time
}

// NewRemindersService creates a new CloudKit-based reminders service
func NewRemindersService(client *Client) *RemindersService {
	return &RemindersService{
		client: client,
		zoneID: ZoneID{ZoneName: RemindersZone},
	}
}

// GetReminders fetches all reminders from CloudKit
func (s *RemindersService) GetReminders(includeCompleted bool) ([]ReminderItem, error) {
	var allRecords []Record
	var syncToken string

	// Fetch all records (may need multiple requests due to pagination)
	for {
		resp, err := s.client.FetchChanges(
			RemindersContainer, RemindersEnv, RemindersDB,
			s.zoneID, syncToken,
		)
		if err != nil {
			return nil, fmt.Errorf("fetch reminders: %w", err)
		}

		allRecords = append(allRecords, resp.Records...)

		if !resp.MoreComing {
			break
		}
		syncToken = resp.SyncToken
	}

	// Convert to ReminderItems
	var reminders []ReminderItem
	for _, r := range allRecords {
		if r.RecordType != "Reminder" || r.Deleted {
			continue
		}

		// Check soft-delete flag in Fields
		if f, ok := r.Fields["Deleted"]; ok {
			if v, ok := f.Value.(float64); ok && v == 1 {
				continue
			}
		}

		item := s.parseReminder(r)

		// Filter completed if requested
		if !includeCompleted && item.Completed {
			continue
		}

		reminders = append(reminders, item)
	}

	return reminders, nil
}

// GetLists fetches all reminder lists from CloudKit
func (s *RemindersService) GetLists() ([]ReminderList, error) {
	var allRecords []Record
	var syncToken string

	// Fetch all records
	for {
		resp, err := s.client.FetchChanges(
			RemindersContainer, RemindersEnv, RemindersDB,
			s.zoneID, syncToken,
		)
		if err != nil {
			return nil, fmt.Errorf("fetch lists: %w", err)
		}

		allRecords = append(allRecords, resp.Records...)

		if !resp.MoreComing {
			break
		}
		syncToken = resp.SyncToken
	}

	// Find unique list IDs from Reminder records
	listIDs := make(map[string]bool)
	for _, r := range allRecords {
		if r.RecordType == "Reminder" && r.Parent != nil {
			listIDs[r.Parent.RecordName] = true
		}
	}

	// Collect list record names for lookup
	var listRecordNames []string
	for listID := range listIDs {
		listRecordNames = append(listRecordNames, listID)
	}

	// Lookup list records to get their names
	listRecords := make(map[string]Record)
	if len(listRecordNames) > 0 {
		resp, err := s.client.LookupRecords(
			RemindersContainer, RemindersEnv, RemindersDB,
			s.zoneID, listRecordNames,
		)
		if err == nil {
			for _, r := range resp.Records {
				listRecords[r.RecordName] = r
			}
		}
	}

	// Build list results
	var lists []ReminderList
	for listID := range listIDs {
		parts := strings.Split(listID, "/")
		uuid := listID
		if len(parts) == 2 {
			uuid = parts[1]
		}

		title := ""
		if r, ok := listRecords[listID]; ok {
			// Extract Name field directly (it's stored in plain text)
			if f, ok := r.Fields["Name"]; ok {
				if v, ok := f.Value.(string); ok {
					title = v
				}
			}
		}
		if title == "" {
			title = "List " + uuid[:8]
		}

		lists = append(lists, ReminderList{ID: uuid, Title: title})
	}

	return lists, nil
}

// parseReminder converts a CloudKit record to a ReminderItem
func (s *RemindersService) parseReminder(r Record) ReminderItem {
	item := ReminderItem{
		ID: r.RecordName,
	}

	// Extract list ID from parent reference
	if r.Parent != nil {
		parts := strings.Split(r.Parent.RecordName, "/")
		if len(parts) == 2 {
			item.ListID = parts[1]
		} else {
			item.ListID = r.Parent.RecordName
		}
	}

	// Decode title
	item.Title = s.decodeTitle(r)

	// Get other fields
	if f, ok := r.Fields["Completed"]; ok {
		if v, ok := f.Value.(float64); ok {
			item.Completed = v == 1
		}
	}

	if f, ok := r.Fields["Priority"]; ok {
		if v, ok := f.Value.(float64); ok {
			item.Priority = int(v)
		}
	}

	if f, ok := r.Fields["CreationDate"]; ok {
		if v, ok := f.Value.(float64); ok {
			item.CreatedDate = time.UnixMilli(int64(v))
		}
	}

	if f, ok := r.Fields["LastModifiedDate"]; ok {
		if v, ok := f.Value.(float64); ok {
			item.ModifiedDate = time.UnixMilli(int64(v))
		}
	}

	if f, ok := r.Fields["DueDate"]; ok {
		if v, ok := f.Value.(float64); ok {
			t := time.UnixMilli(int64(v))
			item.DueDate = &t
		}
	}

	// Notes field (may also be encrypted)
	if f, ok := r.Fields["NotesDocument"]; ok {
		if v, ok := f.Value.(string); ok {
			decoded := decodeGzipBase64(v)
			if decoded != "" {
				item.Notes = extractReadableText(decoded)
			}
		}
	}

	return item
}

// decodeTitle extracts the title from a record's TitleDocument field
func (s *RemindersService) decodeTitle(r Record) string {
	if f, ok := r.Fields["TitleDocument"]; ok {
		if v, ok := f.Value.(string); ok {
			decoded := decodeGzipBase64(v)
			if decoded != "" {
				// The decoded content may still have some binary prefix
				// Try to extract readable text
				return extractReadableText(decoded)
			}
		}
	}

	// Fallback to Title field if available
	if f, ok := r.Fields["Title"]; ok {
		if v, ok := f.Value.(string); ok {
			return v
		}
	}

	return ""
}

// decodeGzipBase64 decodes a base64-encoded gzip-compressed string
func decodeGzipBase64(s string) string {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}

	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	defer reader.Close()

	decompressed, err := io.ReadAll(reader)
	if err != nil {
		return ""
	}

	return string(decompressed)
}

// extractReadableText tries to extract readable text from protobuf-encoded data
func extractReadableText(s string) string {
	// The decoded content is a protobuf where field 2 contains the title
	// Format: \x12 + length byte(s) + UTF-8 string
	data := []byte(s)

	// Look for field 2 marker (\x12 = field 2, wire type 2 = length-delimited)
	for i := 0; i < len(data)-2; i++ {
		if data[i] == 0x12 {
			// Next byte(s) are the length (varint)
			length := int(data[i+1])
			start := i + 2

			// Handle multi-byte varint (length > 127)
			if length > 127 {
				if i+2 < len(data) {
					length = int(data[i+1]&0x7f) | (int(data[i+2]) << 7)
					start = i + 3
				}
			}

			if start+length <= len(data) && length > 0 && length < 1000 {
				text := string(data[start : start+length])
				// Verify it's mostly printable
				printable := 0
				for _, r := range text {
					if r >= 32 && r < 127 || r > 127 {
						printable++
					}
				}
				if printable > len(text)*80/100 {
					return strings.TrimSpace(text)
				}
			}
		}
	}

	// Fallback: find longest printable sequence
	var result strings.Builder
	var current strings.Builder

	for _, r := range s {
		if r >= 32 && r < 127 || r > 127 {
			current.WriteRune(r)
		} else {
			if current.Len() > result.Len() {
				result.Reset()
				result.WriteString(current.String())
			}
			current.Reset()
		}
	}

	if current.Len() > result.Len() {
		return current.String()
	}

	return strings.TrimSpace(result.String())
}
