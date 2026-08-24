package reminders

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/cloudkit"
	"github.com/arkan/icloud-cli/internal/config"
)

func TestAddPreflightsEarlyReminderBeforeCreatingRecord(t *testing.T) {
	t.Parallel()

	modifyCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/changes/zone"):
			_, _ = io.WriteString(w, `{"zones":[{"records":[],"syncToken":"next"}]}`)
		case strings.Contains(r.URL.Path, "/records/lookup"):
			_, _ = io.WriteString(w, `{"records":[{"recordName":"Reminder/TEST","recordType":"Reminder","recordChangeTag":"change-1","fields":{"DueDate":{"value":1787927700000,"type":"TIMESTAMP"},"ResolutionTokenMap":{"value":"{\"map\":{\"lastModifiedDate\":{\"counter\":1,\"modificationTime\":1,\"replicaID\":\"device\"}}}"}}}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			modifyCount++
			var request cloudkit.ModifyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode modify request: %v", err)
			}
			response := cloudkit.RecordsResponse{}
			for _, operation := range request.Operations {
				response.Records = append(response.Records, operation.Record)
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}})
	service, err := NewService(client)
	if err != nil {
		t.Fatal(err)
	}
	due := &cloudkit.DueDateChange{Date: time.Date(2026, 8, 28, 14, 35, 0, 0, time.UTC), TimeZone: "UTC"}
	err = service.Add("Test", "", "List/TEST", due, 0, "", &cloudkit.EarlyReminder{Unit: 1, Count: 1}, false)
	if err == nil || !strings.Contains(err.Error(), "cannot determine the Reminders account identifier") {
		t.Fatalf("Add error = %v", err)
	}
	if modifyCount != 0 {
		t.Fatalf("add submitted %d mutations before early-reminder validation", modifyCount)
	}
}

func TestAddPreflightsUrgentReminderBeforeCreatingRecord(t *testing.T) {
	t.Parallel()

	modifyCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/zones/list"):
			_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
		case strings.Contains(r.URL.Path, "/changes/zone"):
			_, _ = io.WriteString(w, `{"zones":[{"records":[],"syncToken":"next"}]}`)
		case strings.Contains(r.URL.Path, "/records/modify"):
			modifyCount++
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}})
	service, err := NewService(client)
	if err != nil {
		t.Fatal(err)
	}
	err = service.Add("Test", "", "List/TEST", nil, 0, "", nil, true)
	if err == nil || !strings.Contains(err.Error(), "cannot determine the Urgent alarm person identifier") {
		t.Fatalf("Add error = %v", err)
	}
	if modifyCount != 0 {
		t.Fatalf("add submitted %d mutations before Urgent-reminder validation", modifyCount)
	}
}

func TestAddRollsBackOnlyNewReminderAfterLateFailure(t *testing.T) {
	testCases := []struct {
		name          string
		failure       string
		rollbackFails bool
	}{
		{name: "due date", failure: "due"},
		{name: "early alert", failure: "early"},
		{name: "Urgent asset", failure: "urgent"},
		{name: "rollback failure is reported", failure: "due", rollbackFails: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var created cloudkit.Record
			var modifyRequests []cloudkit.ModifyRequest
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/zones/list"):
					_, _ = io.WriteString(w, `{"zones":[{"zoneID":{"zoneName":"Reminders","ownerRecordName":"owner-123"}}]}`)
				case strings.Contains(r.URL.Path, "/changes/zone"):
					switch testCase.failure {
					case "early":
						encoded := base64.StdEncoding.EncodeToString([]byte(`{"accountIdentifier":"account-123"}`))
						_ = json.NewEncoder(w).Encode(map[string]interface{}{"zones": []interface{}{map[string]interface{}{"records": []interface{}{map[string]interface{}{"recordName": "Reminder/BASELINE", "recordType": "Reminder", "fields": map[string]interface{}{"DueDateDeltaAlertsData": map[string]interface{}{"value": encoded, "type": "ENCRYPTED_BYTES"}}}}, "syncToken": "next"}}})
					case "urgent":
						_ = json.NewEncoder(w).Encode(map[string]interface{}{"zones": []interface{}{map[string]interface{}{"records": []interface{}{map[string]interface{}{"recordName": "Reminder/BASELINE", "recordType": "Reminder", "fields": map[string]interface{}{"UrgentPresentationAlarmsAsData": map[string]interface{}{"value": map[string]interface{}{"downloadURL": server.URL + "/urgent-fixture"}, "type": "ASSETID"}}}}, "syncToken": "next"}}})
					default:
						_, _ = io.WriteString(w, `{"zones":[{"records":[],"syncToken":"next"}]}`)
					}
				case r.URL.Path == "/urgent-fixture":
					_, _ = io.WriteString(w, `{"minimumSupportedVersion":20251103,"account":[{"isEnabled":false,"modifiedOn":1,"personID":"person-123"}]}`)
				case strings.Contains(r.URL.Path, "/assets/upload"):
					http.Error(w, "forced Urgent asset failure", http.StatusInternalServerError)
				case strings.Contains(r.URL.Path, "/records/lookup"):
					_ = json.NewEncoder(w).Encode(cloudkit.RecordsResponse{Records: []cloudkit.Record{created}})
				case strings.Contains(r.URL.Path, "/records/modify"):
					var request cloudkit.ModifyRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Errorf("decode modify request: %v", err)
					}
					modifyRequests = append(modifyRequests, request)
					operation := request.Operations[0]
					switch operation.OperationType {
					case cloudkit.OperationCreate:
						created = operation.Record
						created.RecordChangeTag = "created-change"
						_ = json.NewEncoder(w).Encode(cloudkit.RecordsResponse{Records: []cloudkit.Record{created}})
					case cloudkit.OperationDelete:
						if testCase.rollbackFails {
							http.Error(w, "forced rollback failure", http.StatusInternalServerError)
							return
						}
						_ = json.NewEncoder(w).Encode(cloudkit.RecordsResponse{Records: []cloudkit.Record{operation.Record}})
					default:
						if testCase.failure == "early" && len(request.Operations) == 3 {
							for fieldName, field := range operation.Record.Fields {
								created.Fields[fieldName] = field
							}
							created.RecordChangeTag = "due-change"
							response := cloudkit.RecordsResponse{}
							for index, dueOperation := range request.Operations {
								record := dueOperation.Record
								if index == 0 {
									record.RecordChangeTag = created.RecordChangeTag
								}
								response.Records = append(response.Records, record)
							}
							_ = json.NewEncoder(w).Encode(response)
							return
						}
						http.Error(w, "forced due-date failure", http.StatusInternalServerError)
					}
				default:
					http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer server.Close()

			client := api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}})
			service, err := NewService(client)
			if err != nil {
				t.Fatal(err)
			}
			var due *cloudkit.DueDateChange
			var early *cloudkit.EarlyReminder
			urgent := false
			switch testCase.failure {
			case "due":
				due = &cloudkit.DueDateChange{Date: time.Date(2026, 8, 28, 14, 35, 0, 0, time.UTC), TimeZone: "UTC"}
			case "early":
				due = &cloudkit.DueDateChange{Date: time.Date(2026, 8, 28, 14, 35, 0, 0, time.UTC), TimeZone: "UTC"}
				early = &cloudkit.EarlyReminder{Unit: 1, Count: 1}
			case "urgent":
				urgent = true
			}
			err = service.Add("Rollback test", "", "List/TEST", due, 0, "", early, urgent)
			if err == nil {
				t.Fatal("Add unexpectedly succeeded")
			}
			if testCase.rollbackFails && !strings.Contains(err.Error(), "rollback of new reminder") {
				t.Fatalf("rollback failure was not reported: %v", err)
			}
			if len(modifyRequests) < 2 {
				t.Fatalf("modify requests = %d, want create and rollback", len(modifyRequests))
			}
			first := modifyRequests[0].Operations
			last := modifyRequests[len(modifyRequests)-1].Operations
			if len(first) != 1 || first[0].OperationType != cloudkit.OperationCreate {
				t.Fatalf("first mutation = %#v", first)
			}
			if len(last) != 1 || last[0].OperationType != cloudkit.OperationDelete || last[0].Record.RecordName != created.RecordName {
				t.Fatalf("rollback mutation = %#v, created = %q", last, created.RecordName)
			}
			for _, request := range modifyRequests {
				for _, operation := range request.Operations {
					if operation.Record.RecordName == "Reminder/BASELINE" {
						t.Fatalf("mutation touched pre-existing baseline: %#v", operation)
					}
				}
			}
			if testCase.failure == "early" {
				if len(modifyRequests) != 4 {
					t.Fatalf("early modify requests = %d, want create, due, early, rollback", len(modifyRequests))
				}
				dueOperations := modifyRequests[1].Operations
				if !modifyRequests[1].Atomic || len(dueOperations) != 3 || dueOperations[0].OperationType != cloudkit.OperationUpdate || dueOperations[1].OperationType != cloudkit.OperationCreate || dueOperations[2].OperationType != cloudkit.OperationCreate {
					t.Fatalf("due mutation = %#v", modifyRequests[1])
				}
				if dueOperations[1].Record.Parent == nil || dueOperations[1].Record.Parent.RecordName != created.RecordName {
					t.Fatalf("alarm parent = %#v, want %q", dueOperations[1].Record.Parent, created.RecordName)
				}
				if dueOperations[2].Record.Parent == nil || dueOperations[2].Record.Parent.RecordName != dueOperations[1].Record.RecordName {
					t.Fatalf("trigger parent = %#v, want %q", dueOperations[2].Record.Parent, dueOperations[1].Record.RecordName)
				}
			}
		})
	}
}
