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
	"time"

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
				_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/R1","recordType":"Reminder","recordChangeTag":"rem-change","fields":{"List":{"value":{"recordName":"List/SHARED","action":"NONE"}},"AssignmentIDs":{"value":[],"type":"EMPTY_LIST"}}}]}`)
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
