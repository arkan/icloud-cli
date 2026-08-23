package cloudkit

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

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
	client    *Client
	zoneID    ZoneID
	records   map[string]Record
	syncToken string
	synced    bool
	cachePath string
	cacheRead bool
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
	ParentID     string
	Title        string
	Notes        string
	Priority     int
	Flagged      bool
	Completed    bool
	DueDate      *time.Time
	CreatedDate  time.Time
	ModifiedDate time.Time
}

// ReminderChanges contains the fields to update. Nil pointers are unchanged.
type ReminderChanges struct {
	Title    *string
	Notes    *string
	Priority *int
	DueDate  *time.Time
	Flagged  *bool
}

// NewRemindersService creates a new CloudKit-based reminders service
func NewRemindersService(client *Client) *RemindersService {
	cachePath := ""
	if home, err := os.UserHomeDir(); err == nil {
		cachePath = filepath.Join(home, ".icloud-cli", "cloudkit-cache.json")
	}
	return newRemindersService(client, cachePath)
}

func newRemindersService(client *Client, cachePath string) *RemindersService {
	return &RemindersService{
		client:    client,
		zoneID:    ZoneID{ZoneName: RemindersZone},
		records:   make(map[string]Record),
		cachePath: cachePath,
	}
}

type remindersCache struct {
	OwnerRecordName string            `json:"owner_record_name"`
	SyncToken       string            `json:"sync_token"`
	Records         map[string]Record `json:"records"`
}

func (s *RemindersService) loadCache() {
	if s.cacheRead {
		return
	}
	s.cacheRead = true
	if s.cachePath == "" {
		return
	}
	data, err := os.ReadFile(s.cachePath)
	if err != nil {
		return
	}
	var cached remindersCache
	if json.Unmarshal(data, &cached) != nil || cached.OwnerRecordName != s.zoneID.OwnerRecordName {
		return
	}
	if cached.Records != nil {
		s.records = cached.Records
	}
	s.syncToken = cached.SyncToken
}

func (s *RemindersService) saveCache() error {
	if s.cachePath == "" || s.zoneID.OwnerRecordName == "" {
		return nil
	}
	directory := filepath.Dir(s.cachePath)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".cloudkit-cache-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return err
	}
	cache := remindersCache{
		OwnerRecordName: s.zoneID.OwnerRecordName,
		SyncToken:       s.syncToken,
		Records:         s.records,
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(cache); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, s.cachePath)
}

func (s *RemindersService) persistCache() {
	if err := s.saveCache(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save CloudKit cache: %v\n", err)
	}
}

// ensureZone resolves the account-specific owner for the Reminders zone.
// CloudKit custom zones must be addressed with the owner returned by zones/list;
// guessed values such as _defaultOwner are not interchangeable with it.
func (s *RemindersService) ensureZone() error {
	if s.zoneID.OwnerRecordName != "" {
		s.loadCache()
		return nil
	}
	zones, err := s.client.ListZones(RemindersContainer, RemindersEnv, RemindersDB)
	if err != nil {
		return fmt.Errorf("resolve reminders zone: %w", err)
	}
	for _, zone := range zones.Zones {
		if zone.ZoneID.ZoneName == RemindersZone && zone.ZoneID.OwnerRecordName != "" {
			s.zoneID = zone.ZoneID
			s.loadCache()
			return nil
		}
	}
	return fmt.Errorf("reminders zone not found")
}

// Sync refreshes the in-memory record cache using CloudKit delta tokens.
// A forced sync discards the existing token and rebuilds the cache.
func (s *RemindersService) Sync(force bool) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	if force {
		s.records = make(map[string]Record)
		s.syncToken = ""
		s.synced = false
	}
	if s.synced {
		return nil
	}

	token := s.syncToken
	for {
		resp, err := s.client.FetchChanges(
			RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, token,
		)
		if err != nil {
			return fmt.Errorf("fetch reminders changes: %w", err)
		}
		for _, record := range resp.Records {
			if record.Deleted || isSoftDeleted(record) {
				delete(s.records, record.RecordName)
				continue
			}
			s.records[record.RecordName] = record
		}
		previousToken := token
		if resp.SyncToken != "" {
			token = resp.SyncToken
			s.syncToken = resp.SyncToken
		}
		if resp.MoreComing && (resp.SyncToken == "" || token == previousToken) {
			return fmt.Errorf("fetch reminders changes: pagination did not advance sync token")
		}
		if !resp.MoreComing {
			break
		}
	}
	s.synced = true
	s.persistCache()
	return nil
}

func isSoftDeleted(record Record) bool {
	field, ok := record.Fields["Deleted"]
	return ok && fieldBool(field.Value)
}

// GetReminders fetches all reminders from CloudKit
func (s *RemindersService) GetReminders(includeCompleted bool) ([]ReminderItem, error) {
	if err := s.Sync(false); err != nil {
		return nil, err
	}

	// Convert to ReminderItems
	var reminders []ReminderItem
	for _, r := range s.records {
		if r.RecordType != "Reminder" || r.Deleted {
			continue
		}

		if isSoftDeleted(r) {
			continue
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
	if err := s.Sync(false); err != nil {
		return nil, err
	}

	listRecords := make(map[string]Record)
	listIDs := make(map[string]bool)
	for _, r := range s.records {
		switch r.RecordType {
		case "ReminderList", "List":
			listIDs[r.RecordName] = true
			listRecords[r.RecordName] = r
		case "Reminder":
			if listID := recordListID(r); listID != "" {
				listIDs[listID] = true
			}
		}
	}

	// Collect list record names for lookup
	var listRecordNames []string
	for listID := range listIDs {
		if _, exists := listRecords[listID]; !exists {
			listRecordNames = append(listRecordNames, listID)
		}
	}

	// Lookup list records to get their names
	if len(listRecordNames) > 0 {
		resp, err := s.client.LookupRecords(
			RemindersContainer, RemindersEnv, RemindersDB,
			s.zoneID, listRecordNames,
		)
		if err != nil {
			return nil, fmt.Errorf("lookup reminder lists: %w", err)
		}
		for _, r := range resp.Records {
			if err := recordError(r); err != nil {
				return nil, fmt.Errorf("lookup reminder list %s: %w", r.RecordName, err)
			}
			listRecords[r.RecordName] = r
		}
	}

	// Build list results
	var lists []ReminderList
	for listID := range listIDs {
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
			title = "List " + listID[:min(8, len(listID))]
		}

		lists = append(lists, ReminderList{ID: listID, Title: title})
	}

	return lists, nil
}

// parseReminder converts a CloudKit record to a ReminderItem
func (s *RemindersService) parseReminder(r Record) ReminderItem {
	item := ReminderItem{
		ID: r.RecordName,
	}

	item.ListID = recordListID(r)
	if field, ok := r.Fields["ParentReminder"]; ok {
		item.ParentID = recordListID(Record{Fields: map[string]FieldValue{"List": field}})
	}

	// Decode title
	item.Title = s.decodeTitle(r)

	// Get other fields
	if f, ok := r.Fields["Completed"]; ok {
		item.Completed = fieldBool(f.Value)
	}

	if f, ok := r.Fields["Priority"]; ok {
		if v, ok := f.Value.(float64); ok {
			item.Priority = int(v)
		}
	}
	if f, ok := r.Fields["Flagged"]; ok {
		item.Flagged = fieldBool(f.Value)
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

func fieldBool(value interface{}) bool {
	switch value := value.(type) {
	case bool:
		return value
	case float64:
		return value != 0
	case int:
		return value != 0
	case int64:
		return value != 0
	default:
		return false
	}
}

func recordListID(record Record) string {
	if field, ok := record.Fields["List"]; ok {
		switch value := field.Value.(type) {
		case RecordReference:
			return value.RecordName
		case *RecordReference:
			if value != nil {
				return value.RecordName
			}
		case map[string]interface{}:
			if name, ok := value["recordName"].(string); ok {
				return name
			}
		}
	}
	if record.Parent != nil {
		return record.Parent.RecordName
	}
	return ""
}

func mergeRecord(base, update Record) Record {
	if update.RecordName != "" {
		base.RecordName = update.RecordName
	}
	if update.RecordType != "" {
		base.RecordType = update.RecordType
	}
	if update.RecordChangeTag != "" {
		base.RecordChangeTag = update.RecordChangeTag
	}
	if base.Fields == nil {
		base.Fields = make(map[string]FieldValue)
	}
	for name, value := range update.Fields {
		base.Fields[name] = value
	}
	if update.Parent != nil {
		base.Parent = update.Parent
	}
	base.Deleted = update.Deleted
	return base
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

func encodeVarint(value uint64) []byte {
	var encoded []byte
	for value > 127 {
		encoded = append(encoded, byte(value&0x7f)|0x80)
		value >>= 7
	}
	return append(encoded, byte(value))
}

func encodeField(number, wireType int, value interface{}) []byte {
	tag := byte(number<<3 | wireType)
	switch wireType {
	case 0:
		return append([]byte{tag}, encodeVarint(value.(uint64))...)
	case 2:
		data := value.([]byte)
		result := append([]byte{tag}, encodeVarint(uint64(len(data)))...)
		return append(result, data...)
	default:
		panic("unsupported protobuf wire type")
	}
}

func encodePosition(replica uint64, offset int64) []byte {
	result := encodeField(1, 0, replica)
	if offset == -1 {
		return append(result, encodeField(2, 0, uint64(0xffffffff))...)
	}
	return append(result, encodeField(2, 0, uint64(offset))...)
}

// encodeTitleDocument encodes Apple's Reminders CRDT document format.
// Character counts deliberately use Unicode code points rather than UTF-8 bytes.
func encodeTitleDocument(title string) (string, error) {
	titleBytes := []byte(title)
	charLength := uint64(utf8.RuneCountInString(title))

	op1 := encodeField(1, 2, encodePosition(0, 0))
	op1 = append(op1, encodeField(2, 0, uint64(0))...)
	op1 = append(op1, encodeField(3, 2, encodePosition(0, 0))...)
	op1 = append(op1, encodeField(5, 0, uint64(1))...)

	op2 := encodeField(1, 2, encodePosition(1, 0))
	op2 = append(op2, encodeField(2, 0, charLength)...)
	op2 = append(op2, encodeField(3, 2, encodePosition(1, 0))...)
	op2 = append(op2, encodeField(5, 0, uint64(2))...)

	op3 := encodeField(1, 2, encodePosition(0, -1))
	op3 = append(op3, encodeField(2, 0, uint64(0))...)
	op3 = append(op3, encodeField(3, 2, encodePosition(0, -1))...)

	documentUUID, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("generate document UUID: %w", err)
	}
	clock := encodeField(1, 0, charLength)
	replica := encodeField(1, 0, uint64(1))
	uuidEntry := encodeField(1, 2, documentUUID[:])
	uuidEntry = append(uuidEntry, encodeField(2, 2, clock)...)
	uuidEntry = append(uuidEntry, encodeField(2, 2, replica)...)
	metadata := encodeField(1, 2, uuidEntry)

	note := encodeField(2, 2, titleBytes)
	note = append(note, encodeField(3, 2, op1)...)
	note = append(note, encodeField(3, 2, op2)...)
	note = append(note, encodeField(3, 2, op3)...)
	note = append(note, encodeField(4, 2, metadata)...)
	note = append(note, encodeField(5, 2, encodeField(1, 0, charLength))...)

	document := encodeField(1, 0, uint64(0))
	document = append(document, encodeField(2, 0, uint64(0))...)
	document = append(document, encodeField(3, 2, note)...)
	outer := encodeField(1, 0, uint64(0))
	outer = append(outer, encodeField(2, 2, document)...)

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(outer); err != nil {
		return "", fmt.Errorf("compress document: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close document compressor: %w", err)
	}
	return base64.StdEncoding.EncodeToString(compressed.Bytes()), nil
}

// extractReadableText tries to extract readable text from protobuf-encoded data
func extractReadableText(s string) string {
	data := []byte(s)
	// Reminders CRDT documents nest the user text as outer.2 -> document.3 -> note.2.
	if outer, ok := protobufBytesField(data, 2); ok {
		if document, ok := protobufBytesField(outer, 3); ok {
			if text, ok := protobufBytesField(document, 2); ok && utf8.Valid(text) {
				return strings.TrimSpace(string(text))
			}
		}
	}

	// Keep a permissive fallback for older TitleDocument variants.
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

func protobufBytesField(message []byte, wanted int) ([]byte, bool) {
	for offset := 0; offset < len(message); {
		tag, bytesRead := decodeVarint(message[offset:])
		if bytesRead == 0 {
			return nil, false
		}
		offset += bytesRead
		fieldNumber := int(tag >> 3)
		wireType := int(tag & 7)
		switch wireType {
		case 0:
			_, bytesRead = decodeVarint(message[offset:])
			if bytesRead == 0 {
				return nil, false
			}
			offset += bytesRead
		case 2:
			length, lengthBytes := decodeVarint(message[offset:])
			if lengthBytes == 0 {
				return nil, false
			}
			offset += lengthBytes
			end := offset + int(length)
			if end < offset || end > len(message) {
				return nil, false
			}
			if fieldNumber == wanted {
				return message[offset:end], true
			}
			offset = end
		default:
			return nil, false
		}
	}
	return nil, false
}

func decodeVarint(data []byte) (uint64, int) {
	var value uint64
	for i, current := range data {
		if i == 10 || i == 9 && current > 1 {
			return 0, 0
		}
		value |= uint64(current&0x7f) << (7 * i)
		if current < 0x80 {
			return value, i + 1
		}
	}
	return 0, 0
}

// AddReminder creates a new reminder in CloudKit
func (s *RemindersService) AddReminder(title, notes, listID string, priority int, dueDate *time.Time) (*ReminderItem, error) {
	return s.AddReminderWithParent(title, notes, listID, priority, dueDate, "")
}

// AddReminderWithParent creates a reminder and optionally links it as a subtask.
func (s *RemindersService) AddReminderWithParent(title, notes, listID string, priority int, dueDate *time.Time, parentID string) (*ReminderItem, error) {
	if err := s.ensureZone(); err != nil {
		return nil, err
	}
	if parentID != "" {
		parent, err := s.lookupReminder(parentID)
		if err != nil {
			return nil, fmt.Errorf("lookup parent reminder: %w", err)
		}
		parentListID := recordListID(parent)
		if listID != "" && parentListID != "" && listID != parentListID {
			return nil, fmt.Errorf("parent reminder belongs to list %s, not %s", parentListID, listID)
		}
		if listID == "" {
			listID = parentListID
		}
	}
	recordName := "Reminder/" + strings.ToUpper(uuid.New().String())

	// Encode the title
	titleDoc, err := encodeTitleDocument(title)
	if err != nil {
		return nil, fmt.Errorf("encode title: %w", err)
	}

	fields := map[string]FieldValue{
		"TitleDocument": {Value: titleDoc},
		"Completed":     {Value: 0},
	}

	// Add due date if provided
	if dueDate != nil {
		fields["DueDate"] = FieldValue{Value: dueDate.UnixMilli()}
	}

	// Add notes if provided
	if notes != "" {
		notesDoc, err := encodeTitleDocument(notes)
		if err != nil {
			return nil, fmt.Errorf("encode notes: %w", err)
		}
		fields["NotesDocument"] = FieldValue{Value: notesDoc}
	}
	if priority != 0 {
		fields["Priority"] = FieldValue{Value: priority}
	}
	if listID != "" {
		fields["List"] = FieldValue{Value: RecordReference{RecordName: listID, Action: "NONE"}}
	}
	if parentID != "" {
		fields["ParentReminder"] = FieldValue{Value: RecordReference{RecordName: parentID, Action: "NONE"}}
	}

	record := Record{
		RecordName: recordName,
		RecordType: "Reminder",
		Fields:     fields,
	}

	req := ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{
			{
				OperationType: OperationCreate,
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
	if err := recordError(resp.Records[0]); err != nil {
		return nil, fmt.Errorf("create reminder: %w", err)
	}
	s.records[recordName] = mergeRecord(record, resp.Records[0])
	s.persistCache()

	// Return the created reminder
	item := s.parseReminder(resp.Records[0])
	return &item, nil
}

// UpdateReminder applies a partial update using CloudKit optimistic locking.
func (s *RemindersService) UpdateReminder(reminderID string, changes ReminderChanges) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	fields := make(map[string]FieldValue)
	if changes.Title != nil {
		encoded, err := encodeTitleDocument(*changes.Title)
		if err != nil {
			return fmt.Errorf("encode title: %w", err)
		}
		fields["TitleDocument"] = FieldValue{Value: encoded}
	}
	if changes.Notes != nil {
		encoded, err := encodeTitleDocument(*changes.Notes)
		if err != nil {
			return fmt.Errorf("encode notes: %w", err)
		}
		fields["NotesDocument"] = FieldValue{Value: encoded}
	}
	if changes.Priority != nil {
		fields["Priority"] = FieldValue{Value: *changes.Priority}
	}
	if changes.DueDate != nil {
		fields["DueDate"] = FieldValue{Value: changes.DueDate.UnixMilli()}
	}
	if changes.Flagged != nil {
		value := int64(0)
		if *changes.Flagged {
			value = 1
		}
		fields["Flagged"] = FieldValue{Value: value, Type: "NUMBER_INT64"}
		if err := addResolutionTokenUpdate(existing, fields, "flagged"); err != nil {
			return fmt.Errorf("update flag resolution token: %w", err)
		}
	}
	if len(fields) == 0 {
		return fmt.Errorf("no reminder changes specified")
	}

	request := ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{{
			OperationType: OperationUpdate,
			Record: Record{
				RecordName:      reminderID,
				RecordType:      "Reminder",
				RecordChangeTag: existing.RecordChangeTag,
				Fields:          fields,
			},
		}},
	}
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, request)
	if err != nil {
		return fmt.Errorf("update reminder: %w", err)
	}
	if len(response.Records) == 0 {
		return fmt.Errorf("update reminder: no record returned")
	}
	if err := recordError(response.Records[0]); err != nil {
		return fmt.Errorf("update reminder: %w", err)
	}
	s.records[reminderID] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

func addResolutionTokenUpdate(record Record, fields map[string]FieldValue, key string) error {
	tokenField, ok := record.Fields["ResolutionTokenMap"]
	if !ok {
		return fmt.Errorf("record has no ResolutionTokenMap")
	}
	encoded, ok := tokenField.Value.(string)
	if !ok || encoded == "" {
		return fmt.Errorf("record has an invalid ResolutionTokenMap")
	}
	var envelope struct {
		Map map[string]map[string]interface{} `json:"map"`
	}
	if err := json.Unmarshal([]byte(encoded), &envelope); err != nil {
		return err
	}
	now := time.Now()
	coreDataTime := float64(now.UnixMilli())/1000 - 978307200
	for _, tokenKey := range []string{key, "lastModifiedDate"} {
		token, ok := envelope.Map[tokenKey]
		if !ok {
			return fmt.Errorf("resolution token %q is missing", tokenKey)
		}
		counter, ok := token["counter"].(float64)
		if !ok {
			return fmt.Errorf("resolution token %q has no numeric counter", tokenKey)
		}
		token["counter"] = counter + 1
		token["modificationTime"] = coreDataTime
	}
	updated, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	fields["ResolutionTokenMap"] = FieldValue{Value: string(updated)}
	fields["LastModifiedDate"] = FieldValue{Value: now.UnixMilli(), Type: "TIMESTAMP"}
	return nil
}

// UpdateTags atomically updates HashtagIDs and its linked Hashtag records.
func (s *RemindersService) UpdateTags(reminderID string, add, remove []string) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}

	type hashtag struct {
		id     string
		name   string
		record Record
	}
	currentIDs := fieldStringList(existing.Fields["HashtagIDs"].Value)
	current := make([]hashtag, 0, len(currentIDs))
	for _, id := range currentIDs {
		response, err := s.client.LookupRecords(
			RemindersContainer, RemindersEnv, RemindersDB, s.zoneID,
			[]string{"Hashtag/" + id},
		)
		if err != nil {
			return fmt.Errorf("lookup tag %s: %w", id, err)
		}
		if len(response.Records) == 0 {
			return fmt.Errorf("lookup tag %s: no record returned", id)
		}
		if err := recordError(response.Records[0]); err != nil {
			return fmt.Errorf("lookup tag %s: %w", id, err)
		}
		record := response.Records[0]
		current = append(current, hashtag{id: id, name: decodeCloudKitText(record.Fields["Name"]), record: record})
	}

	removeSet := normalizedTagSet(remove)
	nameSet := make(map[string]bool)
	ids := make([]string, 0, len(currentIDs)+len(add))
	childOperations := make([]RecordOperation, 0, len(add)+len(remove))
	for _, tag := range current {
		if removeSet[strings.ToLower(tag.name)] {
			childOperations = append(childOperations, RecordOperation{
				OperationType: OperationDelete,
				Record: Record{
					RecordName:      tag.record.RecordName,
					RecordType:      "Hashtag",
					RecordChangeTag: tag.record.RecordChangeTag,
				},
			})
			delete(removeSet, strings.ToLower(tag.name))
			continue
		}
		ids = append(ids, tag.id)
		nameSet[strings.ToLower(tag.name)] = true
	}
	if len(removeSet) > 0 {
		for name := range removeSet {
			return fmt.Errorf("tag not found: %s", name)
		}
	}

	now := time.Now()
	for _, rawName := range add {
		name := normalizeTagName(rawName)
		if name == "" {
			return fmt.Errorf("tag name cannot be empty")
		}
		if nameSet[strings.ToLower(name)] {
			continue
		}
		id := strings.ToUpper(uuid.New().String())
		ids = append(ids, id)
		nameSet[strings.ToLower(name)] = true
		childOperations = append(childOperations, RecordOperation{
			OperationType: OperationCreate,
			Record: Record{
				RecordName: "Hashtag/" + id,
				RecordType: "Hashtag",
				Parent:     &RecordReference{RecordName: reminderID},
				Fields: map[string]FieldValue{
					"Name": {
						Value:       name,
						Type:        "STRING",
						IsEncrypted: true,
					},
					"Deleted":      {Value: int64(0), Type: "NUMBER_INT64"},
					"Reminder":     {Value: RecordReference{RecordName: reminderID, Action: "VALIDATE"}},
					"CreationDate": {Value: now.UnixMilli(), Type: "TIMESTAMP"},
				},
			},
		})
	}
	if len(childOperations) == 0 {
		return nil
	}

	tokenMap, err := newResolutionTokenMap("hashtagIDs", "lastModifiedDate")
	if err != nil {
		return err
	}
	reminderOperation := RecordOperation{
		OperationType: OperationUpdate,
		Record: Record{
			RecordName:      reminderID,
			RecordType:      "Reminder",
			RecordChangeTag: existing.RecordChangeTag,
			Fields: map[string]FieldValue{
				"HashtagIDs":         {Value: ids, Type: "STRING_LIST"},
				"ResolutionTokenMap": {Value: tokenMap, Type: "STRING"},
				"LastModifiedDate":   {Value: now.UnixMilli(), Type: "TIMESTAMP"},
			},
		},
	}
	operations := append([]RecordOperation{reminderOperation}, childOperations...)
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, ModifyRequest{
		ZoneID: s.zoneID, Operations: operations, Atomic: true,
	})
	if err != nil {
		return fmt.Errorf("update tags: %w", err)
	}
	if len(response.Records) != len(operations) {
		return fmt.Errorf("update tags: got %d records, want %d", len(response.Records), len(operations))
	}
	for _, record := range response.Records {
		if err := recordError(record); err != nil {
			return fmt.Errorf("update tags: %w", err)
		}
	}
	s.records[reminderID] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

func normalizeTagName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "#")
}

func normalizedTagSet(names []string) map[string]bool {
	result := make(map[string]bool)
	for _, name := range names {
		if normalized := normalizeTagName(name); normalized != "" {
			result[strings.ToLower(normalized)] = true
		}
	}
	return result
}

func fieldStringList(value interface{}) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []interface{}:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func decodeCloudKitText(field FieldValue) string {
	value, _ := field.Value.(string)
	if field.Type != "ENCRYPTED_BYTES" {
		return value
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return ""
	}
	return string(decoded)
}

func newResolutionTokenMap(keys ...string) (string, error) {
	appleTime := float64(time.Now().UnixMilli())/1000 - 978307200
	tokens := make(map[string]map[string]interface{}, len(keys))
	for _, key := range keys {
		tokens[key] = map[string]interface{}{
			"counter":          1,
			"modificationTime": appleTime,
			"replicaID":        strings.ToUpper(uuid.New().String()),
		}
	}
	encoded, err := json.Marshal(map[string]interface{}{"map": tokens})
	return string(encoded), err
}

// CompleteReminder submits the completion fields used by Reminders. CloudKit
// may reconcile this undocumented mutation back to the previous state.
func (s *RemindersService) CompleteReminder(reminderID string) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	request := ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{{
			OperationType: OperationUpdate,
			Record: Record{
				RecordName:      reminderID,
				RecordType:      "Reminder",
				RecordChangeTag: existing.RecordChangeTag,
				Fields: map[string]FieldValue{
					"Completed":      {Value: int64(1)},
					"CompletionDate": {Value: time.Now().UnixMilli()},
				},
			},
		}},
	}
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, request)
	if err != nil {
		return fmt.Errorf("complete reminder: %w", err)
	}
	if len(response.Records) == 0 {
		return fmt.Errorf("complete reminder: no record returned")
	}
	if err := recordError(response.Records[0]); err != nil {
		return fmt.Errorf("complete reminder: %w", err)
	}
	s.records[reminderID] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

func (s *RemindersService) lookupReminder(reminderID string) (Record, error) {
	response, err := s.client.LookupRecords(
		RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, []string{reminderID},
	)
	if err != nil {
		return Record{}, fmt.Errorf("lookup reminder: %w", err)
	}
	if len(response.Records) == 0 {
		return Record{}, fmt.Errorf("reminder not found: %s", reminderID)
	}
	if err := recordError(response.Records[0]); err != nil {
		return Record{}, fmt.Errorf("lookup reminder: %w", err)
	}
	if response.Records[0].RecordChangeTag == "" {
		return Record{}, fmt.Errorf("reminder %s has no change tag", reminderID)
	}
	return response.Records[0], nil
}

// DeleteReminder deletes a reminder through CloudKit's native delete operation.
func (s *RemindersService) DeleteReminder(reminderID string) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	existingRecord, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	record := Record{
		RecordName:      reminderID,
		RecordChangeTag: existingRecord.RecordChangeTag,
	}

	req := ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{
			{
				OperationType: OperationDelete,
				Record:        record,
			},
		},
	}

	modified, err := s.client.ModifyRecords(
		RemindersContainer, RemindersEnv, RemindersDB,
		req,
	)
	if err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}
	if len(modified.Records) == 0 {
		return fmt.Errorf("delete reminder: no record returned")
	}
	if err := recordError(modified.Records[0]); err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}
	delete(s.records, reminderID)
	s.persistCache()

	return nil
}
