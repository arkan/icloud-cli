package main

import "testing"

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
