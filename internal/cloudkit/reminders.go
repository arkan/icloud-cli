package cloudkit

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
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

// encodeGzipBase64 encodes a string with gzip compression and base64
func encodeGzipBase64(s string) (string, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(s)); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// encodeTitleDocument encodes a title into the protobuf format used by CloudKit
// Format: \x12 + length (varint) + UTF-8 string
func encodeTitleDocument(title string) (string, error) {
	// Simple protobuf encoding: field 2, wire type 2 (length-delimited)
	// Full format includes some header bytes that Apple uses
	titleBytes := []byte(title)
	length := len(titleBytes)

	var data []byte
	// Add a minimal header (observed from real data)
	data = append(data, 0x0a, 0x00) // field 1, empty

	// Field 2: the title
	data = append(data, 0x12) // field 2, wire type 2
	if length < 128 {
		data = append(data, byte(length))
	} else {
		// Varint encoding for lengths >= 128
		data = append(data, byte(length&0x7f|0x80), byte(length>>7))
	}
	data = append(data, titleBytes...)

	return encodeGzipBase64(string(data))
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

// AddReminder creates a new reminder in CloudKit
func (s *RemindersService) AddReminder(title, notes, listID string, priority int, dueDate *time.Time) (*ReminderItem, error) {
	// Generate new UUID for the reminder
	reminderID := uuid.New().String()
	recordName := "Reminder/" + reminderID

	// Encode the title
	titleDoc, err := encodeTitleDocument(title)
	if err != nil {
		return nil, fmt.Errorf("encode title: %w", err)
	}

	now := time.Now().UnixMilli()

	// Build the record
	fields := map[string]FieldValue{
		"TitleDocument": {
			Value: titleDoc,
			Type:  "ENCRYPTED_BYTES",
		},
		"CreationDate": {
			Value: now,
			Type:  "TIMESTAMP",
		},
		"LastModifiedDate": {
			Value: now,
			Type:  "TIMESTAMP",
		},
		"Priority": {
			Value: priority,
			Type:  "NUMBER_INT64",
		},
		"Completed": {
			Value: 0,
			Type:  "NUMBER_INT64",
		},
	}

	// Add due date if provided
	if dueDate != nil {
		fields["DueDate"] = FieldValue{
			Value: dueDate.UnixMilli(),
			Type:  "TIMESTAMP",
		}
	}

	// Add notes if provided
	if notes != "" {
		notesDoc, err := encodeTitleDocument(notes)
		if err != nil {
			return nil, fmt.Errorf("encode notes: %w", err)
		}
		fields["NotesDocument"] = FieldValue{
			Value: notesDoc,
			Type:  "ENCRYPTED_BYTES",
		}
	}

	// Format list reference
	listRecordName := listID
	if !strings.HasPrefix(listID, "List/") {
		listRecordName = "List/" + listID
	}

	// Add list reference
	fields["List"] = FieldValue{
		Value: map[string]interface{}{
			"recordName": listRecordName,
			"action":     "VALIDATE",
			"zoneID": map[string]interface{}{
				"zoneName":        RemindersZone,
				"ownerRecordName": "_defaultOwner",
				"zoneType":        "REGULAR_CUSTOM_ZONE",
			},
		},
		Type: "REFERENCE",
	}

	record := Record{
		RecordName: recordName,
		RecordType: "Reminder",
		Fields:     fields,
		Parent: &RecordReference{
			RecordName: listRecordName,
		},
	}

	req := ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{
			{
				OperationType: "create",
				Record:        record,
			},
		},
	}

	resp, err := s.client.ModifyRecords(
		RemindersContainer, RemindersEnv, RemindersDB,
		req,
	)
	if err != nil {
		return nil, fmt.Errorf("create reminder: %w", err)
	}

	if len(resp.Records) == 0 {
		return nil, fmt.Errorf("no record returned from create")
	}

	// Return the created reminder
	item := s.parseReminder(resp.Records[0])
	return &item, nil
}

// CompleteReminder marks a reminder as done
func (s *RemindersService) CompleteReminder(reminderID string) error {
	// Format record name
	recordName := reminderID
	if !strings.HasPrefix(reminderID, "Reminder/") {
		recordName = "Reminder/" + reminderID
	}

	// First, lookup the existing record to get the change tag
	resp, err := s.client.LookupRecords(
		RemindersContainer, RemindersEnv, RemindersDB,
		s.zoneID, []string{recordName},
	)
	if err != nil {
		return fmt.Errorf("lookup reminder: %w", err)
	}

	if len(resp.Records) == 0 {
		return fmt.Errorf("reminder not found: %s", reminderID)
	}

	existingRecord := resp.Records[0]
	now := time.Now().UnixMilli()

	// Update the record
	record := Record{
		RecordName:      recordName,
		RecordType:      "Reminder",
		RecordChangeTag: existingRecord.RecordChangeTag,
		Fields: map[string]FieldValue{
			"Completed": {
				Value: 1,
				Type:  "NUMBER_INT64",
			},
			"CompletionDate": {
				Value: now,
				Type:  "TIMESTAMP",
			},
			"LastModifiedDate": {
				Value: now,
				Type:  "TIMESTAMP",
			},
		},
	}

	req := ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{
			{
				OperationType: "update",
				Record:        record,
			},
		},
	}

	_, err = s.client.ModifyRecords(
		RemindersContainer, RemindersEnv, RemindersDB,
		req,
	)
	if err != nil {
		return fmt.Errorf("update reminder: %w", err)
	}

	return nil
}

// DeleteReminder soft-deletes a reminder (moves to recently deleted)
func (s *RemindersService) DeleteReminder(reminderID string) error {
	// Format record name
	recordName := reminderID
	if !strings.HasPrefix(reminderID, "Reminder/") {
		recordName = "Reminder/" + reminderID
	}

	// Lookup the existing record to get the change tag
	resp, err := s.client.LookupRecords(
		RemindersContainer, RemindersEnv, RemindersDB,
		s.zoneID, []string{recordName},
	)
	if err != nil {
		return fmt.Errorf("lookup reminder: %w", err)
	}

	if len(resp.Records) == 0 {
		return fmt.Errorf("reminder not found: %s", reminderID)
	}

	existingRecord := resp.Records[0]
	now := time.Now().UnixMilli()

	// Soft delete by setting Deleted field to 1
	record := Record{
		RecordName:      recordName,
		RecordType:      "Reminder",
		RecordChangeTag: existingRecord.RecordChangeTag,
		Fields: map[string]FieldValue{
			"Deleted": {
				Value: 1,
				Type:  "NUMBER_INT64",
			},
			"LastModifiedDate": {
				Value: now,
				Type:  "TIMESTAMP",
			},
		},
	}

	req := ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{
			{
				OperationType: "update",
				Record:        record,
			},
		},
	}

	_, err = s.client.ModifyRecords(
		RemindersContainer, RemindersEnv, RemindersDB,
		req,
	)
	if err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}

	return nil
}
