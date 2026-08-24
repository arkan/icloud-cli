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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/config"
	"github.com/google/uuid"
)

func TestAddReminderUsesRemindersCloudKitContract(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			record := modify.Operations[0].Record
			response := RecordsResponse{Records: []Record{{
				RecordName:      record.RecordName,
				RecordType:      record.RecordType,
				RecordChangeTag: "change-1",
				Fields:          record.Fields,
			}}}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")

	created, err := service.AddReminder("Café 🛒", "", "LIST-123", 0, nil)
	if err != nil {
		t.Fatalf("AddReminder: %v", err)
	}

	if modify.ZoneID != (ZoneID{ZoneName: RemindersZone, OwnerRecordName: "owner-123"}) {
		t.Fatalf("zone ID = %#v", modify.ZoneID)
	}
	if len(modify.Operations) != 1 || modify.Operations[0].OperationType != "create" {
		t.Fatalf("operations = %#v", modify.Operations)
	}
	record := modify.Operations[0].Record
	if !strings.HasPrefix(record.RecordName, "Reminder/") || record.RecordName != "Reminder/"+strings.ToUpper(strings.TrimPrefix(record.RecordName, "Reminder/")) {
		t.Errorf("record name should use the native Reminder/UUID format, got %q", record.RecordName)
	}
	if created.ID != record.RecordName {
		t.Errorf("created ID = %q, want %q", created.ID, record.RecordName)
	}
	if created.Title != "Café 🛒" {
		t.Errorf("created title = %q", created.Title)
	}
	if record.Parent != nil {
		t.Errorf("create payload should not set a parent record: %#v", record.Parent)
	}
	list := record.Fields["List"]
	if list.Type != "" {
		t.Errorf("List field should not force a CloudKit type, got %q", list.Type)
	}
	listValue, _ := list.Value.(map[string]interface{})
	if listValue["recordName"] != "LIST-123" || listValue["action"] != "NONE" {
		t.Errorf("List reference = %#v", listValue)
	}
	titleField := record.Fields["TitleDocument"]
	if titleField.Type != "" {
		t.Errorf("TitleDocument should not force a CloudKit type, got %q", titleField.Type)
	}
	assertCRDTDocumentContains(t, titleField.Value.(string), "Café 🛒")
	assertMergeableStringLengths(t, titleField.Value.(string))
	for _, fieldName := range []string{"Completed", "CreationDate", "LastModifiedDate", "Priority", "Flagged", "AllDay", "List", "ResolutionTokenMap"} {
		if _, ok := record.Fields[fieldName]; !ok {
			t.Errorf("create payload is missing native field %q", fieldName)
		}
	}
	for _, fieldName := range []string{"Completed", "Priority", "Flagged", "AllDay"} {
		if field := record.Fields[fieldName]; field.Type != "NUMBER_INT64" {
			t.Errorf("%s field = %#v", fieldName, field)
		}
	}
	for _, fieldName := range []string{"CreationDate", "LastModifiedDate"} {
		if field := record.Fields[fieldName]; field.Type != "TIMESTAMP" {
			t.Errorf("%s field = %#v", fieldName, field)
		}
	}
	tokens := resolutionTokens(t, record.Fields)
	for _, key := range []string{"titleDocument", "priority", "flagged", "allDay", "list", "completed", "creationDate", "lastModifiedDate", "minimumSupportedVersion", "icsDisplayOrder"} {
		if tokens[key] == nil {
			t.Errorf("create payload is missing resolution token %q", key)
		}
	}
}

func TestGetReminderRecordReturnsExactCloudKitRecord(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"change","fields":{"Flagged":{"value":1,"type":"NUMBER_INT64"}}}]}`)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	record, err := newRemindersService(client, "").GetReminderRecord("Reminder/R1")
	if err != nil {
		t.Fatal(err)
	}
	if record.RecordName != "Reminder/R1" || record.RecordChangeTag != "change" || !fieldBool(record.Fields["Flagged"].Value) {
		t.Fatalf("raw record = %#v", record)
	}
}

func TestGetReminderPropertiesResolvesLinkedRecords(t *testing.T) {
	t.Parallel()
	earlyData, err := json.Marshal(dueDateDeltaAlertsEnvelope{DueDateDeltaAlerts: []dueDateDeltaAlertData{{DueDateDeltaCount: -15, DueDateDeltaUnit: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/zones/list") {
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner"}}]}`)
			return
		}
		if !strings.Contains(r.URL.Path, "/records/lookup") {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var request LookupRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		name := request.Records[0].RecordName
		var record Record
		switch {
		case name == "Reminder/R1":
			record = Record{RecordName: name, RecordType: "Reminder", Fields: map[string]FieldValue{
				"AttachmentIDs": {Value: []interface{}{"A1"}}, "HashtagIDs": {Value: []interface{}{"H1"}},
				"AssignmentIDs": {Value: []interface{}{"S1"}}, "RecurrenceRuleIDs": {Value: []interface{}{"RR1"}},
				"AlarmIDs": {Value: []interface{}{"AL1"}}, "TimeZone": {Value: "Europe/Paris"}, "AllDay": {Value: float64(0)},
				"DueDateDeltaAlertsData": {Value: base64.StdEncoding.EncodeToString(earlyData)},
			}}
		case strings.HasPrefix(name, "Attachment/"):
			record = Record{RecordName: name, Fields: map[string]FieldValue{"Type": {Value: "URL"}, "URL": {Value: "https://example.com"}}}
		case strings.HasPrefix(name, "Hashtag/"):
			record = Record{RecordName: name, Fields: map[string]FieldValue{"Name": {Value: "work"}}}
		case strings.HasPrefix(name, "Assignment/"):
			record = Record{RecordName: name, Fields: map[string]FieldValue{"EncryptedAssigneeIdentifier": {Value: "participant"}}}
		case strings.HasPrefix(name, "RecurrenceRule/"):
			record = Record{RecordName: name, Fields: map[string]FieldValue{"Frequency": {Value: float64(1)}, "Interval": {Value: float64(2)}}}
		case strings.HasPrefix(name, "Alarm/"):
			record = Record{RecordName: name, Fields: map[string]FieldValue{"TriggerID": {Value: "T1"}}}
		case strings.HasPrefix(name, "AlarmTrigger/"):
			record = Record{RecordName: name, Fields: map[string]FieldValue{"Type": {Value: "Location"}, "Title": {Value: "Office"}, "Address": {Value: "1 Main St"}, "Latitude": {Value: 48.0}, "Longitude": {Value: 2.0}, "Radius": {Value: 100.0}, "Proximity": {Value: float64(1)}}}
		}
		record.RecordChangeTag = "change"
		_ = json.NewEncoder(w).Encode(RecordsResponse{Records: []Record{record}})
	}))
	defer server.Close()
	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	properties, err := newRemindersService(client, "").GetReminderProperties("Reminder/R1")
	if err != nil {
		t.Fatal(err)
	}
	if properties.URL != "https://example.com" || len(properties.Tags) != 1 || properties.Tags[0] != "work" || properties.Assignee != "participant" {
		t.Fatalf("linked properties = %#v", properties)
	}
	if properties.Recurrence == nil || properties.Recurrence.Frequency != 1 || properties.Recurrence.Interval != 2 || properties.EarlyReminder == nil || properties.EarlyReminder.Count != 15 {
		t.Fatalf("scheduled properties = %#v", properties)
	}
	if properties.Location == nil || properties.Location.Title != "Office" || properties.Location.Proximity != 1 {
		t.Fatalf("location = %#v", properties.Location)
	}
}

func TestAddReminderWithParentUsesParentsList(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/PARENT","recordType":"Reminder","recordChangeTag":"parent-change","fields":{"List":{"value":{"recordName":"List/SHOPPING","action":"NONE"}}}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			record := modify.Operations[0].Record
			_ = json.NewEncoder(w).Encode(RecordsResponse{Records: []Record{record}})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	created, err := service.AddReminderWithParent("Child", "", "", 0, nil, "Reminder/PARENT")
	if err != nil {
		t.Fatalf("AddReminderWithParent: %v", err)
	}

	record := modify.Operations[0].Record
	if created.ParentID != "Reminder/PARENT" {
		t.Errorf("created parent ID = %q", created.ParentID)
	}
	parent, ok := record.Fields["ParentReminder"].Value.(map[string]interface{})
	if !ok || parent["recordName"] != "Reminder/PARENT" || parent["action"] != "NONE" {
		t.Errorf("parent reference = %#v", record.Fields["ParentReminder"])
	}
	list, ok := record.Fields["List"].Value.(map[string]interface{})
	if !ok || list["recordName"] != "List/SHOPPING" {
		t.Errorf("list reference = %#v", record.Fields["List"])
	}
}

func TestFetchChangesUsesZoneEndpointAndOwner(t *testing.T) {
	t.Parallel()

	var request ZoneChangesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/changes/zone") {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode changes request: %v", err)
		}
		_, _ = io.WriteString(w, `{"zones":[{"records":[{"recordName":"R1","recordType":"Reminder","recordChangeTag":"c1"}],"syncToken":"next","moreComing":false}]}`)
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	zone := ZoneID{ZoneName: RemindersZone, OwnerRecordName: "owner-123"}
	changes, err := client.FetchChanges(RemindersContainer, RemindersEnv, RemindersDB, zone, "previous")
	if err != nil {
		t.Fatalf("FetchChanges: %v", err)
	}

	if len(request.Zones) != 1 || request.Zones[0].ZoneID != zone || request.Zones[0].SyncToken != "previous" {
		t.Errorf("request = %#v", request)
	}
	desired := make(map[string]bool)
	for _, key := range request.Zones[0].DesiredKeys {
		desired[key] = true
	}
	if !desired["DueDateDeltaAlertsData"] {
		t.Errorf("FetchChanges desired keys omit DueDateDeltaAlertsData: %#v", request.Zones[0].DesiredKeys)
	}
	if len(changes.Records) != 1 || changes.Records[0].RecordChangeTag != "c1" || changes.SyncToken != "next" {
		t.Errorf("changes = %#v", changes)
	}
}

func TestReminderMutationsUseExactRecordNameAndChangeTag(t *testing.T) {
	t.Parallel()

	var operations []RecordOperation
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			_, _ = fmt.Fprintf(w, `{"records":[{"recordName":"ABC-123","recordType":"Reminder","recordChangeTag":"change-%d","fields":{"ResolutionTokenMap":{"value":"{\"map\":{\"titleDocument\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"},\"notesDocument\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"},\"priority\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"},\"flagged\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"},\"completed\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"},\"completionDate\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"},\"lastModifiedDate\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"}}}"}}}]}`, lookupCount+6)
		case strings.Contains(r.URL.Path, "/records/modify"):
			var request ModifyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			operations = append(operations, request.Operations...)
			_ = json.NewEncoder(w).Encode(RecordsResponse{Records: []Record{{RecordName: "ABC-123", RecordType: "Reminder", RecordChangeTag: "change-8"}}})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	priority := 5
	title := "Experimental edit"
	notes := "Experimental notes"
	flagged := true
	if err := service.UpdateReminder("ABC-123", ReminderChanges{Title: &title, Notes: &notes, Priority: &priority, Flagged: &flagged}); err != nil {
		t.Fatalf("UpdateReminder: %v", err)
	}
	if err := service.CompleteReminder("ABC-123"); err != nil {
		t.Fatalf("CompleteReminder: %v", err)
	}
	if err := service.DeleteReminder("ABC-123"); err != nil {
		t.Fatalf("DeleteReminder: %v", err)
	}

	if len(operations) != 3 {
		t.Fatalf("operations = %#v", operations)
	}
	edit := operations[0]
	if edit.OperationType != "update" || edit.Record.RecordName != "ABC-123" || edit.Record.RecordChangeTag != "change-7" {
		t.Errorf("edit operation = %#v", edit)
	}
	if edit.Record.Fields["Priority"].Value != float64(priority) {
		t.Errorf("priority field = %#v", edit.Record.Fields["Priority"])
	}
	assertCRDTDocumentContains(t, edit.Record.Fields["TitleDocument"].Value.(string), title)
	assertCRDTDocumentContains(t, edit.Record.Fields["NotesDocument"].Value.(string), notes)
	if edit.Record.Fields["Flagged"].Value != float64(1) || edit.Record.Fields["Flagged"].Type != "NUMBER_INT64" {
		t.Errorf("flagged field = %#v", edit.Record.Fields["Flagged"])
	}
	tokens := resolutionTokens(t, edit.Record.Fields)
	for _, key := range []string{"titleDocument", "notesDocument", "priority", "flagged", "lastModifiedDate"} {
		if counter := tokens[key]["counter"]; counter != float64(3) {
			t.Errorf("resolution token %q counter = %#v, want 3", key, counter)
		}
	}
	if edit.Record.Fields["LastModifiedDate"].Type != "TIMESTAMP" {
		t.Errorf("last modified field = %#v", edit.Record.Fields["LastModifiedDate"])
	}
	complete := operations[1]
	if complete.Record.Fields["Completed"].Value != float64(1) {
		t.Errorf("complete fields = %#v", complete.Record.Fields)
	}
	completeTokens := resolutionTokens(t, complete.Record.Fields)
	for _, key := range []string{"completed", "completionDate", "lastModifiedDate"} {
		if counter := completeTokens[key]["counter"]; counter != float64(3) {
			t.Errorf("completion token %q counter = %#v, want 3", key, counter)
		}
	}
	if complete.Record.Fields["LastModifiedDate"].Type != "TIMESTAMP" {
		t.Errorf("completion last modified field = %#v", complete.Record.Fields["LastModifiedDate"])
	}
	deleteOp := operations[2]
	if deleteOp.OperationType != "delete" || deleteOp.Record.RecordName != "ABC-123" || deleteOp.Record.RecordChangeTag != "change-9" {
		t.Errorf("delete operation = %#v", deleteOp)
	}
	if len(deleteOp.Record.Fields) != 0 {
		t.Errorf("delete should not emulate a soft deletion: %#v", deleteOp.Record.Fields)
	}
	if lookupCount != 3 {
		t.Errorf("lookup count = %d, want a fresh change tag for every mutation", lookupCount)
	}
}

func TestReplaceDocumentTextTombstonesOldContent(t *testing.T) {
	existing, err := encodeTitleDocument("old text")
	if err != nil {
		t.Fatal(err)
	}

	replicaID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	replaced, err := replaceDocumentText(existing, "Café replacement 🛒", replicaID)
	if err != nil {
		t.Fatal(err)
	}
	replacedRaw := decodeCRDTTestDocument(t, replaced)
	if bytes.Contains(replacedRaw, []byte("old text")) {
		t.Fatalf("replacement retained old text: %x", replacedRaw)
	}
	if !bytes.Contains(replacedRaw, []byte("Café replacement 🛒")) {
		t.Fatalf("replacement is missing new text: %x", replacedRaw)
	}
	assertMergeableStringLengths(t, replaced)
	wrapper, ok := protobufBytesField(replacedRaw, 2)
	if !ok {
		t.Fatal("missing document wrapper")
	}
	note, ok := protobufBytesField(wrapper, 3)
	if !ok {
		t.Fatal("missing CRDT string")
	}
	fields, err := decodeProtobufFields(note)
	if err != nil {
		t.Fatal(err)
	}
	var substrings []protobufField
	metadataEntries := 0
	var newReplicaTimestamp uint64
	for _, field := range fields {
		if field.number == 3 {
			substrings = append(substrings, field)
		}
		if field.number == 4 {
			metadata, err := decodeProtobufFields(field.payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range metadata {
				if entry.number == 1 {
					metadataEntries++
					entryFields, err := decodeProtobufFields(entry.payload)
					if err != nil {
						t.Fatal(err)
					}
					entryUUID, _ := protobufBytesTestField(entryFields, 1)
					isCLIReplica := bytes.Equal(entryUUID, replicaID[:])
					clockIndex := 0
					for _, entryField := range entryFields {
						if entryField.number == 2 {
							clockIndex++
							if clockIndex == 2 {
								clock, err := decodeProtobufFields(entryField.payload)
								if err != nil {
									t.Fatal(err)
								}
								if isCLIReplica {
									newReplicaTimestamp = protobufVarintTestField(clock, 1)
								}
							}
						}
					}
				}
			}
		}
	}
	if len(substrings) != 4 {
		t.Fatalf("substring count = %d, want 4", len(substrings))
	}
	oldSubstring, err := decodeProtobufFields(substrings[2].payload)
	if err != nil {
		t.Fatal(err)
	}
	if !protobufBoolField(oldSubstring, 4) {
		t.Fatal("old live substring was not tombstoned")
	}
	tombstone, ok := protobufBytesTestField(oldSubstring, 3)
	if !ok {
		t.Fatal("old live substring has no tombstone timestamp")
	}
	tombstoneFields, err := decodeProtobufFields(tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if replica := protobufVarintTestField(tombstoneFields, 1); replica != 2 {
		t.Fatalf("tombstone author replica = %d, want 2", replica)
	}
	oldCharID, ok := protobufBytesTestField(oldSubstring, 1)
	if !ok {
		t.Fatal("old substring has no character ID")
	}
	oldCharFields, err := decodeProtobufFields(oldCharID)
	if err != nil {
		t.Fatal(err)
	}
	if replica := protobufVarintTestField(oldCharFields, 1); replica != 1 {
		t.Fatalf("existing character replica = %d, want 1", replica)
	}
	newSubstring, err := decodeProtobufFields(substrings[1].payload)
	if err != nil {
		t.Fatal(err)
	}
	if length := protobufVarintTestField(newSubstring, 2); length != 19 {
		t.Fatalf("new substring UTF-16 length = %d, want 19", length)
	}
	newCharID, ok := protobufBytesTestField(newSubstring, 1)
	if !ok {
		t.Fatal("new substring has no character ID")
	}
	newCharFields, err := decodeProtobufFields(newCharID)
	if err != nil {
		t.Fatal(err)
	}
	if replica, clock := protobufVarintTestField(newCharFields, 1), protobufVarintTestField(newCharFields, 2); replica != 2 || clock != 0 {
		t.Fatalf("new character ID = (%d,%d), want (2,0)", replica, clock)
	}
	if metadataEntries != 2 {
		t.Fatalf("metadata replica count = %d, want 2", metadataEntries)
	}
	if newReplicaTimestamp != 3 {
		t.Fatalf("new replica timestamp = %d, want 3", newReplicaTimestamp)
	}

	// Simulate another device publishing at the same logical timestamp. The
	// next CLI edit must advance beyond it or Reminders may keep that device's
	// value, as happens when title and notes replicas race during sync.
	replaced = setDocumentReplicaTimestamp(t, replaced, 0, 3)
	second, err := replaceDocumentText(replaced, "next", replicaID)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw := decodeCRDTTestDocument(t, second)
	secondWrapper, _ := protobufBytesField(secondRaw, 2)
	secondNote, _ := protobufBytesField(secondWrapper, 3)
	secondFields, err := decodeProtobufFields(secondNote)
	if err != nil {
		t.Fatal(err)
	}
	var secondSubstrings []protobufField
	secondReplicaCount := 0
	for _, field := range secondFields {
		if field.number == 3 {
			secondSubstrings = append(secondSubstrings, field)
		}
		if field.number == 4 {
			secondMetadata, _ := decodeProtobufFields(field.payload)
			for _, entry := range secondMetadata {
				if entry.number == 1 {
					secondReplicaCount++
				}
			}
		}
	}
	if secondReplicaCount != 2 {
		t.Fatalf("second edit replica count = %d, want 2", secondReplicaCount)
	}
	latest, err := decodeProtobufFields(secondSubstrings[1].payload)
	if err != nil {
		t.Fatal(err)
	}
	latestCharID, _ := protobufBytesTestField(latest, 1)
	latestCharFields, _ := decodeProtobufFields(latestCharID)
	if replica, clock := protobufVarintTestField(latestCharFields, 1), protobufVarintTestField(latestCharFields, 2); replica != 2 || clock != 19 {
		t.Fatalf("second edit character ID = (%d,%d), want (2,19)", replica, clock)
	}
	previousLive, err := decodeProtobufFields(secondSubstrings[2].payload)
	if err != nil {
		t.Fatal(err)
	}
	previousTimestamp, _ := protobufBytesTestField(previousLive, 3)
	previousTimestampFields, _ := decodeProtobufFields(previousTimestamp)
	if clock := protobufVarintTestField(previousTimestampFields, 2); clock != 4 {
		t.Fatalf("second edit tombstone timestamp = %d, want 4", clock)
	}
}

const nativeEditedTitleDocumentFixture = `H4sIAAAAAAAAE+NgEFrByMEgwCC1kFFI29nfJUI3ONLPWdfIwMjMwMLIBMgwNtd1UnAMCFDISyzJLEtVSE3JLElNkRLgYgHpA+oE0xqMYBFGAVUBbSkQzaDBJCUEFmEQUAWLcCgwajBLiXFxANX/BwJ+oF44W0mGS4pLYIHeHvYVy10FZqsocUxe7RIjxMQRAMScWiwc2hoMAHFbZgutAAAA`

const corruptEmojiTitleDocumentFixture = `H4sIAAAAAAAA/+JgEOpl5GAQYJBqYxRSy0zOyS9N0U3OyVTIzCtJTS9KLMnMz1MwNLcwN7E0NzEyUfgwf/YkKQEuFpAWAQYpMK3BCBZhFGAQUJYC0xpMUmJcHBwMAv/////PL8AgBWcryXBJcQk8uTbj9ZZcj03LknZcuVt6tFCIiUNZiImDUYuJQxkQAAD//03sRmmSAAAA`

const corruptConcatenatedTitleDocumentFixture = `H4sIAAAAAAAAE+NgENrOzMEgwCC1gVlI3znIJUQhL7EksyxVoSi1ICcxOTU3Na/EDyJSUJSfm1+SmqKQlFicmpOZlyolwsUC0gvUDaY1GDWYNdilBICizAJhAuJSIJpBg0lKCCjCCFSnCBbhVGDU4ASrYhPQE5CQAtEMGixgVWwCkgKiYBGQKlawGCtQpxRYjAMoxgYWYwKKiUiB5BjBpgmB7dQT0IDbwAEWYwGq0wOLcYDViXFxAF36Hwj4ga6Gs5VWMHJJcQlwHM7jcf3tvm5NlXn3oxVXAoWYOBSBGCwXIaa4iCndWWtfaftT9dj7zEBxKSBmAskliOzZ5Mble7/l8QUe16D7yUDxXCDmAslV1RiXZOr5CyyL3y+/2SE+HSiuBzNzgd4e9hXLXQVmqyhxTF7tEoNs5rlv6z83GPkI9XGnPVkSdXoKUNwNZKYWE4e4FguHhAYDAO+d8C28AQAA`

func TestReplaceDocumentTextAcceptsNativeEditedFixture(t *testing.T) {
	replicaID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	replaced, err := replaceDocumentText(nativeEditedTitleDocumentFixture, "CLI replacement 🛒", replicaID)
	if err != nil {
		t.Fatal(err)
	}
	raw := decodeCRDTTestDocument(t, replaced)
	if bytes.Contains(raw, []byte("APP native edited")) {
		t.Fatalf("replacement retained native live text: %x", raw)
	}
	if !bytes.Contains(raw, []byte("CLI replacement 🛒")) {
		t.Fatalf("replacement is missing new text: %x", raw)
	}
	assertMergeableStringLengths(t, replaced)
	wrapper, _ := protobufBytesField(raw, 2)
	note, _ := protobufBytesField(wrapper, 3)
	fields, err := decodeProtobufFields(note)
	if err != nil {
		t.Fatal(err)
	}
	var liveCharacterClock uint64
	var replicaCharacterClock uint64
	for _, field := range fields {
		switch field.number {
		case 3:
			substring, _ := decodeProtobufFields(field.payload)
			characterID, _ := protobufBytesTestField(substring, 1)
			characterFields, _ := decodeProtobufFields(characterID)
			if protobufVarintTestField(characterFields, 1) == 2 && !protobufBoolField(substring, 4) {
				liveCharacterClock = protobufVarintTestField(characterFields, 2)
			}
		case 4:
			metadata, _ := decodeProtobufFields(field.payload)
			for _, metadataField := range metadata {
				entry, _ := decodeProtobufFields(metadataField.payload)
				entryUUID, _ := protobufBytesTestField(entry, 1)
				if !bytes.Equal(entryUUID, replicaID[:]) {
					continue
				}
				for _, entryField := range entry {
					if entryField.number == 2 {
						clock, _ := decodeProtobufFields(entryField.payload)
						replicaCharacterClock = protobufVarintTestField(clock, 1)
						break
					}
				}
			}
		}
	}
	if liveCharacterClock != 0 {
		t.Fatalf("new replica character clock = %d, want 0", liveCharacterClock)
	}
	if replicaCharacterClock != 18 {
		t.Fatalf("new replica vector clock = %d, want 18", replicaCharacterClock)
	}
}

func TestReplaceDocumentTextRepairsCapturedCorruptEmojiFixture(t *testing.T) {
	textLength, liveLength, attributeLength := mergeableStringLengths(t, corruptEmojiTitleDocumentFixture)
	if textLength != 36 || liveLength != 35 || attributeLength != 35 {
		t.Fatalf("captured corrupt lengths: text=%d live=%d attribute=%d", textLength, liveLength, attributeLength)
	}
	replicaID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	repaired, err := replaceDocumentText(
		corruptEmojiTitleDocumentFixture,
		"icloud-cli integration 1787497424 🛒",
		replicaID,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertMergeableStringLengths(t, repaired)
}

func TestReplaceDocumentTextRepairsCapturedConcatenatedFixture(t *testing.T) {
	textLength, liveLength, attributeLength := mergeableStringLengths(t, corruptConcatenatedTitleDocumentFixture)
	if textLength == liveLength && textLength == attributeLength {
		t.Fatalf("captured concatenated fixture unexpectedly valid: text=%d live=%d attribute=%d", textLength, liveLength, attributeLength)
	}
	replicaID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	repaired, err := replaceDocumentText(
		corruptConcatenatedTitleDocumentFixture,
		"CRDT native replacement",
		replicaID,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertMergeableStringLengths(t, repaired)
}

func TestEncodeTitleDocumentUsesUTF16CharacterCounts(t *testing.T) {
	encoded, err := encodeTitleDocument("🛒")
	if err != nil {
		t.Fatal(err)
	}
	raw := decodeCRDTTestDocument(t, encoded)
	wrapper, ok := protobufBytesField(raw, 2)
	if !ok {
		t.Fatal("missing document wrapper")
	}
	note, ok := protobufBytesField(wrapper, 3)
	if !ok {
		t.Fatal("missing note")
	}
	fields, err := decodeProtobufFields(note)
	if err != nil {
		t.Fatal(err)
	}
	var substrings [][]byte
	for _, field := range fields {
		if field.number == 3 && field.wire == 2 {
			substrings = append(substrings, field.payload)
		}
	}
	if len(substrings) < 2 {
		t.Fatalf("substring count = %d", len(substrings))
	}
	live, err := decodeProtobufFields(substrings[1])
	if err != nil {
		t.Fatal(err)
	}
	if length := protobufVarintTestField(live, 2); length != 2 {
		t.Fatalf("UTF-16 character length = %d, want 2", length)
	}
}

func setDocumentReplicaTimestamp(t *testing.T, encoded string, replicaIndex int, value uint64) string {
	t.Helper()
	raw := decodeCRDTTestDocument(t, encoded)
	rewritten, replaced, err := rewriteBytesField(raw, 2, func(wrapper []byte) ([]byte, error) {
		return rewriteRequiredBytesField(wrapper, 3, func(note []byte) ([]byte, error) {
			fields, err := decodeProtobufFields(note)
			if err != nil {
				return nil, err
			}
			for index := range fields {
				if fields[index].number != 4 || fields[index].wire != 2 {
					continue
				}
				metadata, err := decodeProtobufFields(fields[index].payload)
				if err != nil {
					return nil, err
				}
				seen := 0
				for metadataIndex := range metadata {
					if metadata[metadataIndex].number != 1 || metadata[metadataIndex].wire != 2 {
						continue
					}
					if seen != replicaIndex {
						seen++
						continue
					}
					entry, err := decodeProtobufFields(metadata[metadataIndex].payload)
					if err != nil {
						return nil, err
					}
					clockIndex := 0
					for entryIndex := range entry {
						if entry[entryIndex].number != 2 || entry[entryIndex].wire != 2 {
							continue
						}
						clockIndex++
						if clockIndex == 2 {
							entry[entryIndex].payload = encodeField(1, 0, value)
						}
					}
					metadata[metadataIndex].payload = encodeProtobufFields(entry)
					fields[index].payload = encodeProtobufFields(metadata)
					return encodeProtobufFields(fields), nil
				}
			}
			return nil, fmt.Errorf("replica %d not found", replicaIndex)
		})
	})
	if err != nil || !replaced {
		t.Fatalf("set replica timestamp: replaced=%v err=%v", replaced, err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(rewritten); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(compressed.Bytes())
}

func resolutionTokens(t *testing.T, fields map[string]FieldValue) map[string]map[string]interface{} {
	t.Helper()
	var envelope struct {
		Map map[string]map[string]interface{} `json:"map"`
	}
	value, ok := fields["ResolutionTokenMap"].Value.(string)
	if !ok {
		t.Fatalf("ResolutionTokenMap = %#v", fields["ResolutionTokenMap"])
	}
	if err := json.Unmarshal([]byte(value), &envelope); err != nil {
		t.Fatalf("decode ResolutionTokenMap: %v", err)
	}
	return envelope.Map
}

func TestResolutionTokenRepairAdvancesPastSurvivingClock(t *testing.T) {
	t.Parallel()

	title, err := encodeTitleDocument("Edited title")
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		RecordName: "Reminder/R1",
		Created:    &Timestamp{Timestamp: 1787596760936},
		Fields: map[string]FieldValue{
			"TitleDocument":      {Value: title},
			"Completed":          {Value: int64(1), Type: "NUMBER_INT64"},
			"List":               {Value: RecordReference{RecordName: "List/FP"}},
			"ResolutionTokenMap": {Value: `{"map":{"completed":{"counter":2,"modificationTime":100,"replicaID":"survivor"},"lastModifiedDate":{"counter":2,"modificationTime":100,"replicaID":"survivor"}}}`},
		},
	}
	fields := map[string]FieldValue{"TitleDocument": {Value: title}}
	if err := addResolutionTokenUpdates(record, fields, "titleDocument"); err != nil {
		t.Fatal(err)
	}
	tokens := resolutionTokens(t, fields)
	if counter := tokens["titleDocument"]["counter"]; counter != float64(4) {
		t.Errorf("repaired title counter = %#v, want 4", counter)
	}
	for _, key := range []string{"list", "creationDate"} {
		if counter := tokens[key]["counter"]; counter != float64(3) {
			t.Errorf("repaired %s counter = %#v, want 3", key, counter)
		}
	}
	if counter := tokens["lastModifiedDate"]["counter"]; counter != float64(3) {
		t.Errorf("lastModifiedDate counter = %#v, want 3", counter)
	}
	if field := fields["CreationDate"]; field.Value != int64(1787596760936) || field.Type != "TIMESTAMP" {
		t.Errorf("repaired CreationDate = %#v", field)
	}
	if field := fields["ResolutionTokenMap"]; field.Type != "STRING" {
		t.Errorf("repaired ResolutionTokenMap = %#v", field)
	}
}

const nativeResolutionTokenMapFixture = `{"map":{"titleDocument":{"counter":7,"modificationTime":100,"replicaID":"native-device"},"list":{"counter":4,"modificationTime":90,"replicaID":"native-device"},"completed":{"counter":3,"modificationTime":80,"replicaID":"native-device"},"lastModifiedDate":{"counter":9,"modificationTime":110,"replicaID":"native-device"}}}`

func assertNativeResolutionTokensPreserved(t *testing.T, fields map[string]FieldValue) {
	t.Helper()
	tokens := resolutionTokens(t, fields)
	for key, counter := range map[string]float64{"titleDocument": 7, "list": 4, "completed": 3} {
		if token := tokens[key]; token == nil || token["counter"] != counter || token["replicaID"] != "native-device" {
			t.Errorf("resolution token %q was not preserved: %#v", key, token)
		}
	}
	if token := tokens["lastModifiedDate"]; token == nil || token["counter"] != float64(10) || token["replicaID"] != "native-device" {
		t.Errorf("lastModifiedDate resolution token = %#v", token)
	}
}

func TestCreateTagUsesAtomicLinkedChildContract(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = fmt.Fprintf(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"change-1","fields":{"HashtagIDs":{"value":[],"type":"EMPTY_LIST"},"ResolutionTokenMap":{"value":%q,"type":"STRING"}}}]}`, nativeResolutionTokenMapFixture)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateTags("Reminder/R1", []string{"#café"}, nil); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 2 {
		t.Fatalf("modify request = %#v", modify)
	}
	reminder := modify.Operations[0]
	if reminder.OperationType != OperationUpdate || reminder.Record.RecordChangeTag != "change-1" {
		t.Errorf("reminder operation = %#v", reminder)
	}
	ids := fieldStringList(reminder.Record.Fields["HashtagIDs"].Value)
	if len(ids) != 1 {
		t.Fatalf("hashtag IDs = %#v", ids)
	}
	var tokens struct {
		Map map[string]interface{} `json:"map"`
	}
	if err := json.Unmarshal([]byte(reminder.Record.Fields["ResolutionTokenMap"].Value.(string)), &tokens); err != nil {
		t.Fatal(err)
	}
	if tokens.Map["hashtagIDs"] == nil || tokens.Map["lastModifiedDate"] == nil {
		t.Errorf("resolution tokens = %#v", tokens.Map)
	}
	assertNativeResolutionTokensPreserved(t, reminder.Record.Fields)
	child := modify.Operations[1]
	if child.OperationType != OperationCreate || child.Record.RecordName != "Hashtag/"+ids[0] {
		t.Errorf("child operation = %#v", child)
	}
	if child.Record.Parent == nil || child.Record.Parent.RecordName != "Reminder/R1" {
		t.Errorf("child parent = %#v", child.Record.Parent)
	}
	name := child.Record.Fields["Name"]
	if name.Type != "STRING" || name.Value != "café" || !name.IsEncrypted {
		t.Errorf("tag name = %#v", name)
	}
	reference, _ := child.Record.Fields["Reminder"].Value.(map[string]interface{})
	if reference["recordName"] != "Reminder/R1" || reference["action"] != "VALIDATE" {
		t.Errorf("reminder reference = %#v", reference)
	}
}

func TestRemoveTagUsesNativeDelete(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			if lookupCount == 1 {
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"HashtagIDs":{"value":["TAG-1"],"type":"STRING_LIST"}}}]}`)
			} else {
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Hashtag/TAG-1","recordType":"Hashtag","recordChangeTag":"tag-change","fields":{"Name":{"value":"travel","type":"STRING","isEncrypted":true}}}]}`)
			}
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateTags("Reminder/R1", nil, []string{"travel"}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 2 {
		t.Fatalf("modify request = %#v", modify)
	}
	if ids := fieldStringList(modify.Operations[0].Record.Fields["HashtagIDs"].Value); len(ids) != 0 {
		t.Errorf("remaining hashtag IDs = %#v", ids)
	}
	deleted := modify.Operations[1]
	if deleted.OperationType != OperationDelete || deleted.Record.RecordName != "Hashtag/TAG-1" || deleted.Record.RecordChangeTag != "tag-change" {
		t.Errorf("delete operation = %#v", deleted)
	}
}

func TestAssignReminderUsesAtomicLinkedChildContract(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/changes/zone"):
			_ = json.NewEncoder(w).Encode(ZoneChangesResponse{Zones: []ChangesResponse{{Records: []Record{{
				RecordName: "Share/1", RecordType: "cloudkit.share",
			}}}}})
		case strings.Contains(r.URL.Path, "/records/lookup"):
			var lookup LookupRequest
			if err := json.NewDecoder(r.Body).Decode(&lookup); err != nil {
				t.Errorf("decode lookup request: %v", err)
			}
			if len(lookup.Records) == 1 && lookup.Records[0].RecordName == "Reminder/R1" {
				_, _ = fmt.Fprintf(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"List":{"value":{"recordName":"List/SHARED","action":"NONE"}},"AssignmentIDs":{"value":[],"type":"EMPTY_LIST"},"ResolutionTokenMap":{"value":%q,"type":"STRING"}}}]}`, nativeResolutionTokenMapFixture)
				return
			}
			_ = json.NewEncoder(w).Encode(RecordsResponse{Records: []Record{{
				RecordName: "Share/1", RecordType: "cloudkit.share",
				Fields: map[string]FieldValue{"RootRecord": {Value: RecordReference{RecordName: "List/SHARED"}}},
				Participants: []ShareParticipant{
					{ParticipantID: "participant-me", AcceptanceStatus: "ACCEPTED", UserIdentity: UserIdentity{NameComponents: NameComponents{GivenName: "Current", FamilyName: "User"}}},
					{ParticipantID: "participant-alex", AcceptanceStatus: "ACCEPTED", UserIdentity: UserIdentity{LookupInfo: UserLookupInfo{EmailAddress: "alex@example.com"}, NameComponents: NameComponents{GivenName: "Alex", FamilyName: "Smith"}}},
				},
				CurrentUser: &ShareParticipant{ParticipantID: "participant-me", AcceptanceStatus: "ACCEPTED"},
			}}})
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateAssignment("Reminder/R1", "alex@example.com", false); err != nil {
		t.Fatalf("UpdateAssignment: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 2 {
		t.Fatalf("modify request = %#v", modify)
	}
	reminder := modify.Operations[0]
	ids := fieldStringList(reminder.Record.Fields["AssignmentIDs"].Value)
	if reminder.OperationType != OperationUpdate || len(ids) != 1 {
		t.Fatalf("reminder operation = %#v", reminder)
	}
	child := modify.Operations[1]
	if child.OperationType != OperationCreate || child.Record.RecordName != "Assignment/"+ids[0] {
		t.Errorf("assignment operation = %#v", child)
	}
	if child.Record.Parent == nil || child.Record.Parent.RecordName != "Reminder/R1" {
		t.Errorf("assignment parent = %#v", child.Record.Parent)
	}
	assignee := child.Record.Fields["EncryptedAssigneeIdentifier"]
	originator := child.Record.Fields["EncryptedOriginatorIdentifier"]
	if assignee.Value != "participant-alex" || assignee.Type != "STRING" || !assignee.IsEncrypted {
		t.Errorf("assignee field = %#v", assignee)
	}
	if originator.Value != "participant-me" || originator.Type != "STRING" || !originator.IsEncrypted {
		t.Errorf("originator field = %#v", originator)
	}
	if child.Record.Fields["OwningReminderIdentifier"].Value != "R1" || child.Record.Fields["Status"].Value != float64(1) {
		t.Errorf("assignment fields = %#v", child.Record.Fields)
	}
	reference, _ := child.Record.Fields["Reminder"].Value.(map[string]interface{})
	if reference["recordName"] != "Reminder/R1" || reference["action"] != "VALIDATE" {
		t.Errorf("reminder reference = %#v", reference)
	}
	var tokens struct {
		Map map[string]interface{} `json:"map"`
	}
	if err := json.Unmarshal([]byte(reminder.Record.Fields["ResolutionTokenMap"].Value.(string)), &tokens); err != nil {
		t.Fatal(err)
	}
	if tokens.Map["assignmentIDs"] == nil || tokens.Map["lastModifiedDate"] == nil {
		t.Errorf("resolution tokens = %#v", tokens.Map)
	}
	assertNativeResolutionTokensPreserved(t, reminder.Record.Fields)
}

func TestUnassignReminderUsesNativeDelete(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/changes/zone"):
			_, _ = io.WriteString(w, `{"zones":[{"records":[{"recordName":"Share/1","recordType":"cloudkit.share"}]}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			switch lookupCount {
			case 1:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"List":{"value":{"recordName":"List/SHARED"}},"AssignmentIDs":{"value":["A1"],"type":"STRING_LIST"}}}]}`)
			case 2:
				_ = json.NewEncoder(w).Encode(RecordsResponse{Records: []Record{{
					RecordName: "Share/1", RecordType: "cloudkit.share",
					Fields:      map[string]FieldValue{"RootRecord": {Value: RecordReference{RecordName: "List/SHARED"}}},
					CurrentUser: &ShareParticipant{ParticipantID: "participant-me", AcceptanceStatus: "ACCEPTED"},
				}}})
			case 3:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Assignment/A1","recordType":"Assignment","recordChangeTag":"assignment-change"}]}`)
			}
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateAssignment("Reminder/R1", "", true); err != nil {
		t.Fatalf("UpdateAssignment: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 2 {
		t.Fatalf("modify request = %#v", modify)
	}
	if ids := fieldStringList(modify.Operations[0].Record.Fields["AssignmentIDs"].Value); len(ids) != 0 {
		t.Errorf("remaining assignment IDs = %#v", ids)
	}
	deleted := modify.Operations[1]
	if deleted.OperationType != OperationDelete || deleted.Record.RecordName != "Assignment/A1" || deleted.Record.RecordChangeTag != "assignment-change" {
		t.Errorf("delete operation = %#v", deleted)
	}
}

func TestSetURLAttachmentReplacesURLAndPreservesOtherAttachments(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			if lookupCount == 1 {
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"AttachmentIDs":{"value":["OLD","IMAGE"],"type":"STRING_LIST"}}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Attachment/OLD","recordType":"Attachment","recordChangeTag":"old-change","fields":{"Type":{"value":"URL","type":"STRING"}}},{"recordName":"Attachment/IMAGE","recordType":"Attachment","recordChangeTag":"image-change","fields":{"Type":{"value":"Image","type":"STRING"}}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateURLAttachment("Reminder/R1", "https://example.com/new"); err != nil {
		t.Fatalf("UpdateURLAttachment: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 3 {
		t.Fatalf("modify request = %#v", modify)
	}
	ids := fieldStringList(modify.Operations[0].Record.Fields["AttachmentIDs"].Value)
	if len(ids) != 2 || ids[0] != "IMAGE" {
		t.Fatalf("attachment IDs = %#v", ids)
	}
	if modify.Operations[1].OperationType != OperationDelete || modify.Operations[1].Record.RecordName != "Attachment/OLD" {
		t.Errorf("delete operation = %#v", modify.Operations[1])
	}
	created := modify.Operations[2]
	if created.OperationType != OperationCreate || created.Record.RecordName != "Attachment/"+ids[1] {
		t.Fatalf("create operation = %#v", created)
	}
	if created.Record.Parent == nil || created.Record.Parent.RecordName != "Reminder/R1" {
		t.Errorf("attachment parent = %#v", created.Record.Parent)
	}
	if field := created.Record.Fields["URL"]; field.Value != "https://example.com/new" || field.Type != "STRING" || !field.IsEncrypted {
		t.Errorf("URL field = %#v", field)
	}
	if created.Record.Fields["Type"].Value != "URL" || created.Record.Fields["UTI"].Value != "public.url" {
		t.Errorf("attachment fields = %#v", created.Record.Fields)
	}
	if _, ok := modify.Operations[0].Record.Fields["ResolutionTokenMap"]; ok {
		t.Error("native URL attachment update must not replace ResolutionTokenMap")
	}
}

func TestClearURLAttachmentUsesNativeDelete(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			if lookupCount == 1 {
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"AttachmentIDs":{"value":["URL1"],"type":"STRING_LIST"}}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Attachment/URL1","recordType":"Attachment","recordChangeTag":"url-change","fields":{"Type":{"value":"URL","type":"STRING"}}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateURLAttachment("Reminder/R1", ""); err != nil {
		t.Fatalf("UpdateURLAttachment: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 2 {
		t.Fatalf("modify request = %#v", modify)
	}
	if ids := fieldStringList(modify.Operations[0].Record.Fields["AttachmentIDs"].Value); len(ids) != 0 {
		t.Errorf("remaining attachment IDs = %#v", ids)
	}
	deleted := modify.Operations[1]
	if deleted.OperationType != OperationDelete || deleted.Record.RecordName != "Attachment/URL1" || deleted.Record.RecordChangeTag != "url-change" {
		t.Errorf("delete operation = %#v", deleted)
	}
}

func TestCreateLocationAlarmUsesAtomicAlarmAndTriggerContract(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = fmt.Fprintf(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"AlarmIDs":{"value":[],"type":"EMPTY_LIST"},"ResolutionTokenMap":{"value":%q,"type":"STRING"}}}]}`, nativeResolutionTokenMapFixture)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	location := LocationAlarm{
		Title: "Eiffel Tower", Address: "Paris", Latitude: 48.8584, Longitude: 2.2945,
		Radius: 150, Proximity: 1,
	}
	if err := service.UpdateLocationAlarm("Reminder/R1", &location); err != nil {
		t.Fatalf("UpdateLocationAlarm: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 3 {
		t.Fatalf("modify request = %#v", modify)
	}
	reminder := modify.Operations[0]
	alarmIDs := fieldStringList(reminder.Record.Fields["AlarmIDs"].Value)
	if reminder.OperationType != OperationUpdate || len(alarmIDs) != 1 {
		t.Fatalf("reminder operation = %#v", reminder)
	}
	var tokens struct {
		Map map[string]interface{} `json:"map"`
	}
	if err := json.Unmarshal([]byte(reminder.Record.Fields["ResolutionTokenMap"].Value.(string)), &tokens); err != nil {
		t.Fatal(err)
	}
	if tokens.Map["alarmIDs"] != nil || tokens.Map["lastModifiedDate"] == nil {
		t.Errorf("resolution tokens = %#v", tokens.Map)
	}
	assertNativeResolutionTokensPreserved(t, reminder.Record.Fields)
	alarm := modify.Operations[1]
	if alarm.OperationType != OperationCreate || alarm.Record.RecordName != "Alarm/"+alarmIDs[0] {
		t.Errorf("alarm operation = %#v", alarm)
	}
	if alarm.Record.Parent == nil || alarm.Record.Parent.RecordName != "Reminder/R1" {
		t.Errorf("alarm parent = %#v", alarm.Record.Parent)
	}
	if alarm.Record.Fields["DueDateResolutionTokenAsNonce"].Value != float64(0) ||
		alarm.Record.Fields["DueDateResolutionTokenAsNonce"].Type != "NUMBER_DOUBLE" {
		t.Errorf("alarm nonce = %#v", alarm.Record.Fields["DueDateResolutionTokenAsNonce"])
	}
	triggerID, _ := alarm.Record.Fields["TriggerID"].Value.(string)
	trigger := modify.Operations[2]
	if trigger.OperationType != OperationCreate || trigger.Record.RecordName != "AlarmTrigger/"+triggerID {
		t.Errorf("trigger operation = %#v", trigger)
	}
	if trigger.Record.Parent == nil || trigger.Record.Parent.RecordName != alarm.Record.RecordName {
		t.Errorf("trigger parent = %#v", trigger.Record.Parent)
	}
	for _, fieldName := range []string{"Title", "Address", "Latitude", "Longitude", "ReferenceFrameString"} {
		if !trigger.Record.Fields[fieldName].IsEncrypted {
			t.Errorf("%s should be encrypted: %#v", fieldName, trigger.Record.Fields[fieldName])
		}
	}
	if trigger.Record.Fields["Imported"].Value != float64(0) || trigger.Record.Fields["Imported"].Type != "NUMBER_INT64" {
		t.Errorf("trigger imported field = %#v", trigger.Record.Fields["Imported"])
	}
	if trigger.Record.Fields["Title"].Value != "Eiffel Tower" ||
		trigger.Record.Fields["Latitude"].Value != float64(48.8584) ||
		trigger.Record.Fields["Longitude"].Value != float64(2.2945) ||
		trigger.Record.Fields["Radius"].Value != float64(150) ||
		trigger.Record.Fields["Proximity"].Value != float64(1) ||
		trigger.Record.Fields["Type"].Value != "Location" {
		t.Errorf("trigger fields = %#v", trigger.Record.Fields)
	}
	reference, _ := trigger.Record.Fields["Alarm"].Value.(map[string]interface{})
	if reference["recordName"] != alarm.Record.RecordName || reference["action"] != "VALIDATE" {
		t.Errorf("alarm reference = %#v", reference)
	}
}

func TestSetTimedDueDateCreatesNativeDateAlarm(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"AlarmIDs":{"value":[],"type":"EMPTY_LIST"}}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	due := time.Date(2026, 8, 26, 14, 30, 0, 0, time.FixedZone("Europe/Paris", 2*60*60))
	if err := service.UpdateDueDate("Reminder/R1", &DueDateChange{Date: due}); err != nil {
		t.Fatalf("UpdateDueDate: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 3 {
		t.Fatalf("modify request = %#v", modify)
	}
	parent := modify.Operations[0].Record
	if parent.Fields["DueDate"].Value != float64(1787747400000) || parent.Fields["AllDay"].Value != float64(0) {
		t.Errorf("due fields = %#v", parent.Fields)
	}
	ids := fieldStringList(parent.Fields["AlarmIDs"].Value)
	if len(ids) != 1 || modify.Operations[1].Record.RecordName != "Alarm/"+ids[0] {
		t.Fatalf("alarm operations = %#v", modify.Operations)
	}
	trigger := modify.Operations[2].Record
	if trigger.Fields["Type"].Value != "Date" || trigger.Parent == nil || trigger.Parent.RecordName != "Alarm/"+ids[0] {
		t.Errorf("trigger = %#v", trigger)
	}
	encoded, _ := trigger.Fields["DateComponentsData"].Value.(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var components map[string]interface{}
	if err := json.Unmarshal(decoded, &components); err != nil {
		t.Fatal(err)
	}
	if components["hour"] != float64(14) || components["minute"] != float64(30) {
		t.Errorf("date components = %#v", components)
	}
}

func TestClearDueDateDeletesOnlyDateAlarm(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			switch lookupCount {
			case 1:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"AlarmIDs":{"value":["DATE","LOCATION"],"type":"STRING_LIST"},"ResolutionTokenMap":{"value":"{\"map\":{\"dueDate\":{\"counter\":1,\"modificationTime\":1,\"replicaID\":\"D\"},\"timeZone\":{\"counter\":1,\"modificationTime\":1,\"replicaID\":\"T\"},\"lastModifiedDate\":{\"counter\":1,\"modificationTime\":1,\"replicaID\":\"L\"}}}"}}}]}`)
			case 2:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Alarm/DATE","recordType":"Alarm","recordChangeTag":"date-change","fields":{"TriggerID":{"value":"TD"}}},{"recordName":"Alarm/LOCATION","recordType":"Alarm","recordChangeTag":"location-change","fields":{"TriggerID":{"value":"TL"}}}]}`)
			case 3:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"AlarmTrigger/TD","recordType":"AlarmTrigger","recordChangeTag":"td-change","fields":{"Type":{"value":"Date"}}}]}`)
			case 4:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"AlarmTrigger/TL","recordType":"AlarmTrigger","recordChangeTag":"tl-change","fields":{"Type":{"value":"Location"}}}]}`)
			}
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateDueDate("Reminder/R1", nil); err != nil {
		t.Fatalf("UpdateDueDate: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 3 {
		t.Fatalf("modify request = %#v", modify)
	}
	parent := modify.Operations[0].Record
	if ids := fieldStringList(parent.Fields["AlarmIDs"].Value); len(ids) != 1 || ids[0] != "LOCATION" {
		t.Errorf("remaining alarm IDs = %#v", ids)
	}
	if parent.Fields["DueDate"].Value != nil || parent.Fields["TimeZone"].Value != nil {
		t.Errorf("cleared fields = %#v", parent.Fields)
	}
	if modify.Operations[1].Record.RecordName != "AlarmTrigger/TD" || modify.Operations[2].Record.RecordName != "Alarm/DATE" {
		t.Errorf("delete operations = %#v", modify.Operations[1:])
	}
}

func TestCreateRecurrenceRejectsClearedDueDate(t *testing.T) {
	t.Parallel()

	modifyCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"DueDate":{"value":null,"type":"TIMESTAMP"}}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			modifyCalled = true
			_ = json.NewEncoder(w).Encode(RecordsResponse{})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	err = service.UpdateRecurrence("Reminder/R1", &RecurrenceRule{Frequency: 1, Interval: 1})
	if err == nil || !strings.Contains(err.Error(), "valid due date") {
		t.Fatalf("UpdateRecurrence error = %v", err)
	}
	if modifyCalled {
		t.Fatal("recurrence mutation was submitted for a cleared due date")
	}
}

func TestUpdateEarlyReminderRejectsClearedDueDate(t *testing.T) {
	t.Parallel()

	baseline, err := json.Marshal(dueDateDeltaAlertsEnvelope{AccountIdentifier: "account-1"})
	if err != nil {
		t.Fatal(err)
	}
	modifyCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = fmt.Fprintf(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"DueDate":{"value":null,"type":"TIMESTAMP"},"DueDateDeltaAlertsData":{"value":%q,"type":"ENCRYPTED_BYTES"}}}]}`, base64.StdEncoding.EncodeToString(baseline))
		case strings.Contains(r.URL.Path, "/records/modify"):
			modifyCalled = true
			_ = json.NewEncoder(w).Encode(RecordsResponse{})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	err = service.UpdateEarlyReminder("Reminder/R1", &EarlyReminder{Unit: 0, Count: 15})
	if err == nil || !strings.Contains(err.Error(), "valid due date") {
		t.Fatalf("UpdateEarlyReminder error = %v", err)
	}
	if modifyCalled {
		t.Fatal("early reminder mutation was submitted for a cleared due date")
	}
}

func TestUpdateRecurrencePreservesExistingNativeRule(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			if lookupCount == 1 {
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"DueDate":{"value":1787904000000,"type":"TIMESTAMP"},"AllDay":{"value":0,"type":"NUMBER_INT64"},"TimeZone":{"value":"Europe/Paris","type":"STRING"},"RecurrenceRuleIDs":{"value":["OLD"],"type":"STRING_LIST"},"ResolutionTokenMap":{"value":"{\"map\":{\"lastModifiedDate\":{\"counter\":1,\"modificationTime\":1,\"replicaID\":\"R\"}}}"}}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"records":[{"recordName":"RecurrenceRule/OLD","recordType":"RecurrenceRule","recordChangeTag":"old-change"}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	end := time.Date(2026, 9, 30, 0, 0, 0, 0, time.FixedZone("Europe/Paris", 2*60*60))
	if err := service.UpdateRecurrence("Reminder/R1", &RecurrenceRule{Frequency: 1, Interval: 2, EndDate: &end}); err != nil {
		t.Fatalf("UpdateRecurrence: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 2 {
		t.Fatalf("modify request = %#v", modify)
	}
	ids := fieldStringList(modify.Operations[0].Record.Fields["RecurrenceRuleIDs"].Value)
	if len(ids) != 1 {
		t.Fatalf("recurrence IDs = %#v", ids)
	}
	updated := modify.Operations[1]
	if updated.OperationType != OperationUpdate || updated.Record.RecordName != "RecurrenceRule/OLD" || updated.Record.RecordChangeTag != "old-change" {
		t.Fatalf("update operation = %#v", updated)
	}
	if updated.Record.Fields["Frequency"].Value != float64(1) || updated.Record.Fields["Interval"].Value != float64(2) {
		t.Errorf("recurrence fields = %#v", updated.Record.Fields)
	}
	if updated.Record.Fields["EndDate"].Value != float64(1790755140000) {
		t.Errorf("end date = %#v", updated.Record.Fields["EndDate"])
	}
}

func TestCreateRecurrenceWritesChildBeforeParent(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"DueDate":{"value":1787904000000,"type":"TIMESTAMP"},"ResolutionTokenMap":{"value":"{\"map\":{\"lastModifiedDate\":{\"counter\":1,\"modificationTime\":1,\"replicaID\":\"R\"}}}"}}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateRecurrence("Reminder/R1", &RecurrenceRule{Frequency: 0, Interval: 1}); err != nil {
		t.Fatalf("UpdateRecurrence: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 2 {
		t.Fatalf("modify request = %#v", modify)
	}
	child := modify.Operations[0]
	parent := modify.Operations[1]
	if child.OperationType != OperationCreate || child.Record.RecordType != "RecurrenceRule" {
		t.Fatalf("first operation = %#v", child)
	}
	ids := fieldStringList(parent.Record.Fields["RecurrenceRuleIDs"].Value)
	if parent.OperationType != OperationUpdate || len(ids) != 1 || child.Record.RecordName != "RecurrenceRule/"+ids[0] {
		t.Fatalf("parent operation = %#v", parent)
	}
	if cached := service.records["Reminder/R1"]; cached.RecordName != "Reminder/R1" || len(fieldStringList(cached.Fields["RecurrenceRuleIDs"].Value)) != 1 {
		t.Fatalf("cached reminder = %#v", cached)
	}
}

func TestClearRecurrenceDeletesNativeRule(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			if lookupCount == 1 {
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"RecurrenceRuleIDs":{"value":["RULE"],"type":"STRING_LIST"},"ResolutionTokenMap":{"value":"{\"map\":{\"lastModifiedDate\":{\"counter\":1,\"modificationTime\":1,\"replicaID\":\"R\"}}}"}}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"records":[{"recordName":"RecurrenceRule/RULE","recordType":"RecurrenceRule","recordChangeTag":"rule-change"}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateRecurrence("Reminder/R1", nil); err != nil {
		t.Fatalf("UpdateRecurrence: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 2 {
		t.Fatalf("modify request = %#v", modify)
	}
	parent := modify.Operations[0]
	deleted := modify.Operations[1]
	if ids := fieldStringList(parent.Record.Fields["RecurrenceRuleIDs"].Value); len(ids) != 0 {
		t.Errorf("recurrence IDs = %#v", ids)
	}
	if deleted.OperationType != OperationDelete || deleted.Record.RecordName != "RecurrenceRule/RULE" || deleted.Record.RecordChangeTag != "rule-change" {
		t.Fatalf("delete operation = %#v", deleted)
	}
}

func TestUpdateEarlyReminderWritesNativeEnvelopeAndToken(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"DueDate":{"value":1787904000000,"type":"TIMESTAMP"},"ResolutionTokenMap":{"value":"{\"map\":{\"lastModifiedDate\":{\"counter\":1,\"modificationTime\":1,\"replicaID\":\"R\"}}}"}}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(RecordsResponse{Records: []Record{modify.Operations[0].Record}})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	baseline, err := json.Marshal(dueDateDeltaAlertsEnvelope{AccountIdentifier: "ACCOUNT"})
	if err != nil {
		t.Fatal(err)
	}
	service.records["Reminder/BASELINE"] = Record{RecordName: "Reminder/BASELINE", RecordType: "Reminder", Fields: map[string]FieldValue{
		"DueDateDeltaAlertsData": {Value: base64.StdEncoding.EncodeToString(baseline), Type: "ENCRYPTED_BYTES"},
	}}
	if err := service.UpdateEarlyReminder("Reminder/R1", &EarlyReminder{Unit: 0, Count: 15}); err != nil {
		t.Fatalf("UpdateEarlyReminder: %v", err)
	}
	if len(modify.Operations) != 1 || modify.Operations[0].OperationType != OperationUpdate {
		t.Fatalf("modify request = %#v", modify)
	}
	fields := modify.Operations[0].Record.Fields
	if fields["DueDateDeltaAlertsData"].Type != "ENCRYPTED_BYTES" {
		t.Fatalf("delta field = %#v", fields["DueDateDeltaAlertsData"])
	}
	encoded := fields["DueDateDeltaAlertsData"].Value.(string)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var envelope dueDateDeltaAlertsEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.AccountIdentifier != "ACCOUNT" || envelope.ReminderIdentifier != "R1" || envelope.MinimumSupportedVersion != 20230430 || len(envelope.DueDateDeltaAlerts) != 1 {
		t.Fatalf("envelope = %#v", envelope)
	}
	alert := envelope.DueDateDeltaAlerts[0]
	if alert.DueDateDeltaUnit != 0 || alert.DueDateDeltaCount != -15 || alert.Identifier == "" {
		t.Errorf("alert = %#v", alert)
	}
	var tokens struct {
		Map map[string]json.RawMessage `json:"map"`
	}
	if err := json.Unmarshal([]byte(fields["ResolutionTokenMap"].Value.(string)), &tokens); err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Map["dueDateDeltaAlertsData"]; !ok {
		t.Errorf("tokens = %#v", tokens.Map)
	}
}

func TestUpdateUrgentReminderUploadsNativeAccountState(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = fmt.Fprintf(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"UrgentPresentationAlarmsAsData":{"value":{"downloadURL":%q},"type":"ASSETID"},"ResolutionTokenMap":{"value":"{\"map\":{\"urgentPresentationAlarmsChecksum\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"},\"lastModifiedDate\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"}}}"}}}]}`, serverURL(r)+"/baseline")
		case r.URL.Path == "/baseline":
			_, _ = io.WriteString(w, `{"minimumSupportedVersion":20251103,"account":[{"isEnabled":false,"modifiedOn":100,"personID":"PERSON"}]}`)
		case strings.Contains(r.URL.Path, "/assets/upload"):
			_ = json.NewEncoder(w).Encode(AssetUploadResponse{Tokens: []AssetUploadToken{{URL: serverURL(r) + "/asset-data"}}})
		case r.URL.Path == "/asset-data":
			var err error
			uploaded, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read uploaded asset: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"singleFile": AssetValue{
				WrappingKey: "key", FileChecksum: "file", Receipt: "receipt", ReferenceChecksum: "reference", Size: int64(len(uploaded)),
			}})
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(RecordsResponse{Records: []Record{modify.Operations[0].Record}})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateUrgentReminder("Reminder/R1", true); err != nil {
		t.Fatalf("UpdateUrgentReminder: %v", err)
	}
	var envelope urgentPresentationAlarmsEnvelope
	if err := json.Unmarshal(uploaded, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.MinimumSupportedVersion != 20251103 || len(envelope.Account) != 1 || !envelope.Account[0].IsEnabled || envelope.Account[0].PersonID != "PERSON" {
		t.Fatalf("urgent envelope = %#v", envelope)
	}
	fields := modify.Operations[0].Record.Fields
	asset, ok := fields["UrgentPresentationAlarmsAsData"].Value.(map[string]interface{})
	if !ok || asset["receipt"] != "receipt" {
		t.Fatalf("urgent asset field = %#v", fields["UrgentPresentationAlarmsAsData"])
	}
	digest := sha512.Sum512(uploaded)
	if checksum := fields["UrgentPresentationAlarmsChecksum"]; checksum.Value != hex.EncodeToString(digest[:]) || checksum.Type != "STRING" || !checksum.IsEncrypted {
		t.Fatalf("urgent checksum = %#v", checksum)
	}
	tokens := resolutionTokens(t, fields)
	for _, key := range []string{"urgentPresentationAlarmsChecksum", "lastModifiedDate"} {
		if counter := tokens[key]["counter"]; counter != float64(3) {
			t.Errorf("urgent token %q counter = %#v, want 3", key, counter)
		}
	}
}

func TestClearLocationAlarmPreservesOtherAlarmTypes(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	lookupCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			lookupCount++
			switch lookupCount {
			case 1:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"AlarmIDs":{"value":["LOC","TIME"],"type":"STRING_LIST"}}}]}`)
			case 2:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Alarm/LOC","recordType":"Alarm","recordChangeTag":"alarm-loc-change","fields":{"TriggerID":{"value":"TRIG-LOC"}}},{"recordName":"Alarm/TIME","recordType":"Alarm","recordChangeTag":"alarm-time-change","fields":{"TriggerID":{"value":"TRIG-TIME"}}}]}`)
			case 3:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"AlarmTrigger/TRIG-LOC","recordType":"AlarmTrigger","recordChangeTag":"trigger-loc-change","fields":{"Type":{"value":"Location"}}}]}`)
			case 4:
				_, _ = io.WriteString(w, `{"records":[{"recordName":"AlarmTrigger/TRIG-TIME","recordType":"AlarmTrigger","recordChangeTag":"trigger-time-change","fields":{"Type":{"value":"Date"}}}]}`)
			}
		case strings.Contains(r.URL.Path, "/records/modify"):
			if err := json.NewDecoder(r.Body).Decode(&modify); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := RecordsResponse{}
			for _, operation := range modify.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	if err := service.UpdateLocationAlarm("Reminder/R1", nil); err != nil {
		t.Fatalf("UpdateLocationAlarm: %v", err)
	}
	if !modify.Atomic || len(modify.Operations) != 3 {
		t.Fatalf("modify request = %#v", modify)
	}
	if ids := fieldStringList(modify.Operations[0].Record.Fields["AlarmIDs"].Value); len(ids) != 1 || ids[0] != "TIME" {
		t.Errorf("remaining alarm IDs = %#v", ids)
	}
	triggerDelete := modify.Operations[1]
	alarmDelete := modify.Operations[2]
	if triggerDelete.OperationType != OperationDelete || triggerDelete.Record.RecordName != "AlarmTrigger/TRIG-LOC" || triggerDelete.Record.RecordChangeTag != "trigger-loc-change" {
		t.Errorf("trigger delete = %#v", triggerDelete)
	}
	if alarmDelete.OperationType != OperationDelete || alarmDelete.Record.RecordName != "Alarm/LOC" || alarmDelete.Record.RecordChangeTag != "alarm-loc-change" {
		t.Errorf("alarm delete = %#v", alarmDelete)
	}
}

func TestSyncBuildsListsAndRemindersFromCloudKitFields(t *testing.T) {
	t.Parallel()

	title, err := encodeTitleDocument("Synced reminder")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/changes/zone"):
			response := ZoneChangesResponse{Zones: []ChangesResponse{{
				SyncToken: "next",
				Records: []Record{
					{RecordName: "LIST-123", RecordType: "ReminderList", Fields: map[string]FieldValue{"Name": {Value: "Inbox"}}},
					{RecordName: "REM-123", RecordType: "Reminder", RecordChangeTag: "c1", Fields: map[string]FieldValue{
						"TitleDocument": {Value: title},
						"Completed":     {Value: true},
						"List":          {Value: map[string]interface{}{"recordName": "LIST-123", "action": "NONE"}},
					}},
				},
			}}}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	service := newRemindersService(client, "")
	lists, err := service.GetLists()
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 || lists[0] != (ReminderList{ID: "LIST-123", Title: "Inbox"}) {
		t.Fatalf("lists = %#v", lists)
	}
	reminders, err := service.GetReminders(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(reminders) != 1 || reminders[0].ID != "REM-123" || reminders[0].ListID != "LIST-123" || !reminders[0].Completed {
		t.Fatalf("reminders = %#v", reminders)
	}
}

func TestSyncPersistsRecordsAndResumesFromDeltaToken(t *testing.T) {
	t.Parallel()

	var receivedTokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/changes/zone"):
			var request ZoneChangesRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode changes request: %v", err)
			}
			token := request.Zones[0].SyncToken
			receivedTokens = append(receivedTokens, token)
			if token == "" {
				_ = json.NewEncoder(w).Encode(ZoneChangesResponse{Zones: []ChangesResponse{{
					SyncToken: "token-1",
					Records:   []Record{{RecordName: "REM-1", RecordType: "Reminder", RecordChangeTag: "c1"}},
				}}})
				return
			}
			_ = json.NewEncoder(w).Encode(ZoneChangesResponse{Zones: []ChangesResponse{{
				SyncToken: "token-2",
				Records:   []Record{{RecordName: "REM-2", RecordType: "Reminder", RecordChangeTag: "c2"}},
			}}})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	newClient := func() *Client {
		client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	cachePath := filepath.Join(t.TempDir(), "cloudkit-cache.json")
	first := newRemindersService(newClient(), cachePath)
	if err := first.Sync(false); err != nil {
		t.Fatal(err)
	}

	second := newRemindersService(newClient(), cachePath)
	if err := second.Sync(false); err != nil {
		t.Fatal(err)
	}
	if len(receivedTokens) != 2 || receivedTokens[0] != "" || receivedTokens[1] != "token-1" {
		t.Fatalf("received sync tokens = %#v", receivedTokens)
	}
	if _, ok := second.records["REM-1"]; !ok {
		t.Error("record loaded from cache was lost during delta sync")
	}
	if _, ok := second.records["REM-2"]; !ok {
		t.Error("delta record was not merged into cache")
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("cache permissions = %o", info.Mode().Perm())
	}
}

func assertCRDTDocumentContains(t *testing.T, encoded, title string) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	zr, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if !strings.Contains(string(decompressed), title) {
		t.Errorf("CRDT document does not contain title %q", title)
	}
	// A valid Reminders document includes the three CRDT operation markers.
	if strings.Count(string(decompressed), "\x1a") < 3 {
		t.Errorf("CRDT document does not contain three operations: %x", decompressed)
	}
}

func assertMergeableStringLengths(t *testing.T, encoded string) {
	t.Helper()
	wantLength, liveLength, attributeLength := mergeableStringLengths(t, encoded)
	if liveLength != wantLength || attributeLength != wantLength {
		t.Fatalf("mergeable string lengths: text=%d live=%d attribute=%d", wantLength, liveLength, attributeLength)
	}
}

func mergeableStringLengths(t *testing.T, encoded string) (uint64, uint64, uint64) {
	t.Helper()
	raw := decodeCRDTTestDocument(t, encoded)
	wrapper, ok := protobufBytesField(raw, 2)
	if !ok {
		t.Fatal("missing document wrapper")
	}
	note, ok := protobufBytesField(wrapper, 3)
	if !ok {
		t.Fatal("missing note")
	}
	fields, err := decodeProtobufFields(note)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := protobufBytesValue(fields, 2)
	wantLength := uint64(len(utf16.Encode([]rune(string(text)))))
	var liveLength, attributeLength uint64
	for _, field := range fields {
		switch {
		case field.number == 3 && field.wire == 2:
			substring, err := decodeProtobufFields(field.payload)
			if err != nil {
				t.Fatal(err)
			}
			if !protobufBoolField(substring, 4) {
				liveLength += protobufVarintValue(substring, 2)
			}
		case field.number == 5 && field.wire == 2:
			attribute, err := decodeProtobufFields(field.payload)
			if err != nil {
				t.Fatal(err)
			}
			attributeLength = protobufVarintValue(attribute, 1)
		}
	}
	return wantLength, liveLength, attributeLength
}

func decodeCRDTTestDocument(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return decompressed
}

func crdtTestHistory(t *testing.T, document []byte) []byte {
	t.Helper()
	wrapper, ok := protobufBytesField(document, 2)
	if !ok {
		t.Fatal("missing document wrapper")
	}
	note, ok := protobufBytesField(wrapper, 3)
	if !ok {
		t.Fatal("missing note")
	}
	var history []byte
	for offset := 0; offset < len(note); {
		start := offset
		tag, tagBytes := decodeVarint(note[offset:])
		if tagBytes == 0 {
			t.Fatal("invalid note tag")
		}
		offset += tagBytes
		if int(tag&7) != 2 {
			t.Fatalf("unexpected note wire type %d", tag&7)
		}
		length, lengthBytes := decodeVarint(note[offset:])
		if lengthBytes == 0 {
			t.Fatal("invalid note length")
		}
		offset += lengthBytes + int(length)
		if offset > len(note) {
			t.Fatal("note field exceeds document")
		}
		if fieldNumber := int(tag >> 3); fieldNumber == 3 || fieldNumber == 4 {
			history = append(history, note[start:offset]...)
		}
	}
	return history
}

func protobufVarintTestField(fields []protobufField, number int) uint64 {
	for _, field := range fields {
		if field.number == number && field.wire == 0 {
			return field.varint
		}
	}
	return 0
}

func protobufBytesTestField(fields []protobufField, number int) ([]byte, bool) {
	for _, field := range fields {
		if field.number == number && field.wire == 2 {
			return field.payload, true
		}
	}
	return nil, false
}
