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
			_, _ = fmt.Fprintf(w, `{"records":[{"recordName":"ABC-123","recordType":"Reminder","recordChangeTag":"change-%d","fields":{"ResolutionTokenMap":{"value":"{\"map\":{\"flagged\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"},\"lastModifiedDate\":{\"counter\":2,\"modificationTime\":100,\"replicaID\":\"device\"}}}"}}}]}`, lookupCount+6)
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
	flagged := true
	if err := service.UpdateReminder("ABC-123", ReminderChanges{Title: &title, Priority: &priority, Flagged: &flagged}); err != nil {
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
	if edit.Record.Fields["Flagged"].Value != float64(1) || edit.Record.Fields["Flagged"].Type != "NUMBER_INT64" {
		t.Errorf("flagged field = %#v", edit.Record.Fields["Flagged"])
	}
	tokenJSON, _ := edit.Record.Fields["ResolutionTokenMap"].Value.(string)
	if !strings.Contains(tokenJSON, `"counter":3`) || !strings.Contains(tokenJSON, `"replicaID":"device"`) {
		t.Errorf("resolution token map = %s", tokenJSON)
	}
	if edit.Record.Fields["LastModifiedDate"].Type != "TIMESTAMP" {
		t.Errorf("last modified field = %#v", edit.Record.Fields["LastModifiedDate"])
	}
	complete := operations[1]
	if complete.Record.Fields["Completed"].Value != float64(1) {
		t.Errorf("complete fields = %#v", complete.Record.Fields)
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

func TestCreateTagUsesAtomicLinkedChildContract(t *testing.T) {
	t.Parallel()

	var modify ModifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"change-1","fields":{"HashtagIDs":{"value":[],"type":"EMPTY_LIST"}}}]}`)
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
