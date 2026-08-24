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
