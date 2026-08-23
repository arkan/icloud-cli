package cloudkit

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/config"
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
	if strings.Contains(record.RecordName, "/") || record.RecordName != strings.ToUpper(record.RecordName) {
		t.Errorf("record name should be a bare uppercase UUID, got %q", record.RecordName)
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
			_, _ = fmt.Fprintf(w, `{"records":[{"recordName":"ABC-123","recordType":"Reminder","recordChangeTag":"change-%d"}]}`, lookupCount+6)
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
	if err := service.UpdateReminder("ABC-123", ReminderChanges{Priority: &priority}); err != nil {
		t.Fatalf("UpdateReminder: %v", err)
	}
	if err := service.DeleteReminder("ABC-123"); err != nil {
		t.Fatalf("DeleteReminder: %v", err)
	}

	if len(operations) != 2 {
		t.Fatalf("operations = %#v", operations)
	}
	edit := operations[0]
	if edit.OperationType != "update" || edit.Record.RecordName != "ABC-123" || edit.Record.RecordChangeTag != "change-7" {
		t.Errorf("edit operation = %#v", edit)
	}
	if edit.Record.Fields["Priority"].Value != float64(priority) {
		t.Errorf("priority field = %#v", edit.Record.Fields["Priority"])
	}
	deleteOp := operations[1]
	if deleteOp.OperationType != "delete" || deleteOp.Record.RecordName != "ABC-123" || deleteOp.Record.RecordChangeTag != "change-8" {
		t.Errorf("delete operation = %#v", deleteOp)
	}
	if len(deleteOp.Record.Fields) != 0 {
		t.Errorf("delete should not emulate a soft deletion: %#v", deleteOp.Record.Fields)
	}
	if lookupCount != 2 {
		t.Errorf("lookup count = %d, want a fresh change tag for every mutation", lookupCount)
	}
}

func TestUpdateReminderRejectsTextChanges(t *testing.T) {
	t.Parallel()

	service := newRemindersService(nil, "")
	title := "not silently ignored"
	err := service.UpdateReminder("ABC-123", ReminderChanges{Title: &title})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("UpdateReminder error = %v", err)
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
