package alarm_test

import (
	"testing"
	"time"

	"github.com/waylen888/alarm"
)

// TestSetRulesDoesNotReorderCallerLevels pins that installing a rule has no
// side effect on the caller's own slice.
//
// The engine sorts Levels from highest to lowest severity. Rule is passed by
// value, but Levels is a slice: copying the Rule copies only the header, so
// sorting in place would reorder the backing array the caller still holds. A
// caller that retains its []Level — to diff against the next reload, or to
// render current settings — would find it silently rearranged by a call that
// only claims to install rules.
func TestSetRulesDoesNotReorderCallerLevels(t *testing.T) {
	// Deliberately not in the order the engine wants: ascending severity.
	levels := []alarm.Level{
		{Severity: alarm.SeverityInfo, Condition: gt(10)},
		{Severity: alarm.SeverityWarn, Condition: gt(50)},
		{Severity: alarm.SeverityError, Condition: gt(90)},
	}
	want := []alarm.Severity{alarm.SeverityInfo, alarm.SeverityWarn, alarm.SeverityError}

	e := alarm.New(nil)
	e.SetRules([]alarm.Rule{{
		ID:          "r",
		Levels:      levels,
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	for i, lv := range levels {
		if lv.Severity != want[i] {
			t.Fatalf("caller's Levels reordered by SetRules: got %v at index %d, want %v",
				lv.Severity, i, want[i])
		}
	}
}

// TestSetRulesStillSortsInternally guards the other half: the caller's slice is
// left alone, but the engine must still evaluate highest severity first. Fed a
// value breaching all three levels, it has to report error, not the info level
// that comes first in the caller's ordering.
func TestSetRulesStillSortsInternally(t *testing.T) {
	var got []alarm.Event
	e := alarm.New(func(ev alarm.Event) { got = append(got, ev) })

	e.SetRules([]alarm.Rule{{
		ID: "r",
		Levels: []alarm.Level{
			{Severity: alarm.SeverityInfo, Condition: gt(10)},
			{Severity: alarm.SeverityWarn, Condition: gt(50)},
			{Severity: alarm.SeverityError, Condition: gt(90)},
		},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	e.Observe("r", "k", 95, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	if len(got) != 1 {
		t.Fatalf("events = %v; want exactly one", got)
	}
	if got[0].Severity != alarm.SeverityError {
		t.Fatalf("severity = %v; want error (highest breaching level wins)", got[0].Severity)
	}
}

func gt(limit float64) alarm.Condition {
	return alarm.Threshold(func(v float64) bool { return v > limit })
}
