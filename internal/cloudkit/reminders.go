package cloudkit

import (
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
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
	replicaID uuid.UUID
}

// ReminderList represents a reminder list
type ReminderList struct {
	ID    string
	Title string
}

// ReminderSharee is a participant who can be assigned reminders in a shared list.
type ReminderSharee struct {
	ParticipantID  string
	UserRecordName string
	DisplayName    string
	Email          string
	Phone          string
	CurrentUser    bool
}

// ReminderItem represents a single reminder
type ReminderItem struct {
	ID             string
	ListID         string
	ParentID       string
	Title          string
	Notes          string
	Priority       int
	Flagged        bool
	Completed      bool
	CompletionDate *time.Time
	DueDate        *time.Time
	CreatedDate    time.Time
	ModifiedDate   time.Time
}

// ReminderChanges contains the fields to update. Nil pointers are unchanged.
type ReminderChanges struct {
	Title    *string
	Notes    *string
	Priority *int
	Flagged  *bool
}

// ReminderProperties contains native properties stored in linked CloudKit records.
type ReminderProperties struct {
	URL           string
	Tags          []string
	Assignee      string
	TimeZone      string
	AllDay        bool
	Location      *LocationAlarm
	Recurrence    *RecurrenceRule
	EarlyReminder *EarlyReminder
	Urgent        *bool
}

// DueDateChange describes a native due date. A nil change clears the due date.
type DueDateChange struct {
	Date     time.Time
	AllDay   bool
	TimeZone string
}

// RecurrenceRule describes the simple recurrence forms exposed by the CLI.
type RecurrenceRule struct {
	Frequency int
	Interval  int
	EndDate   *time.Time
}

// EarlyReminder describes an alert offset before the reminder's due date.
type EarlyReminder struct {
	Unit  int
	Count int
}

// LocationAlarm describes a geofence trigger for a reminder.
type LocationAlarm struct {
	Title     string
	Address   string
	Latitude  float64
	Longitude float64
	Radius    float64
	Proximity int
}

// NewRemindersService creates a new CloudKit-based reminders service
func NewRemindersService(client *Client) *RemindersService {
	cachePath := ""
	if home, err := os.UserHomeDir(); err == nil {
		cachePath = filepath.Join(home, ".icloud-cli", "cloudkit-cache.json")
	}
	replicaID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(client.apiClient.Session().ClientID+":reminders-crdt"))
	return newRemindersServiceWithReplica(client, cachePath, replicaID)
}

func newRemindersService(client *Client, cachePath string) *RemindersService {
	return newRemindersServiceWithReplica(client, cachePath, uuid.NewSHA1(uuid.NameSpaceOID, []byte("icloud-cli-test-replica")))
}

func newRemindersServiceWithReplica(client *Client, cachePath string, replicaID uuid.UUID) *RemindersService {
	return &RemindersService{
		client:    client,
		zoneID:    ZoneID{ZoneName: RemindersZone},
		records:   make(map[string]Record),
		cachePath: cachePath,
		replicaID: replicaID,
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
	if f, ok := r.Fields["CompletionDate"]; ok {
		if value, ok := numericInt64(f.Value); ok {
			date := time.UnixMilli(value)
			item.CompletionDate = &date
		}
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

// GetReminderRecord returns the exact CloudKit record for a reminder.
func (s *RemindersService) GetReminderRecord(reminderID string) (Record, error) {
	if err := s.ensureZone(); err != nil {
		return Record{}, err
	}
	return s.lookupReminder(reminderID)
}

// GetReminderProperties resolves native properties stored on linked records.
func (s *RemindersService) GetReminderProperties(reminderID string) (ReminderProperties, error) {
	record, err := s.GetReminderRecord(reminderID)
	if err != nil {
		return ReminderProperties{}, err
	}
	var properties ReminderProperties
	properties.TimeZone, _ = record.Fields["TimeZone"].Value.(string)
	properties.AllDay = fieldBool(record.Fields["AllDay"].Value)
	if field, ok := record.Fields["DueDateDeltaAlertsData"]; ok {
		if encoded, ok := field.Value.(string); ok {
			if raw, decodeErr := base64.StdEncoding.DecodeString(encoded); decodeErr == nil {
				var envelope dueDateDeltaAlertsEnvelope
				if json.Unmarshal(raw, &envelope) == nil && len(envelope.DueDateDeltaAlerts) > 0 {
					alert := envelope.DueDateDeltaAlerts[0]
					properties.EarlyReminder = &EarlyReminder{Unit: alert.DueDateDeltaUnit, Count: absInt(alert.DueDateDeltaCount)}
				}
			}
		}
	}
	if _, exists := record.Fields["UrgentPresentationAlarmsAsData"]; exists {
		enabled := false
		if envelope, ok := s.decodeUrgentPresentationEnvelope(record); ok {
			for _, account := range envelope.Account {
				enabled = enabled || account.IsEnabled
			}
		}
		properties.Urgent = &enabled
	}

	attachmentIDs := fieldStringList(record.Fields["AttachmentIDs"].Value)
	attachments, err := s.lookupRelatedRecords("Attachment/", attachmentIDs)
	if err != nil {
		return ReminderProperties{}, fmt.Errorf("lookup reminder attachments: %w", err)
	}
	for _, attachment := range attachments {
		if attachment.Fields["Type"].Value == "URL" {
			properties.URL = decodeCloudKitText(attachment.Fields["URL"])
		}
	}

	hashtags, err := s.lookupRelatedRecords("Hashtag/", fieldStringList(record.Fields["HashtagIDs"].Value))
	if err != nil {
		return ReminderProperties{}, fmt.Errorf("lookup reminder tags: %w", err)
	}
	for _, hashtag := range hashtags {
		if name := decodeCloudKitText(hashtag.Fields["Name"]); name != "" {
			properties.Tags = append(properties.Tags, name)
		}
	}

	assignments, err := s.lookupRelatedRecords("Assignment/", fieldStringList(record.Fields["AssignmentIDs"].Value))
	if err != nil {
		return ReminderProperties{}, fmt.Errorf("lookup reminder assignments: %w", err)
	}
	if len(assignments) > 0 {
		properties.Assignee = decodeCloudKitText(assignments[0].Fields["EncryptedAssigneeIdentifier"])
	}

	rules, err := s.lookupRelatedRecords("RecurrenceRule/", fieldStringList(record.Fields["RecurrenceRuleIDs"].Value))
	if err != nil {
		return ReminderProperties{}, fmt.Errorf("lookup reminder recurrence: %w", err)
	}
	if len(rules) > 0 {
		frequency, _ := numericInt64(rules[0].Fields["Frequency"].Value)
		interval, _ := numericInt64(rules[0].Fields["Interval"].Value)
		recurrence := &RecurrenceRule{Frequency: int(frequency), Interval: int(interval)}
		if end, ok := numericInt64(rules[0].Fields["EndDate"].Value); ok {
			date := time.UnixMilli(end)
			recurrence.EndDate = &date
		}
		properties.Recurrence = recurrence
	}

	alarms, err := s.lookupRelatedRecords("Alarm/", fieldStringList(record.Fields["AlarmIDs"].Value))
	if err != nil {
		return ReminderProperties{}, fmt.Errorf("lookup reminder alarms: %w", err)
	}
	var triggerIDs []string
	for _, alarm := range alarms {
		if id, ok := alarm.Fields["TriggerID"].Value.(string); ok && id != "" {
			triggerIDs = append(triggerIDs, id)
		}
	}
	triggers, err := s.lookupRelatedRecords("AlarmTrigger/", triggerIDs)
	if err != nil {
		return ReminderProperties{}, fmt.Errorf("lookup reminder alarm triggers: %w", err)
	}
	for _, trigger := range triggers {
		if trigger.Fields["Type"].Value != "Location" {
			continue
		}
		latitude, _ := numericFloat64(trigger.Fields["Latitude"].Value)
		longitude, _ := numericFloat64(trigger.Fields["Longitude"].Value)
		radius, _ := numericFloat64(trigger.Fields["Radius"].Value)
		proximity, _ := numericInt64(trigger.Fields["Proximity"].Value)
		properties.Location = &LocationAlarm{Title: decodeCloudKitText(trigger.Fields["Title"]), Address: decodeCloudKitText(trigger.Fields["Address"]), Latitude: latitude, Longitude: longitude, Radius: radius, Proximity: int(proximity)}
		break
	}
	return properties, nil
}

func (s *RemindersService) lookupRelatedRecords(prefix string, ids []string) ([]Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, prefix+id)
	}
	response, err := s.client.LookupRecords(RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, names)
	if err != nil {
		return nil, err
	}
	for _, record := range response.Records {
		if err := recordError(record); err != nil {
			return nil, err
		}
	}
	return response.Records, nil
}

func numericFloat64(value interface{}) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case int64:
		return float64(value), true
	case int:
		return float64(value), true
	default:
		return 0, false
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
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

func recordReferenceName(field FieldValue) string {
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

// replaceDocumentText keeps the document's CRDT identity and operation history.
// Replacing an existing document with a freshly encoded snapshot creates a
// concurrent branch, which Reminders resolves by concatenating both strings.
func replaceDocumentText(encoded, text string, replicaUUID uuid.UUID) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode document: %w", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("open document compressor: %w", err)
	}
	message, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("read document: %w", err)
	}
	if err := reader.Close(); err != nil {
		return "", fmt.Errorf("close document compressor: %w", err)
	}

	message, replaced, err := rewriteBytesField(message, 2, func(document []byte) ([]byte, error) {
		return rewriteRequiredBytesField(document, 3, func(note []byte) ([]byte, error) {
			return replaceCRDTString(note, text, replicaUUID)
		})
	})
	if err != nil {
		return "", fmt.Errorf("replace document text: %w", err)
	}
	if !replaced {
		return "", fmt.Errorf("replace document text: document wrapper is missing")
	}

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(message); err != nil {
		return "", fmt.Errorf("compress document: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close document compressor: %w", err)
	}
	return base64.StdEncoding.EncodeToString(compressed.Bytes()), nil
}

type protobufField struct {
	number  int
	wire    int
	varint  uint64
	payload []byte
}

func replaceCRDTString(message []byte, text string, replicaUUID uuid.UUID) ([]byte, error) {
	fields, err := decodeProtobufFields(message)
	if err != nil {
		return nil, err
	}
	textIndex := -1
	metadataIndex := -1
	var substringIndexes []int
	var attributeIndexes []int
	for index, field := range fields {
		switch {
		case field.number == 2 && field.wire == 2 && textIndex == -1:
			textIndex = index
		case field.number == 3 && field.wire == 2:
			substringIndexes = append(substringIndexes, index)
		case field.number == 4 && field.wire == 2 && metadataIndex == -1:
			metadataIndex = index
		case field.number == 5 && field.wire == 2:
			attributeIndexes = append(attributeIndexes, index)
		}
	}
	if textIndex == -1 || metadataIndex == -1 || len(substringIndexes) < 2 {
		return nil, fmt.Errorf("incomplete CRDT string document")
	}
	var attributeTemplate []protobufField
	if len(attributeIndexes) > 0 {
		attributeTemplate, _ = decodeProtobufFields(fields[attributeIndexes[len(attributeIndexes)-1]].payload)
	}

	metadata, err := decodeProtobufFields(fields[metadataIndex].payload)
	if err != nil {
		return nil, fmt.Errorf("decode vector timestamp: %w", err)
	}
	var maximumCharClock uint64
	var maximumTimestamp uint64
	for _, fieldIndex := range substringIndexes {
		substring, decodeErr := decodeProtobufFields(fields[fieldIndex].payload)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode CRDT substring clock: %w", decodeErr)
		}
		charID, _ := protobufBytesValue(substring, 1)
		charFields, _ := decodeProtobufFields(charID)
		replica := protobufVarintValue(charFields, 1)
		clock := protobufVarintValue(charFields, 2)
		substringLength := protobufVarintValue(substring, 2)
		isSentinel := replica == 0 && clock == 0xffffffff
		if !isSentinel {
			end := clock
			if substringLength > 0 {
				end += substringLength - 1
			}
			if end > maximumCharClock {
				maximumCharClock = end
			}
		}
		if !isSentinel {
			timestamp, _ := protobufBytesValue(substring, 3)
			timestampFields, _ := decodeProtobufFields(timestamp)
			if value := protobufVarintValue(timestampFields, 2); value > maximumTimestamp {
				maximumTimestamp = value
			}
		}
	}
	length := uint64(len(utf16.Encode([]rune(text))))
	newCharClock := maximumCharClock
	tombstoneTimestamp := maximumTimestamp + 1
	replicaSlot := -1
	var replicaTimestamp uint64
	var maximumOtherReplicaTimestamp uint64
	for index, field := range metadata {
		if field.number != 1 || field.wire != 2 {
			continue
		}
		entry, decodeErr := decodeProtobufFields(field.payload)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode replica entry: %w", decodeErr)
		}
		uuidBytes, ok := protobufBytesValue(entry, 1)
		isReplica := ok && bytes.Equal(uuidBytes, replicaUUID[:])
		clockIndex := 0
		for _, entryField := range entry {
			if entryField.number != 2 || entryField.wire != 2 {
				continue
			}
			clockIndex++
			clock, clockErr := decodeProtobufFields(entryField.payload)
			if clockErr != nil {
				return nil, fmt.Errorf("decode replica clock: %w", clockErr)
			}
			if isReplica && clockIndex == 1 {
				newCharClock = protobufVarintValue(clock, 1)
			} else if clockIndex == 2 {
				value := protobufVarintValue(clock, 1)
				if isReplica {
					replicaTimestamp = value
				} else if value > maximumOtherReplicaTimestamp {
					maximumOtherReplicaTimestamp = value
				}
			}
		}
		if isReplica {
			replicaSlot = index
		}
	}
	if replicaSlot >= 0 {
		tombstoneTimestamp = replicaTimestamp
		if tombstoneTimestamp <= maximumOtherReplicaTimestamp {
			tombstoneTimestamp = maximumOtherReplicaTimestamp + 1
		}
	} else if tombstoneTimestamp <= maximumOtherReplicaTimestamp {
		tombstoneTimestamp = maximumOtherReplicaTimestamp + 1
	}
	replicaID := uint64(replicaSlot + 1)
	if replicaSlot == -1 {
		replicaID = uint64(len(metadata) + 1)
	}
	clockEntry := encodeField(1, 2, replicaUUID[:])
	clockEntry = append(clockEntry, encodeField(2, 2, encodeField(1, 0, newCharClock+length))...)
	clockEntry = append(clockEntry, encodeField(2, 2, encodeField(1, 0, tombstoneTimestamp+1))...)
	if replicaSlot == -1 {
		metadata = append(metadata, protobufField{number: 1, wire: 2, payload: clockEntry})
	} else {
		metadata[replicaSlot].payload = clockEntry
	}
	fields[metadataIndex].payload = encodeProtobufFields(metadata)

	for arrayIndex, fieldIndex := range substringIndexes {
		substring, err := decodeProtobufFields(fields[fieldIndex].payload)
		if err != nil {
			return nil, fmt.Errorf("decode CRDT substring %d: %w", arrayIndex, err)
		}
		if arrayIndex > 0 {
			for index := range substring {
				if substring[index].number == 5 && substring[index].wire == 0 {
					substring[index].varint++
				}
			}
		} else {
			firstChild := true
			for index := range substring {
				if substring[index].number != 5 || substring[index].wire != 0 {
					continue
				}
				if firstChild {
					firstChild = false
					continue
				}
				substring[index].varint++
			}
		}
		if arrayIndex > 0 && arrayIndex < len(substringIndexes)-1 && !protobufBoolField(substring, 4) {
			substring = setProtobufVarint(substring, 4, 1)
			timestamp := encodeField(1, 0, replicaID)
			timestamp = append(timestamp, encodeField(2, 0, tombstoneTimestamp)...)
			substring = setProtobufBytes(substring, 3, timestamp)
		}
		fields[fieldIndex].payload = encodeProtobufFields(substring)
	}

	newSubstring := encodeField(1, 2, append(encodeField(1, 0, replicaID), encodeField(2, 0, newCharClock)...))
	newSubstring = append(newSubstring, encodeField(2, 0, length)...)
	newSubstring = append(newSubstring, encodeField(3, 2, append(encodeField(1, 0, replicaID), encodeField(2, 0, uint64(0))...))...)
	newSubstring = append(newSubstring, encodeField(5, 0, uint64(2))...)
	docStartIndex := substringIndexes[0]
	fields = append(fields[:docStartIndex+1], append([]protobufField{{number: 3, wire: 2, payload: newSubstring}}, fields[docStartIndex+1:]...)...)

	fields[textIndex].payload = []byte(text)
	attributeTemplate = setProtobufVarint(attributeTemplate, 1, length)
	var withoutAttributes []protobufField
	for _, field := range fields {
		if field.number != 5 || field.wire != 2 {
			withoutAttributes = append(withoutAttributes, field)
		}
	}
	withoutAttributes = append(withoutAttributes, protobufField{number: 5, wire: 2, payload: encodeProtobufFields(attributeTemplate)})
	return encodeProtobufFields(withoutAttributes), nil
}

func decodeProtobufFields(message []byte) ([]protobufField, error) {
	var fields []protobufField
	for offset := 0; offset < len(message); {
		tag, tagBytes := decodeVarint(message[offset:])
		if tagBytes == 0 {
			return nil, fmt.Errorf("invalid protobuf tag at byte %d", offset)
		}
		offset += tagBytes
		field := protobufField{number: int(tag >> 3), wire: int(tag & 7)}
		switch field.wire {
		case 0:
			value, valueBytes := decodeVarint(message[offset:])
			if valueBytes == 0 {
				return nil, fmt.Errorf("invalid protobuf varint at byte %d", offset)
			}
			field.varint = value
			offset += valueBytes
		case 2:
			length, lengthBytes := decodeVarint(message[offset:])
			if lengthBytes == 0 {
				return nil, fmt.Errorf("invalid protobuf length at byte %d", offset)
			}
			offset += lengthBytes
			end := offset + int(length)
			if end < offset || end > len(message) {
				return nil, fmt.Errorf("invalid protobuf field %d length", field.number)
			}
			field.payload = append([]byte(nil), message[offset:end]...)
			offset = end
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d", field.wire)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func encodeProtobufFields(fields []protobufField) []byte {
	var message []byte
	for _, field := range fields {
		message = append(message, encodeVarint(uint64(field.number<<3|field.wire))...)
		if field.wire == 0 {
			message = append(message, encodeVarint(field.varint)...)
		} else {
			message = append(message, encodeVarint(uint64(len(field.payload)))...)
			message = append(message, field.payload...)
		}
	}
	return message
}

func protobufBoolField(fields []protobufField, number int) bool {
	for _, field := range fields {
		if field.number == number && field.wire == 0 {
			return field.varint != 0
		}
	}
	return false
}

func setProtobufVarint(fields []protobufField, number int, value uint64) []protobufField {
	for index := range fields {
		if fields[index].number == number && fields[index].wire == 0 {
			fields[index].varint = value
			return fields
		}
	}
	return append(fields, protobufField{number: number, wire: 0, varint: value})
}

func setProtobufBytes(fields []protobufField, number int, value []byte) []protobufField {
	for index := range fields {
		if fields[index].number == number && fields[index].wire == 2 {
			fields[index].payload = value
			return fields
		}
	}
	return append(fields, protobufField{number: number, wire: 2, payload: value})
}

func protobufVarintValue(fields []protobufField, number int) uint64 {
	for _, field := range fields {
		if field.number == number && field.wire == 0 {
			return field.varint
		}
	}
	return 0
}

func protobufBytesValue(fields []protobufField, number int) ([]byte, bool) {
	for _, field := range fields {
		if field.number == number && field.wire == 2 {
			return field.payload, true
		}
	}
	return nil, false
}

func rewriteRequiredBytesField(message []byte, number int, rewrite func([]byte) ([]byte, error)) ([]byte, error) {
	updated, replaced, err := rewriteBytesField(message, number, rewrite)
	if err != nil {
		return nil, err
	}
	if !replaced {
		return nil, fmt.Errorf("protobuf field %d is missing", number)
	}
	return updated, nil
}

func replaceBytesFieldValue(message []byte, number int, value []byte) ([]byte, bool, error) {
	return rewriteBytesField(message, number, func([]byte) ([]byte, error) { return value, nil })
}

func rewriteBytesField(message []byte, number int, rewrite func([]byte) ([]byte, error)) ([]byte, bool, error) {
	var result []byte
	replaced := false
	for offset := 0; offset < len(message); {
		start := offset
		tag, tagBytes := decodeVarint(message[offset:])
		if tagBytes == 0 {
			return nil, false, fmt.Errorf("invalid protobuf tag at byte %d", offset)
		}
		offset += tagBytes
		fieldNumber, wireType := int(tag>>3), int(tag&7)
		switch wireType {
		case 0:
			_, valueBytes := decodeVarint(message[offset:])
			if valueBytes == 0 {
				return nil, false, fmt.Errorf("invalid protobuf varint at byte %d", offset)
			}
			offset += valueBytes
			result = append(result, message[start:offset]...)
		case 2:
			length, lengthBytes := decodeVarint(message[offset:])
			if lengthBytes == 0 {
				return nil, false, fmt.Errorf("invalid protobuf length at byte %d", offset)
			}
			offset += lengthBytes
			end := offset + int(length)
			if end < offset || end > len(message) {
				return nil, false, fmt.Errorf("invalid protobuf field %d length", fieldNumber)
			}
			if fieldNumber == number && !replaced {
				value, err := rewrite(message[offset:end])
				if err != nil {
					return nil, false, err
				}
				result = append(result, message[start:start+tagBytes]...)
				result = append(result, encodeVarint(uint64(len(value)))...)
				result = append(result, value...)
				replaced = true
			} else {
				result = append(result, message[start:end]...)
			}
			offset = end
		default:
			return nil, false, fmt.Errorf("unsupported protobuf wire type %d", wireType)
		}
	}
	return result, replaced, nil
}

func replaceVarintFieldValue(message []byte, number int, value uint64) ([]byte, error) {
	var result []byte
	replaced := false
	for offset := 0; offset < len(message); {
		start := offset
		tag, tagBytes := decodeVarint(message[offset:])
		if tagBytes == 0 {
			return nil, fmt.Errorf("invalid protobuf tag at byte %d", offset)
		}
		offset += tagBytes
		fieldNumber, wireType := int(tag>>3), int(tag&7)
		if wireType != 0 {
			return nil, fmt.Errorf("unexpected protobuf wire type %d in attributes", wireType)
		}
		_, valueBytes := decodeVarint(message[offset:])
		if valueBytes == 0 {
			return nil, fmt.Errorf("invalid protobuf varint at byte %d", offset)
		}
		offset += valueBytes
		if fieldNumber == number && !replaced {
			result = append(result, message[start:start+tagBytes]...)
			result = append(result, encodeVarint(value)...)
			replaced = true
		} else {
			result = append(result, message[start:offset]...)
		}
	}
	if !replaced {
		return nil, fmt.Errorf("protobuf field %d is missing", number)
	}
	return result, nil
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
	var resolutionKeys []string
	if changes.Title != nil {
		encoded, err := s.replaceReminderDocument(existing, "TitleDocument", *changes.Title)
		if err != nil {
			return fmt.Errorf("encode title: %w", err)
		}
		fields["TitleDocument"] = FieldValue{Value: encoded}
		resolutionKeys = append(resolutionKeys, "titleDocument")
	}
	if changes.Notes != nil {
		encoded, err := s.replaceReminderDocument(existing, "NotesDocument", *changes.Notes)
		if err != nil {
			return fmt.Errorf("encode notes: %w", err)
		}
		fields["NotesDocument"] = FieldValue{Value: encoded}
		resolutionKeys = append(resolutionKeys, "notesDocument")
	}
	if changes.Priority != nil {
		fields["Priority"] = FieldValue{Value: *changes.Priority}
	}
	if changes.Flagged != nil {
		value := int64(0)
		if *changes.Flagged {
			value = 1
		}
		fields["Flagged"] = FieldValue{Value: value, Type: "NUMBER_INT64"}
		resolutionKeys = append(resolutionKeys, "flagged")
	}
	if len(fields) == 0 {
		return fmt.Errorf("no reminder changes specified")
	}
	if len(resolutionKeys) > 0 {
		if err := addResolutionTokenUpdates(existing, fields, resolutionKeys...); err != nil {
			return fmt.Errorf("update reminder resolution tokens: %w", err)
		}
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

func (s *RemindersService) replaceReminderDocument(record Record, fieldName, text string) (string, error) {
	field, ok := record.Fields[fieldName]
	if !ok {
		return encodeTitleDocument(text)
	}
	encoded, ok := field.Value.(string)
	if !ok || encoded == "" {
		return encodeTitleDocument(text)
	}
	return replaceDocumentText(encoded, text, s.replicaID)
}

func addResolutionTokenUpdate(record Record, fields map[string]FieldValue, key string) error {
	return addResolutionTokenUpdates(record, fields, key)
}

func addResolutionTokenUpdates(record Record, fields map[string]FieldValue, keys ...string) error {
	var envelope struct {
		Map map[string]map[string]interface{} `json:"map"`
	}
	tokenField, exists := record.Fields["ResolutionTokenMap"]
	if exists {
		encoded, ok := tokenField.Value.(string)
		if !ok || encoded == "" {
			return fmt.Errorf("record has an invalid ResolutionTokenMap")
		}
		if err := json.Unmarshal([]byte(encoded), &envelope); err != nil {
			return err
		}
	}
	if envelope.Map == nil {
		envelope.Map = make(map[string]map[string]interface{})
	}
	now := time.Now()
	coreDataTime := float64(now.UnixMilli())/1000 - 978307200
	keys = append(keys, "lastModifiedDate")
	for _, tokenKey := range keys {
		token, ok := envelope.Map[tokenKey]
		if !ok {
			envelope.Map[tokenKey] = map[string]interface{}{
				"counter": float64(1), "modificationTime": coreDataTime,
				"replicaID": strings.ToUpper(uuid.New().String()),
			}
			continue
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

// GetSharees returns the accepted participants of the CloudKit share rooted at listID.
func (s *RemindersService) GetSharees(listID string) ([]ReminderSharee, error) {
	if err := s.Sync(false); err != nil {
		return nil, err
	}
	var shareNames []string
	for _, record := range s.records {
		if record.RecordType == "cloudkit.share" {
			shareNames = append(shareNames, record.RecordName)
		}
	}
	if len(shareNames) == 0 {
		return nil, fmt.Errorf("list is not shared")
	}
	response, err := s.client.LookupRecords(
		RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, shareNames,
	)
	if err != nil {
		return nil, fmt.Errorf("lookup list shares: %w", err)
	}
	for _, share := range response.Records {
		if err := recordError(share); err != nil {
			continue
		}
		if recordReferenceName(share.Fields["RootRecord"]) != listID {
			continue
		}
		return acceptedSharees(share), nil
	}
	return nil, fmt.Errorf("list is not shared")
}

func acceptedSharees(share Record) []ReminderSharee {
	participants := append([]ShareParticipant(nil), share.Participants...)
	if share.Owner != nil {
		participants = append(participants, *share.Owner)
	}
	currentID := ""
	if share.CurrentUser != nil {
		currentID = share.CurrentUser.ParticipantID
		participants = append(participants, *share.CurrentUser)
	}
	seen := make(map[string]bool)
	result := make([]ReminderSharee, 0, len(participants))
	for _, participant := range participants {
		if participant.ParticipantID == "" || seen[participant.ParticipantID] ||
			!strings.EqualFold(participant.AcceptanceStatus, "ACCEPTED") {
			continue
		}
		seen[participant.ParticipantID] = true
		name := strings.TrimSpace(strings.Join([]string{
			participant.UserIdentity.NameComponents.GivenName,
			participant.UserIdentity.NameComponents.FamilyName,
		}, " "))
		result = append(result, ReminderSharee{
			ParticipantID:  participant.ParticipantID,
			UserRecordName: participant.UserIdentity.UserRecordName,
			DisplayName:    name,
			Email:          participant.UserIdentity.LookupInfo.EmailAddress,
			Phone:          participant.UserIdentity.LookupInfo.PhoneNumber,
			CurrentUser:    participant.ParticipantID == currentID,
		})
	}
	return result
}

// UpdateAssignment assigns a shared reminder to one participant, or clears it.
func (s *RemindersService) UpdateAssignment(reminderID, assignee string, clear bool) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	listID := recordListID(existing)
	if listID == "" {
		return fmt.Errorf("reminder has no list")
	}
	sharees, err := s.GetSharees(listID)
	if err != nil {
		return err
	}

	var target *ReminderSharee
	if !clear {
		resolved, err := resolveSharee(sharees, assignee)
		if err != nil {
			return err
		}
		target = &resolved
	}
	var originator *ReminderSharee
	for i := range sharees {
		if sharees[i].CurrentUser {
			originator = &sharees[i]
			break
		}
	}
	if originator == nil {
		return fmt.Errorf("current user is not an accepted participant of the shared list")
	}

	currentIDs := fieldStringList(existing.Fields["AssignmentIDs"].Value)
	if clear && len(currentIDs) == 0 {
		return nil
	}
	childOperations := make([]RecordOperation, 0, len(currentIDs)+1)
	if len(currentIDs) > 0 {
		names := make([]string, 0, len(currentIDs))
		for _, id := range currentIDs {
			names = append(names, "Assignment/"+id)
		}
		lookup, err := s.client.LookupRecords(
			RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, names,
		)
		if err != nil {
			return fmt.Errorf("lookup current assignments: %w", err)
		}
		for _, record := range lookup.Records {
			if err := recordError(record); err != nil {
				return fmt.Errorf("lookup current assignment: %w", err)
			}
			childOperations = append(childOperations, RecordOperation{
				OperationType: OperationDelete,
				Record:        Record{RecordName: record.RecordName, RecordType: "Assignment", RecordChangeTag: record.RecordChangeTag},
			})
		}
	}

	now := time.Now()
	assignmentIDs := []string{}
	if target != nil {
		id := strings.ToUpper(uuid.New().String())
		assignmentIDs = []string{id}
		childOperations = append(childOperations, RecordOperation{
			OperationType: OperationCreate,
			Record: Record{
				RecordName: "Assignment/" + id,
				RecordType: "Assignment",
				Parent:     &RecordReference{RecordName: reminderID},
				Fields: map[string]FieldValue{
					"AssignedDate":                  {Value: now.UnixMilli(), Type: "TIMESTAMP"},
					"Deleted":                       {Value: int64(0), Type: "NUMBER_INT64"},
					"EncryptedAssigneeIdentifier":   {Value: target.ParticipantID, Type: "STRING", IsEncrypted: true},
					"EncryptedOriginatorIdentifier": {Value: originator.ParticipantID, Type: "STRING", IsEncrypted: true},
					"Imported":                      {Value: int64(0), Type: "NUMBER_INT64"},
					"OwningReminderIdentifier":      {Value: strings.TrimPrefix(reminderID, "Reminder/"), Type: "STRING"},
					"Reminder":                      {Value: RecordReference{RecordName: reminderID, Action: "VALIDATE"}},
					"Status":                        {Value: int64(1), Type: "NUMBER_INT64"},
				},
			},
		})
	}
	tokenMap, err := newResolutionTokenMap("assignmentIDs", "lastModifiedDate")
	if err != nil {
		return err
	}
	operations := append([]RecordOperation{{
		OperationType: OperationUpdate,
		Record: Record{
			RecordName: reminderID, RecordType: "Reminder", RecordChangeTag: existing.RecordChangeTag,
			Fields: map[string]FieldValue{
				"AssignmentIDs":      {Value: assignmentIDs, Type: "STRING_LIST"},
				"ResolutionTokenMap": {Value: tokenMap, Type: "STRING"},
				"LastModifiedDate":   {Value: now.UnixMilli(), Type: "TIMESTAMP"},
			},
		},
	}}, childOperations...)
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, ModifyRequest{
		ZoneID: s.zoneID, Operations: operations, Atomic: true,
	})
	if err != nil {
		return fmt.Errorf("update assignment: %w", err)
	}
	if len(response.Records) != len(operations) {
		return fmt.Errorf("update assignment: got %d records, want %d", len(response.Records), len(operations))
	}
	for _, record := range response.Records {
		if err := recordError(record); err != nil {
			return fmt.Errorf("update assignment: %w", err)
		}
	}
	s.records[reminderID] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

func resolveSharee(sharees []ReminderSharee, selector string) (ReminderSharee, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return ReminderSharee{}, fmt.Errorf("assignee cannot be empty")
	}
	var matches []ReminderSharee
	for _, sharee := range sharees {
		matched := strings.EqualFold(selector, sharee.ParticipantID) ||
			strings.EqualFold(selector, sharee.UserRecordName) ||
			strings.EqualFold(selector, sharee.Email) ||
			strings.EqualFold(selector, sharee.Phone) ||
			strings.EqualFold(selector, sharee.DisplayName) ||
			(strings.EqualFold(selector, "me") && sharee.CurrentUser)
		if matched {
			matches = append(matches, sharee)
		}
	}
	if len(matches) == 0 {
		return ReminderSharee{}, fmt.Errorf("no accepted share participant matches %q", selector)
	}
	if len(matches) > 1 {
		return ReminderSharee{}, fmt.Errorf("multiple share participants match %q; use an email or participant ID", selector)
	}
	return matches[0], nil
}

// UpdateLocationAlarm replaces the reminder's location alarm or clears it.
// Other alarm types remain linked to the reminder.
func (s *RemindersService) UpdateLocationAlarm(reminderID string, location *LocationAlarm) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	if location != nil {
		if location.Latitude < -90 || location.Latitude > 90 {
			return fmt.Errorf("latitude must be between -90 and 90")
		}
		if location.Longitude < -180 || location.Longitude > 180 {
			return fmt.Errorf("longitude must be between -180 and 180")
		}
		if location.Radius <= 0 || location.Radius > 100000 {
			return fmt.Errorf("radius must be greater than 0 and at most 100000 meters")
		}
		if location.Proximity != 1 && location.Proximity != 2 {
			return fmt.Errorf("proximity must be arriving or leaving")
		}
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}

	remainingAlarmIDs, deleteOperations, err := s.locationAlarmDeletes(
		fieldStringList(existing.Fields["AlarmIDs"].Value),
	)
	if err != nil {
		return err
	}
	if location == nil && len(deleteOperations) == 0 {
		return nil
	}

	now := time.Now()
	childOperations := deleteOperations
	if location != nil {
		alarmID := strings.ToUpper(uuid.New().String())
		triggerID := strings.ToUpper(uuid.New().String())
		locationID := strings.ToUpper(uuid.New().String())
		remainingAlarmIDs = append(remainingAlarmIDs, alarmID)
		alarmName := "Alarm/" + alarmID
		childOperations = append(childOperations,
			RecordOperation{
				OperationType: OperationCreate,
				Record: Record{
					RecordName: alarmName,
					RecordType: "Alarm",
					Parent:     &RecordReference{RecordName: reminderID},
					Fields: map[string]FieldValue{
						"AlarmUID":                      {Value: alarmID, Type: "STRING"},
						"Deleted":                       {Value: int64(0), Type: "NUMBER_INT64"},
						"Imported":                      {Value: int64(0), Type: "NUMBER_INT64"},
						"Reminder":                      {Value: RecordReference{RecordName: reminderID, Action: "VALIDATE"}, Type: "REFERENCE"},
						"TriggerID":                     {Value: triggerID, Type: "STRING"},
						"DueDateResolutionTokenAsNonce": {Value: float64(0), Type: "NUMBER_DOUBLE"},
					},
				},
			},
			RecordOperation{
				OperationType: OperationCreate,
				Record: Record{
					RecordName: "AlarmTrigger/" + triggerID,
					RecordType: "AlarmTrigger",
					Parent:     &RecordReference{RecordName: alarmName},
					Fields: map[string]FieldValue{
						"Address":              {Value: location.Address, Type: "STRING", IsEncrypted: true},
						"Alarm":                {Value: RecordReference{RecordName: alarmName, Action: "VALIDATE"}, Type: "REFERENCE"},
						"Deleted":              {Value: int64(0), Type: "NUMBER_INT64"},
						"Imported":             {Value: int64(0), Type: "NUMBER_INT64"},
						"Latitude":             {Value: location.Latitude, Type: "NUMBER_DOUBLE", IsEncrypted: true},
						"LocationUID":          {Value: locationID, Type: "STRING"},
						"Longitude":            {Value: location.Longitude, Type: "NUMBER_DOUBLE", IsEncrypted: true},
						"Proximity":            {Value: int64(location.Proximity), Type: "NUMBER_INT64"},
						"Radius":               {Value: location.Radius, Type: "NUMBER_DOUBLE"},
						"ReferenceFrameString": {Value: "1", Type: "STRING", IsEncrypted: true},
						"Title":                {Value: location.Title, Type: "STRING", IsEncrypted: true},
						"Type":                 {Value: "Location", Type: "STRING"},
					},
				},
			},
		)
	}

	tokenMap, err := newResolutionTokenMap("lastModifiedDate")
	if err != nil {
		return err
	}
	operations := append([]RecordOperation{{
		OperationType: OperationUpdate,
		Record: Record{
			RecordName: reminderID, RecordType: "Reminder", RecordChangeTag: existing.RecordChangeTag,
			Fields: map[string]FieldValue{
				"AlarmIDs":           {Value: remainingAlarmIDs, Type: "STRING_LIST"},
				"ResolutionTokenMap": {Value: tokenMap, Type: "STRING"},
				"LastModifiedDate":   {Value: now.UnixMilli(), Type: "TIMESTAMP"},
			},
		},
	}}, childOperations...)
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, ModifyRequest{
		ZoneID: s.zoneID, Operations: operations, Atomic: true,
	})
	if err != nil {
		return fmt.Errorf("update location alarm: %w", err)
	}
	if len(response.Records) != len(operations) {
		return fmt.Errorf("update location alarm: got %d records, want %d", len(response.Records), len(operations))
	}
	for _, record := range response.Records {
		if err := recordError(record); err != nil {
			return fmt.Errorf("update location alarm: %w", err)
		}
	}
	s.records[reminderID] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

func (s *RemindersService) locationAlarmDeletes(alarmIDs []string) ([]string, []RecordOperation, error) {
	if len(alarmIDs) == 0 {
		return nil, nil, nil
	}
	alarmNames := make([]string, 0, len(alarmIDs))
	for _, id := range alarmIDs {
		alarmNames = append(alarmNames, "Alarm/"+id)
	}
	alarms, err := s.client.LookupRecords(RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, alarmNames)
	if err != nil {
		return nil, nil, fmt.Errorf("lookup reminder alarms: %w", err)
	}
	remaining := make([]string, 0, len(alarmIDs))
	deletes := make([]RecordOperation, 0)
	for _, alarm := range alarms.Records {
		if err := recordError(alarm); err != nil {
			return nil, nil, fmt.Errorf("lookup reminder alarm: %w", err)
		}
		triggerID, _ := alarm.Fields["TriggerID"].Value.(string)
		if triggerID == "" {
			remaining = append(remaining, strings.TrimPrefix(alarm.RecordName, "Alarm/"))
			continue
		}
		triggers, err := s.client.LookupRecords(
			RemindersContainer, RemindersEnv, RemindersDB, s.zoneID,
			[]string{"AlarmTrigger/" + triggerID},
		)
		if err != nil {
			return nil, nil, fmt.Errorf("lookup alarm trigger: %w", err)
		}
		if len(triggers.Records) == 0 {
			return nil, nil, fmt.Errorf("alarm trigger not found: %s", triggerID)
		}
		trigger := triggers.Records[0]
		if err := recordError(trigger); err != nil {
			return nil, nil, fmt.Errorf("lookup alarm trigger: %w", err)
		}
		if trigger.Fields["Type"].Value != "Location" {
			remaining = append(remaining, strings.TrimPrefix(alarm.RecordName, "Alarm/"))
			continue
		}
		deletes = append(deletes,
			RecordOperation{OperationType: OperationDelete, Record: Record{
				RecordName: trigger.RecordName, RecordType: "AlarmTrigger", RecordChangeTag: trigger.RecordChangeTag,
			}},
			RecordOperation{OperationType: OperationDelete, Record: Record{
				RecordName: alarm.RecordName, RecordType: "Alarm", RecordChangeTag: alarm.RecordChangeTag,
			}},
		)
	}
	return remaining, deletes, nil
}

// UpdateDueDate replaces or clears a due date and its native Date alarm while
// preserving location and other alarm types.
func (s *RemindersService) UpdateDueDate(reminderID string, due *DueDateChange) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	remainingAlarmIDs, childOperations, err := s.dateAlarmDeletes(fieldStringList(existing.Fields["AlarmIDs"].Value))
	if err != nil {
		return err
	}

	now := time.Now()
	fields := map[string]FieldValue{
		"LastModifiedDate": {Value: now.UnixMilli(), Type: "TIMESTAMP"},
	}
	tokenKeys := []string{"dueDate"}
	if due == nil {
		fields["DueDate"] = FieldValue{Value: nil, Type: "TIMESTAMP"}
		fields["TimeZone"] = FieldValue{Value: nil, Type: "STRING"}
		tokenKeys = append(tokenKeys, "timeZone")
	} else {
		date := due.Date
		timestamp := date.UnixMilli()
		if due.AllDay {
			timestamp = wallClockTimestamp(date)
		}
		fields["DueDate"] = FieldValue{Value: timestamp, Type: "TIMESTAMP"}
		allDay := int64(0)
		if due.AllDay {
			allDay = 1
			fields["TimeZone"] = FieldValue{Value: nil, Type: "STRING"}
		} else {
			zone := due.TimeZone
			if zone == "" {
				zone = date.Location().String()
			}
			fields["TimeZone"] = FieldValue{Value: zone, Type: "STRING"}
			alarmID := strings.ToUpper(uuid.New().String())
			triggerID := strings.ToUpper(uuid.New().String())
			remainingAlarmIDs = append(remainingAlarmIDs, alarmID)
			alarmName := "Alarm/" + alarmID
			components, err := json.Marshal(map[string]interface{}{
				"era": 1, "year": date.Year(), "month": int(date.Month()), "day": date.Day(),
				"hour": date.Hour(), "minute": date.Minute(), "second": date.Second(),
				"timeZone": map[string]string{"identifier": zone},
			})
			if err != nil {
				return fmt.Errorf("encode date alarm components: %w", err)
			}
			childOperations = append(childOperations,
				RecordOperation{OperationType: OperationCreate, Record: Record{
					RecordName: alarmName, RecordType: "Alarm", Parent: &RecordReference{RecordName: reminderID},
					Fields: map[string]FieldValue{
						"AlarmUID": {Value: alarmID, Type: "STRING"}, "Deleted": {Value: int64(0), Type: "NUMBER_INT64"},
						"Imported":                      {Value: int64(0), Type: "NUMBER_INT64"},
						"Reminder":                      {Value: RecordReference{RecordName: reminderID, Action: "VALIDATE"}, Type: "REFERENCE"},
						"TriggerID":                     {Value: triggerID, Type: "STRING"},
						"DueDateResolutionTokenAsNonce": {Value: float64(100000000000) + float64(now.UnixMilli())/1000 - 978307200, Type: "NUMBER_DOUBLE"},
					},
				}},
				RecordOperation{OperationType: OperationCreate, Record: Record{
					RecordName: "AlarmTrigger/" + triggerID, RecordType: "AlarmTrigger", Parent: &RecordReference{RecordName: alarmName},
					Fields: map[string]FieldValue{
						"Alarm":              {Value: RecordReference{RecordName: alarmName, Action: "VALIDATE"}, Type: "REFERENCE"},
						"DateComponentsData": {Value: base64.StdEncoding.EncodeToString(components), Type: "BYTES"},
						"Deleted":            {Value: int64(0), Type: "NUMBER_INT64"}, "Imported": {Value: int64(0), Type: "NUMBER_INT64"},
						"Type": {Value: "Date", Type: "STRING"},
					},
				}},
			)
		}
		fields["AllDay"] = FieldValue{Value: allDay, Type: "NUMBER_INT64"}
		tokenKeys = append(tokenKeys, "allDay", "timeZone")
	}
	fields["AlarmIDs"] = FieldValue{Value: remainingAlarmIDs, Type: "STRING_LIST"}
	if err := addResolutionTokenUpdates(existing, fields, tokenKeys...); err != nil {
		return fmt.Errorf("update due date resolution tokens: %w", err)
	}

	parent := RecordOperation{OperationType: OperationUpdate, Record: Record{
		RecordName: reminderID, RecordType: "Reminder", RecordChangeTag: existing.RecordChangeTag, Fields: fields,
	}}
	operations := append([]RecordOperation{parent}, childOperations...)
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, ModifyRequest{
		ZoneID: s.zoneID, Operations: operations, Atomic: true,
	})
	if err != nil {
		return fmt.Errorf("update due date: %w", err)
	}
	if len(response.Records) != len(operations) {
		return fmt.Errorf("update due date: got %d records, want %d", len(response.Records), len(operations))
	}
	for _, record := range response.Records {
		if err := recordError(record); err != nil {
			return fmt.Errorf("update due date: %w", err)
		}
	}
	s.records[reminderID] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

func wallClockTimestamp(date time.Time) int64 {
	return time.Date(date.Year(), date.Month(), date.Day(), date.Hour(), date.Minute(), date.Second(), date.Nanosecond(), time.UTC).UnixMilli()
}

func (s *RemindersService) dateAlarmDeletes(alarmIDs []string) ([]string, []RecordOperation, error) {
	if len(alarmIDs) == 0 {
		return nil, nil, nil
	}
	alarmNames := make([]string, 0, len(alarmIDs))
	for _, id := range alarmIDs {
		alarmNames = append(alarmNames, "Alarm/"+id)
	}
	response, err := s.client.LookupRecords(RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, alarmNames)
	if err != nil {
		return nil, nil, fmt.Errorf("lookup reminder alarms: %w", err)
	}
	remaining := make([]string, 0, len(alarmIDs))
	deletes := make([]RecordOperation, 0)
	for _, alarm := range response.Records {
		if err := recordError(alarm); err != nil {
			return nil, nil, fmt.Errorf("lookup reminder alarm: %w", err)
		}
		triggerID, _ := alarm.Fields["TriggerID"].Value.(string)
		if triggerID == "" {
			remaining = append(remaining, strings.TrimPrefix(alarm.RecordName, "Alarm/"))
			continue
		}
		triggers, err := s.client.LookupRecords(RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, []string{"AlarmTrigger/" + triggerID})
		if err != nil {
			return nil, nil, fmt.Errorf("lookup alarm trigger %s: %w", triggerID, err)
		}
		if len(triggers.Records) == 0 {
			return nil, nil, fmt.Errorf("lookup alarm trigger %s: no record returned", triggerID)
		}
		trigger := triggers.Records[0]
		if err := recordError(trigger); err != nil {
			return nil, nil, fmt.Errorf("lookup alarm trigger: %w", err)
		}
		if trigger.Fields["Type"].Value != "Date" {
			remaining = append(remaining, strings.TrimPrefix(alarm.RecordName, "Alarm/"))
			continue
		}
		deletes = append(deletes,
			RecordOperation{OperationType: OperationDelete, Record: Record{RecordName: trigger.RecordName, RecordType: "AlarmTrigger", RecordChangeTag: trigger.RecordChangeTag}},
			RecordOperation{OperationType: OperationDelete, Record: Record{RecordName: alarm.RecordName, RecordType: "Alarm", RecordChangeTag: alarm.RecordChangeTag}},
		)
	}
	return remaining, deletes, nil
}

// UpdateURLAttachment replaces or clears URL attachments while preserving
// image and other attachment types linked to the reminder.
func (s *RemindersService) UpdateURLAttachment(reminderID, rawURL string) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL != "" {
		parsed, err := url.ParseRequestURI(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("URL must be an absolute HTTP or HTTPS URL")
		}
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}

	attachmentIDs := fieldStringList(existing.Fields["AttachmentIDs"].Value)
	remainingIDs := make([]string, 0, len(attachmentIDs)+1)
	operations := make([]RecordOperation, 0, len(attachmentIDs)+2)
	if len(attachmentIDs) > 0 {
		names := make([]string, 0, len(attachmentIDs))
		for _, id := range attachmentIDs {
			names = append(names, "Attachment/"+id)
		}
		response, err := s.client.LookupRecords(RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, names)
		if err != nil {
			return fmt.Errorf("lookup reminder attachments: %w", err)
		}
		if len(response.Records) != len(names) {
			return fmt.Errorf("lookup reminder attachments: got %d records, want %d", len(response.Records), len(names))
		}
		for _, attachment := range response.Records {
			if err := recordError(attachment); err != nil {
				return fmt.Errorf("lookup reminder attachment: %w", err)
			}
			if attachment.Fields["Type"].Value == "URL" {
				operations = append(operations, RecordOperation{OperationType: OperationDelete, Record: Record{
					RecordName: attachment.RecordName, RecordType: "Attachment", RecordChangeTag: attachment.RecordChangeTag,
				}})
				continue
			}
			remainingIDs = append(remainingIDs, strings.TrimPrefix(attachment.RecordName, "Attachment/"))
		}
	}
	if rawURL == "" && len(operations) == 0 {
		return nil
	}

	if rawURL != "" {
		id := strings.ToUpper(uuid.New().String())
		remainingIDs = append(remainingIDs, id)
		operations = append(operations, RecordOperation{OperationType: OperationCreate, Record: Record{
			RecordName: "Attachment/" + id,
			RecordType: "Attachment",
			Parent:     &RecordReference{RecordName: reminderID},
			Fields: map[string]FieldValue{
				"Deleted":  {Value: int64(0), Type: "NUMBER_INT64"},
				"Imported": {Value: int64(0), Type: "NUMBER_INT64"},
				"Reminder": {Value: RecordReference{RecordName: reminderID, Action: "VALIDATE"}, Type: "REFERENCE"},
				"Type":     {Value: "URL", Type: "STRING"},
				"URL":      {Value: rawURL, Type: "STRING", IsEncrypted: true},
				"UTI":      {Value: "public.url", Type: "STRING"},
			},
		}})
	}

	parent := RecordOperation{OperationType: OperationUpdate, Record: Record{
		RecordName: reminderID, RecordType: "Reminder", RecordChangeTag: existing.RecordChangeTag,
		Fields: map[string]FieldValue{
			"AttachmentIDs":    {Value: remainingIDs, Type: "STRING_LIST"},
			"LastModifiedDate": {Value: time.Now().UnixMilli(), Type: "TIMESTAMP"},
		},
	}}
	operations = append([]RecordOperation{parent}, operations...)
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, ModifyRequest{
		ZoneID: s.zoneID, Operations: operations, Atomic: true,
	})
	if err != nil {
		return fmt.Errorf("update URL attachment: %w", err)
	}
	if len(response.Records) != len(operations) {
		return fmt.Errorf("update URL attachment: got %d records, want %d", len(response.Records), len(operations))
	}
	for _, record := range response.Records {
		if err := recordError(record); err != nil {
			return fmt.Errorf("update URL attachment: %w", err)
		}
	}
	s.records[reminderID] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

// UpdateRecurrence replaces or clears the reminder's simple recurrence rule.
func (s *RemindersService) UpdateRecurrence(reminderID string, recurrence *RecurrenceRule) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	if recurrence != nil {
		if recurrence.Frequency < 0 || recurrence.Frequency > 3 {
			return fmt.Errorf("recurrence frequency must be daily, weekly, monthly, or yearly")
		}
		if recurrence.Interval < 1 || recurrence.Interval > 999 {
			return fmt.Errorf("recurrence interval must be between 1 and 999")
		}
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	currentIDs := fieldStringList(existing.Fields["RecurrenceRuleIDs"].Value)
	operations := make([]RecordOperation, 0, len(currentIDs)+2)
	currentRules := make([]Record, 0, len(currentIDs))
	if len(currentIDs) > 0 {
		names := make([]string, 0, len(currentIDs))
		for _, id := range currentIDs {
			names = append(names, "RecurrenceRule/"+id)
		}
		response, err := s.client.LookupRecords(RemindersContainer, RemindersEnv, RemindersDB, s.zoneID, names)
		if err != nil {
			return fmt.Errorf("lookup recurrence rules: %w", err)
		}
		if len(response.Records) != len(names) {
			return fmt.Errorf("lookup recurrence rules: got %d records, want %d", len(response.Records), len(names))
		}
		for _, record := range response.Records {
			if err := recordError(record); err != nil {
				return fmt.Errorf("lookup recurrence rule: %w", err)
			}
			currentRules = append(currentRules, record)
		}
	}

	updateExisting := recurrence != nil && len(currentRules) == 1
	if updateExisting {
		rule := currentRules[0]
		fields := map[string]FieldValue{
			"Deleted":           {Value: int64(0), Type: "NUMBER_INT64"},
			"FirstDayOfTheWeek": {Value: int64(0), Type: "NUMBER_INT64"},
			"Frequency":         {Value: int64(recurrence.Frequency), Type: "NUMBER_INT64"},
			"Imported":          {Value: int64(0), Type: "NUMBER_INT64"},
			"Interval":          {Value: int64(recurrence.Interval), Type: "NUMBER_INT64"},
			"OccurrenceCount":   {Value: int64(0), Type: "NUMBER_INT64"},
		}
		if recurrence.EndDate != nil {
			endDate, err := recurrenceEndDate(existing, *recurrence.EndDate)
			if err != nil {
				return err
			}
			fields["EndDate"] = FieldValue{Value: endDate.UnixMilli(), Type: "TIMESTAMP"}
		} else if _, exists := rule.Fields["EndDate"]; exists {
			fields["EndDate"] = FieldValue{Value: nil, Type: "TIMESTAMP"}
		}
		operations = append(operations, RecordOperation{OperationType: OperationUpdate, Record: Record{
			RecordName: rule.RecordName, RecordType: "RecurrenceRule", RecordChangeTag: rule.RecordChangeTag,
			Fields: fields,
		}})
	} else {
		for _, record := range currentRules {
			if recurrence == nil {
				operations = append(operations, RecordOperation{OperationType: OperationDelete, Record: Record{
					RecordName: record.RecordName, RecordType: "RecurrenceRule", RecordChangeTag: record.RecordChangeTag,
				}})
				continue
			}
			operations = append(operations, RecordOperation{OperationType: OperationUpdate, Record: Record{
				RecordName: record.RecordName, RecordType: "RecurrenceRule", RecordChangeTag: record.RecordChangeTag,
				Fields: map[string]FieldValue{"Deleted": {Value: int64(1), Type: "NUMBER_INT64"}},
			}})
		}
	}
	if recurrence == nil && len(operations) == 0 {
		return nil
	}

	ids := make([]string, 0, 1)
	if updateExisting {
		ids = append(ids, currentIDs...)
	}
	if recurrence != nil && !updateExisting {
		if _, ok := existing.Fields["DueDate"]; !ok {
			return fmt.Errorf("recurring reminders require a due date")
		}
		id := strings.ToUpper(uuid.New().String())
		ids = append(ids, id)
		fields := map[string]FieldValue{
			"Deleted":           {Value: int64(0), Type: "NUMBER_INT64"},
			"FirstDayOfTheWeek": {Value: int64(0), Type: "NUMBER_INT64"},
			"Frequency":         {Value: int64(recurrence.Frequency), Type: "NUMBER_INT64"},
			"Imported":          {Value: int64(0), Type: "NUMBER_INT64"},
			"Interval":          {Value: int64(recurrence.Interval), Type: "NUMBER_INT64"},
			"OccurrenceCount":   {Value: int64(0), Type: "NUMBER_INT64"},
			"Reminder":          {Value: RecordReference{RecordName: reminderID, Action: "VALIDATE"}, Type: "REFERENCE"},
		}
		if recurrence.EndDate != nil {
			endDate, err := recurrenceEndDate(existing, *recurrence.EndDate)
			if err != nil {
				return err
			}
			fields["EndDate"] = FieldValue{Value: endDate.UnixMilli(), Type: "TIMESTAMP"}
		}
		operations = append(operations, RecordOperation{OperationType: OperationCreate, Record: Record{
			RecordName: "RecurrenceRule/" + id, RecordType: "RecurrenceRule",
			Parent: &RecordReference{RecordName: reminderID}, Fields: fields,
		}})
	}

	now := time.Now()
	parentFields := map[string]FieldValue{
		"RecurrenceRuleIDs": {Value: ids, Type: "STRING_LIST"},
		"LastModifiedDate":  {Value: now.UnixMilli(), Type: "TIMESTAMP"},
	}
	if err := addResolutionTokenUpdates(existing, parentFields); err != nil {
		return fmt.Errorf("update recurrence resolution token: %w", err)
	}
	parent := RecordOperation{OperationType: OperationUpdate, Record: Record{
		RecordName: reminderID, RecordType: "Reminder", RecordChangeTag: existing.RecordChangeTag, Fields: parentFields,
	}}
	if recurrence != nil && !updateExisting {
		operations = append(operations, parent)
	} else {
		operations = append([]RecordOperation{parent}, operations...)
	}
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, ModifyRequest{
		ZoneID: s.zoneID, Operations: operations, Atomic: true,
	})
	if err != nil {
		return fmt.Errorf("update recurrence: %w", err)
	}
	if len(response.Records) != len(operations) {
		return fmt.Errorf("update recurrence: got %d records, want %d", len(response.Records), len(operations))
	}
	for _, record := range response.Records {
		if err := recordError(record); err != nil {
			return fmt.Errorf("update recurrence: %w", err)
		}
	}
	for _, record := range response.Records {
		if record.RecordName == reminderID {
			s.records[reminderID] = mergeRecord(existing, record)
			break
		}
	}
	s.persistCache()
	return nil
}

func recurrenceEndDate(reminder Record, selected time.Time) (time.Time, error) {
	dueMillis, ok := numericInt64(reminder.Fields["DueDate"].Value)
	if !ok {
		return time.Time{}, fmt.Errorf("recurring reminders require a valid due date")
	}
	zoneName, _ := reminder.Fields["TimeZone"].Value.(string)
	location := time.UTC
	if zoneName != "" {
		loaded, err := time.LoadLocation(zoneName)
		if err != nil {
			return time.Time{}, fmt.Errorf("load reminder timezone %q: %w", zoneName, err)
		}
		location = loaded
	}
	due := time.UnixMilli(dueMillis).In(location)
	if fieldBool(reminder.Fields["AllDay"].Value) {
		return time.Date(selected.Year(), selected.Month(), selected.Day()+1, 0, 0, 0, 0, location).Add(-time.Minute), nil
	}
	return time.Date(selected.Year(), selected.Month(), selected.Day(), due.Hour(), due.Minute(), due.Second(), 0, location).Add(-time.Minute), nil
}

type dueDateDeltaAlertsEnvelope struct {
	ReminderIdentifier      string                  `json:"reminderIdentifier"`
	AccountIdentifier       string                  `json:"accountIdentifier"`
	DueDateDeltaAlerts      []dueDateDeltaAlertData `json:"dueDateDeltaAlerts"`
	MinimumSupportedVersion int                     `json:"minimumSupportedVersion"`
}

type dueDateDeltaAlertData struct {
	CreationDate               float64 `json:"creationDate"`
	DueDateDeltaCount          int     `json:"dueDateDeltaCount"`
	DueDateDeltaUnit           int     `json:"dueDateDeltaUnit"`
	Identifier                 string  `json:"identifier"`
	MinimumSupportedAppVersion int     `json:"minimumSupportedAppVersion"`
}

type urgentPresentationAlarmsEnvelope struct {
	MinimumSupportedVersion int                              `json:"minimumSupportedVersion"`
	Account                 []urgentPresentationAlarmAccount `json:"account"`
}

type urgentPresentationAlarmAccount struct {
	IsEnabled  bool    `json:"isEnabled"`
	ModifiedOn float64 `json:"modifiedOn"`
	PersonID   string  `json:"personID"`
}

// UpdateUrgentReminder enables or disables the alarm that bypasses Focus and
// silent mode when the reminder becomes due.
func (s *RemindersService) UpdateUrgentReminder(reminderID string, enabled bool) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	envelope, err := s.urgentPresentationEnvelope(existing)
	if err != nil {
		return err
	}
	modifiedOn := float64(time.Now().UnixMilli())/1000 - 978307200
	for index := range envelope.Account {
		envelope.Account[index].IsEnabled = enabled
		envelope.Account[index].ModifiedOn = modifiedOn
	}
	if envelope.MinimumSupportedVersion == 0 {
		envelope.MinimumSupportedVersion = 20251103
	}
	content, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode urgent reminder state: %w", err)
	}
	asset, err := s.client.UploadAsset(
		RemindersContainer, RemindersEnv, RemindersDB, s.zoneID,
		existing.RecordName, "Reminder", "UrgentPresentationAlarmsAsData", content,
	)
	if err != nil {
		return fmt.Errorf("upload urgent reminder state: %w", err)
	}
	digest := sha512.Sum512(content)
	fields := map[string]FieldValue{
		"UrgentPresentationAlarmsAsData": {Value: asset, Type: "ASSETID"},
		"UrgentPresentationAlarmsChecksum": {
			Value: hex.EncodeToString(digest[:]), Type: "STRING", IsEncrypted: true,
		},
	}
	if err := addResolutionTokenUpdates(existing, fields, "urgentPresentationAlarmsChecksum"); err != nil {
		return fmt.Errorf("update urgent reminder resolution token: %w", err)
	}
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{{OperationType: OperationUpdate, Record: Record{
			RecordName: existing.RecordName, RecordType: "Reminder", RecordChangeTag: existing.RecordChangeTag, Fields: fields,
		}}},
	})
	if err != nil {
		return fmt.Errorf("update urgent reminder: %w", err)
	}
	if len(response.Records) != 1 {
		return fmt.Errorf("update urgent reminder: got %d records, want 1", len(response.Records))
	}
	if err := recordError(response.Records[0]); err != nil {
		return fmt.Errorf("update urgent reminder: %w", err)
	}
	s.records[existing.RecordName] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

func (s *RemindersService) urgentPresentationEnvelope(existing Record) (urgentPresentationAlarmsEnvelope, error) {
	if envelope, ok := s.decodeUrgentPresentationEnvelope(existing); ok {
		return envelope, nil
	}
	for _, record := range s.records {
		if envelope, ok := s.decodeUrgentPresentationEnvelope(record); ok {
			return envelope, nil
		}
	}
	if err := s.Sync(true); err != nil {
		return urgentPresentationAlarmsEnvelope{}, fmt.Errorf("find urgent reminder account state: %w", err)
	}
	for _, record := range s.records {
		if envelope, ok := s.decodeUrgentPresentationEnvelope(record); ok {
			return envelope, nil
		}
	}
	return urgentPresentationAlarmsEnvelope{}, fmt.Errorf("cannot determine the Urgent alarm person identifier; enable Urgent once in Reminders and sync it first")
}

func (s *RemindersService) decodeUrgentPresentationEnvelope(record Record) (urgentPresentationAlarmsEnvelope, bool) {
	field, ok := record.Fields["UrgentPresentationAlarmsAsData"]
	if !ok {
		return urgentPresentationAlarmsEnvelope{}, false
	}
	value, ok := field.Value.(map[string]interface{})
	if !ok {
		return urgentPresentationAlarmsEnvelope{}, false
	}
	downloadURL, _ := value["downloadURL"].(string)
	if downloadURL == "" {
		return urgentPresentationAlarmsEnvelope{}, false
	}
	content, err := s.client.DownloadAsset(downloadURL)
	if err != nil {
		return urgentPresentationAlarmsEnvelope{}, false
	}
	var envelope urgentPresentationAlarmsEnvelope
	if json.Unmarshal(content, &envelope) != nil || len(envelope.Account) == 0 || envelope.Account[0].PersonID == "" {
		return urgentPresentationAlarmsEnvelope{}, false
	}
	return envelope, true
}

// UpdateEarlyReminder replaces or clears the reminder's due-date delta alert.
func (s *RemindersService) UpdateEarlyReminder(reminderID string, alert *EarlyReminder) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	if alert != nil {
		if alert.Unit < 0 || alert.Unit > 4 {
			return fmt.Errorf("early reminder unit must be minutes, hours, days, weeks, or months")
		}
		if alert.Count < 1 || alert.Count > 999 {
			return fmt.Errorf("early reminder count must be between 1 and 999")
		}
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	if alert != nil {
		if _, ok := existing.Fields["DueDate"]; !ok {
			return fmt.Errorf("early reminders require a due date")
		}
	} else if _, ok := existing.Fields["DueDateDeltaAlertsData"]; !ok {
		return nil
	}

	accountIdentifier, err := s.earlyReminderAccountIdentifier(existing)
	if err != nil {
		return err
	}
	envelope := dueDateDeltaAlertsEnvelope{
		ReminderIdentifier:      strings.TrimPrefix(existing.RecordName, "Reminder/"),
		AccountIdentifier:       accountIdentifier,
		DueDateDeltaAlerts:      []dueDateDeltaAlertData{},
		MinimumSupportedVersion: 20230430,
	}
	if alert != nil {
		envelope.DueDateDeltaAlerts = append(envelope.DueDateDeltaAlerts, dueDateDeltaAlertData{
			CreationDate:               float64(time.Now().UnixMilli())/1000 - 978307200,
			DueDateDeltaCount:          -alert.Count,
			DueDateDeltaUnit:           alert.Unit,
			Identifier:                 strings.ToUpper(uuid.New().String()),
			MinimumSupportedAppVersion: 0,
		})
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode early reminder: %w", err)
	}
	fields := map[string]FieldValue{
		"DueDateDeltaAlertsData": {Value: base64.StdEncoding.EncodeToString(raw), Type: "ENCRYPTED_BYTES"},
	}
	if err := addResolutionTokenUpdates(existing, fields, "dueDateDeltaAlertsData"); err != nil {
		return fmt.Errorf("update early reminder resolution token: %w", err)
	}
	response, err := s.client.ModifyRecords(RemindersContainer, RemindersEnv, RemindersDB, ModifyRequest{
		ZoneID: s.zoneID,
		Operations: []RecordOperation{{OperationType: OperationUpdate, Record: Record{
			RecordName: reminderID, RecordType: "Reminder", RecordChangeTag: existing.RecordChangeTag, Fields: fields,
		}}},
	})
	if err != nil {
		return fmt.Errorf("update early reminder: %w", err)
	}
	if len(response.Records) != 1 {
		return fmt.Errorf("update early reminder: got %d records, want 1", len(response.Records))
	}
	if err := recordError(response.Records[0]); err != nil {
		return fmt.Errorf("update early reminder: %w", err)
	}
	s.records[reminderID] = mergeRecord(existing, response.Records[0])
	s.persistCache()
	return nil
}

func (s *RemindersService) earlyReminderAccountIdentifier(existing Record) (string, error) {
	records := []Record{existing}
	for _, record := range s.records {
		if record.RecordName != existing.RecordName {
			records = append(records, record)
		}
	}
	for _, record := range records {
		field, ok := record.Fields["DueDateDeltaAlertsData"]
		if !ok {
			continue
		}
		encoded, ok := field.Value.(string)
		if !ok || encoded == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		var envelope dueDateDeltaAlertsEnvelope
		if json.Unmarshal(raw, &envelope) == nil && envelope.AccountIdentifier != "" {
			return envelope.AccountIdentifier, nil
		}
	}
	return "", fmt.Errorf("cannot determine the Reminders account identifier; create one native early reminder and sync it first")
}

func numericInt64(value interface{}) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, true
	case int:
		return int64(number), true
	case float64:
		return int64(number), true
	default:
		return 0, false
	}
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

// CompleteReminder submits the native completion fields used by Reminders.
func (s *RemindersService) CompleteReminder(reminderID string) error {
	if err := s.ensureZone(); err != nil {
		return err
	}
	existing, err := s.lookupReminder(reminderID)
	if err != nil {
		return err
	}
	fields := map[string]FieldValue{
		"Completed":      {Value: int64(1), Type: "NUMBER_INT64"},
		"CompletionDate": {Value: time.Now().UnixMilli(), Type: "TIMESTAMP"},
	}
	if err := addResolutionTokenUpdates(existing, fields, "completed", "completionDate"); err != nil {
		return fmt.Errorf("update completion resolution tokens: %w", err)
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
