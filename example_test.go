package alarm_test

import (
	"fmt"
	"time"

	"github.com/waylen888/alarm"
)

// A full fire-then-resolve cycle, driven by an injected clock so the run is
// deterministic. The rule requires the condition to hold for two minutes
// before it fires, so the first two observations pass silently.
func Example() {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	engine := alarm.New(func(ev alarm.Event) {
		fmt.Printf("%s %s/%s severity=%s value=%.0f\n",
			ev.Kind, ev.RuleID, ev.Key, ev.Severity, ev.Value)
	}, alarm.WithClock(func() time.Time { return now }))

	engine.SetRules([]alarm.Rule{{
		ID: "cpu-high",
		Levels: []alarm.Level{{
			Severity:  alarm.SeverityError,
			Condition: alarm.Threshold(func(v float64) bool { return v > 90 }),
		}},
		For:         2 * time.Minute,
		StaleAfter:  -1, // no data-gap detection in this example
		VanishAfter: -1,
	}})

	observe := func(v float64) { engine.Observe("cpu-high", "node-1", v, now) }
	advance := func(d time.Duration) { now = now.Add(d) }

	observe(95) // condition holds: Pending, silent
	advance(time.Minute)
	observe(97) // still Pending, For has not elapsed
	advance(2 * time.Minute)
	observe(96) // For elapsed: Fire
	advance(time.Minute)
	observe(10) // condition clears: Resolve

	fmt.Println("firing keys:", len(engine.Snapshot()))
	// Output:
	// fire cpu-high/node-1 severity=error value=96
	// resolve cpu-high/node-1 severity=error value=10
	// firing keys: 0
}

// A time-window condition decays on its own. Nothing new is observed after
// the alert fires; Tick advances time until the hits fall out of the window
// and the alert resolves.
func ExampleEngine_Tick() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	engine := alarm.New(func(ev alarm.Event) {
		fmt.Printf("%s %s/%s count=%.0f\n", ev.Kind, ev.RuleID, ev.Key, ev.Value)
	})

	engine.SetRules([]alarm.Rule{{
		ID: "log-flood",
		Levels: []alarm.Level{{
			Severity:  alarm.SeverityWarn,
			Condition: alarm.CountInWindow(3, time.Minute),
		}},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	engine.ObserveEvent("log-flood", "auth", base)
	engine.ObserveEvent("log-flood", "auth", base.Add(10*time.Second))
	engine.ObserveEvent("log-flood", "auth", base.Add(20*time.Second)) // third hit in a minute

	engine.Tick(base.Add(90 * time.Second)) // all three hits now outside the window
	// Output:
	// fire log-flood/auth count=3
	// resolve log-flood/auth count=0
}

// Exit is the de-escalation guard. Here warn means "two consecutive samples
// at 1 or above" and error means "two consecutive samples at 2 or above", so
// every error sample also satisfies warn. Without Exit, the first sample that
// drops to 1 would immediately step the alert down to warn, because the
// preceding error sample still counts towards the warn condition. Exit
// requires two consecutive sub-error samples before the step down happens.
func ExampleLevel_exitGuard() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	engine := alarm.New(func(ev alarm.Event) {
		fmt.Printf("%s severity=%s\n", ev.Kind, ev.Severity)
	})

	engine.SetRules([]alarm.Rule{{
		ID: "link",
		Levels: []alarm.Level{
			{
				Severity:  alarm.SeverityError,
				Condition: alarm.ConsecutiveN(2, func(v float64) bool { return v >= 2 }),
				Exit:      alarm.ConsecutiveN(2, func(v float64) bool { return v < 2 }),
			},
			{
				Severity:  alarm.SeverityWarn,
				Condition: alarm.ConsecutiveN(2, func(v float64) bool { return v >= 1 }),
			},
		},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }

	engine.Observe("link", "line-1", 2, at(0))
	engine.Observe("link", "line-1", 2, at(1)) // two error samples: Fire at error
	engine.Observe("link", "line-1", 1, at(2)) // warn holds, but Exit does not: stays error
	engine.Observe("link", "line-1", 1, at(3)) // Exit now holds: Deescalate to warn
	// Output:
	// fire severity=error
	// deescalate severity=warn
}
