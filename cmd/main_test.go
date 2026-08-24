package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arkan/icloud-cli/internal/cloudkit"
	"github.com/arkan/icloud-cli/internal/reminders"
	"github.com/fatih/color"
)

func TestRenderReminderShowText(t *testing.T) {
	due := time.Date(2026, 8, 28, 10, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	completedAt := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	details := reminderShowView{
		Reminder: reminders.ParsedReminder{
			GUID: "ABC12345-full", RecordName: "Reminder/ABC12345-full", Title: "Buy milk",
			Description: "Get two cartons", DueDate: &due, Priority: 5, Flagged: true,
			Completed: true, CompletionDate: &completedAt,
		},
		ListName: "Shopping",
		Properties: cloudkit.ReminderProperties{
			URL: "https://example.com/product", Tags: []string{"shopping", "errands"},
			Assignee: "participant-1", TimeZone: "Europe/Paris", AllDay: false,
			Recurrence:    &cloudkit.RecurrenceRule{Frequency: 1, Interval: 2},
			EarlyReminder: &cloudkit.EarlyReminder{Unit: 0, Count: 15},
			Urgent:        boolPointer(true),
			Location:      &cloudkit.LocationAlarm{Title: "Market", Address: "1 Main St", Proximity: 1},
		},
		Parent:   &reminders.ParsedReminder{GUID: "PARENT01-full", Title: "Groceries"},
		Subtasks: []reminders.ParsedReminder{{GUID: "CHILD001-full", Title: "Check fridge", Completed: true}},
	}

	var output bytes.Buffer
	renderReminderShowText(&output, details, true)
	want := `Title:      Buy milk
ID:         ABC12345-full
List:       Shopping
Parent:     Groceries (PARENT01)
Completed:  yes
Completed at: 2026-08-27T09:00:00Z
Priority:   medium
Flagged:    yes
Due:        2026-08-28 10:00 UTC+2
Timezone:   Europe/Paris
All day:    no
URL:        https://example.com/product
Tags:       shopping, errands
Assigned:   participant-1
Location:   Market — 1 Main St (arriving)
Repeat:     every 2 weeks
Early:      15 minutes
Urgent:     yes
Created:    —
Modified:   —
Notes:
  Get two cartons
Subtasks:
  └─ ✓ Check fridge (CHILD001)
`
	if output.String() != want {
		t.Fatalf("show output:\n%s\nwant:\n%s", output.String(), want)
	}
}

func boolPointer(value bool) *bool { return &value }

func TestFindReminderByIDRequiresUniquePrefix(t *testing.T) {
	items := []reminders.ParsedReminder{
		{GUID: "ABC12345-one", RecordName: "Reminder/ABC12345-one"},
		{GUID: "ABC12399-two", RecordName: "Reminder/ABC12399-two"},
	}
	if _, err := findReminderByID(items, "ABC123"); err == nil || !strings.Contains(err.Error(), "multiple reminders") {
		t.Fatalf("ambiguous prefix error = %v", err)
	}
	item, err := findReminderByID(items, "abc12345")
	if err != nil {
		t.Fatal(err)
	}
	if item.RecordName != "Reminder/ABC12345-one" {
		t.Fatalf("matched reminder = %#v", item)
	}
}

func TestRenderReminderShowJSONIsStableAndMachineReadable(t *testing.T) {
	view := reminderShowView{
		Reminder: reminders.ParsedReminder{GUID: "ABC12345", RecordName: "Reminder/ABC12345", Title: "Buy milk", Priority: 1},
		ListName: "Shopping",
		Subtasks: []reminders.ParsedReminder{{GUID: "CHILD001", RecordName: "Reminder/CHILD001", Title: "Check fridge"}},
	}
	var output bytes.Buffer
	if err := renderReminderShowJSON(&output, view); err != nil {
		t.Fatal(err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, output.String())
	}
	if document["id"] != "ABC12345" || document["title"] != "Buy milk" || document["list"] != "Shopping" || document["priority"] != "high" {
		t.Fatalf("JSON document = %#v", document)
	}
	subtasks, ok := document["subtasks"].([]interface{})
	if !ok || len(subtasks) != 1 || subtasks[0].(map[string]interface{})["id"] != "CHILD001" {
		t.Fatalf("JSON subtasks = %#v", document["subtasks"])
	}
}

func TestShowRejectsJSONAndRawTogetherBeforeAccessingICloud(t *testing.T) {
	err := (&ShowCmd{ID: "ABC12345", JSON: true, Raw: true}).Run()
	if err == nil || err.Error() != "--json and --raw are mutually exclusive" {
		t.Fatalf("conflicting format error = %v", err)
	}
}

func TestRenderReminderShowTextDisplaysUnsetSupportedProperties(t *testing.T) {
	var output bytes.Buffer
	renderReminderShowText(&output, reminderShowView{Reminder: reminders.ParsedReminder{GUID: "ABC", Title: "Empty"}}, true)
	for _, label := range []string{"Completed at:", "Due:", "Timezone:", "All day:", "URL:", "Tags:", "Assigned:", "Location:", "Repeat:", "Early:", "Urgent:", "Created:", "Modified:", "Notes:", "Subtasks:"} {
		if !strings.Contains(output.String(), label) {
			t.Errorf("unset property %q missing from output:\n%s", label, output.String())
		}
	}
}

func TestRenderRemindersShowsSubtasksAsTree(t *testing.T) {
	previousNoColor := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = previousNoColor })

	items := []reminders.ParsedReminder{
		{GUID: "PARENT01-full", RecordName: "Reminder/PARENT01-full", Title: "Parent"},
		{GUID: "CHILD001-full", RecordName: "Reminder/CHILD001-full", ParentRecordName: "Reminder/PARENT01-full", Title: "First child"},
		{GUID: "GRAND001-full", RecordName: "Reminder/GRAND001-full", ParentRecordName: "Reminder/CHILD001-full", Title: "Grandchild"},
		{GUID: "CHILD002-full", RecordName: "Reminder/CHILD002-full", ParentRecordName: "Reminder/PARENT01-full", Title: "Last child"},
		{GUID: "ORPHAN01-full", RecordName: "Reminder/ORPHAN01-full", ParentRecordName: "Reminder/MISSING", Title: "Orphan"},
	}

	var output bytes.Buffer
	renderReminders(&output, items, false)
	lines := reminderTitleLines(output.String())
	want := []string{
		"  ○ Parent",
		"  ├─ ○ First child",
		"  │  └─ ○ Grandchild",
		"  └─ ○ Last child",
		"  ○ Orphan",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tree title lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestRenderRemindersFlatPreservesInputOrderWithoutBranches(t *testing.T) {
	previousNoColor := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = previousNoColor })

	items := []reminders.ParsedReminder{
		{GUID: "CHILD001", RecordName: "Reminder/CHILD001", ParentRecordName: "Reminder/PARENT01", Title: "Child"},
		{GUID: "PARENT01", RecordName: "Reminder/PARENT01", Title: "Parent"},
	}
	var output bytes.Buffer
	renderReminders(&output, items, true)
	lines := reminderTitleLines(output.String())
	want := []string{"  ○ Child", "  ○ Parent"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("flat title lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func reminderTitleLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "○") || strings.Contains(line, "✓") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestParseEarlyReminder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		unit  int
		count int
		clear bool
	}{
		{input: "15m", unit: 0, count: 15},
		{input: "1h", unit: 1, count: 1},
		{input: "2d", unit: 2, count: 2},
		{input: "1w", unit: 3, count: 1},
		{input: "1mo", unit: 4, count: 1},
		{input: "clear", clear: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			alert, clear, err := parseEarlyReminder(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if clear != test.clear {
				t.Fatalf("clear = %v, want %v", clear, test.clear)
			}
			if test.clear {
				if alert != nil {
					t.Fatalf("alert = %#v, want nil", alert)
				}
				return
			}
			if alert == nil || alert.Unit != test.unit || alert.Count != test.count {
				t.Fatalf("alert = %#v, want unit %d count %d", alert, test.unit, test.count)
			}
		})
	}
}

func TestParseEarlyReminderRejectsInvalidOffset(t *testing.T) {
	t.Parallel()
	if _, _, err := parseEarlyReminder("soon"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestUrgentChange(t *testing.T) {
	t.Parallel()

	enabled, err := urgentChange(true, false)
	if err != nil || enabled == nil || !*enabled {
		t.Fatalf("urgentChange(true, false) = %#v, %v", enabled, err)
	}
	disabled, err := urgentChange(false, true)
	if err != nil || disabled == nil || *disabled {
		t.Fatalf("urgentChange(false, true) = %#v, %v", disabled, err)
	}
	unchanged, err := urgentChange(false, false)
	if err != nil || unchanged != nil {
		t.Fatalf("urgentChange(false, false) = %#v, %v", unchanged, err)
	}
	if _, err := urgentChange(true, true); err == nil {
		t.Fatal("urgentChange(true, true) should reject conflicting flags")
	}
}
