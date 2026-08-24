package cloudkit

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/config"
)

// TestIntegrationReminderLifecycle is intentionally opt-in because it writes to
// the authenticated iCloud account. Use a dedicated account when possible:
//
//	ICLOUD_INTEGRATION=1 go test ./internal/cloudkit -run TestIntegrationReminderLifecycle -v
func TestIntegrationReminderLifecycle(t *testing.T) {
	if os.Getenv("ICLOUD_INTEGRATION") != "1" {
		t.Skip("set ICLOUD_INTEGRATION=1 to run the live CloudKit lifecycle test")
	}

	session, err := config.LoadSession()
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if session == nil || len(session.Webservices) == 0 {
		t.Fatal("no authenticated session; run `icloud login` first")
	}
	client, err := NewClient(api.NewClient(session))
	if err != nil {
		t.Fatal(err)
	}
	service := NewRemindersService(client)
	lists, err := service.GetLists()
	if err != nil {
		t.Fatalf("get lists: %v", err)
	}
	if len(lists) == 0 {
		t.Fatal("account has no reminder list")
	}

	title := fmt.Sprintf("icloud-cli integration %d 🛒", time.Now().Unix())
	created, err := service.AddReminder(title, "integration test; safe to delete", lists[0].ID, 0, nil)
	if err != nil {
		t.Fatalf("create reminder: %v", err)
	}
	t.Cleanup(func() {
		if err := deleteReminderWithRetry(service, created.ID); err != nil {
			t.Logf("cleanup reminder %s: %v", created.ID, err)
		}
	})

	if err := service.Sync(true); err != nil {
		t.Fatalf("resync after create: %v", err)
	}
	assertReminder(t, service, created.ID, func(reminder ReminderItem) bool {
		return reminder.Title == title && !reminder.Completed
	})

	editedPriority := 5
	editedDue := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	if err := retryMutation(func() error {
		return service.UpdateReminder(created.ID, ReminderChanges{
			Priority: &editedPriority,
		})
	}); err != nil {
		t.Fatalf("edit reminder: %v", err)
	}
	if err := retryMutation(func() error {
		return service.UpdateDueDate(created.ID, &DueDateChange{Date: editedDue})
	}); err != nil {
		t.Fatalf("edit reminder due date: %v", err)
	}
	if err := service.Sync(true); err != nil {
		t.Fatalf("resync after edit: %v", err)
	}
	assertReminder(t, service, created.ID, func(reminder ReminderItem) bool {
		return reminder.Title == title && reminder.Notes == "integration test; safe to delete" &&
			reminder.Priority == editedPriority && reminder.DueDate != nil &&
			reminder.DueDate.UnixMilli() == editedDue.UnixMilli()
	})

	if err := deleteReminderWithRetry(service, created.ID); err != nil {
		t.Fatalf("delete reminder: %v", err)
	}
	if err := service.Sync(true); err != nil {
		t.Fatalf("resync after delete: %v", err)
	}
	reminders, err := service.GetReminders(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, reminder := range reminders {
		if reminder.ID == created.ID {
			t.Fatalf("deleted reminder %s is still returned", created.ID)
		}
	}
}

func deleteReminderWithRetry(service *RemindersService, id string) error {
	return retryMutation(func() error { return service.DeleteReminder(id) })
}

func retryMutation(mutate func() error) error {
	var lastError error
	for attempt := 0; attempt < 5; attempt++ {
		if err := mutate(); err == nil || strings.Contains(err.Error(), "NOT_FOUND") {
			return nil
		} else {
			lastError = err
		}
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	return lastError
}

func assertReminder(t *testing.T, service *RemindersService, id string, matches func(ReminderItem) bool) {
	t.Helper()
	reminders, err := service.GetReminders(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, reminder := range reminders {
		if reminder.ID == id {
			if !matches(reminder) {
				t.Fatalf("reminder did not match expectation: %#v", reminder)
			}
			return
		}
	}
	t.Fatalf("reminder %s not found after CloudKit resync", id)
}
