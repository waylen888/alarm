package spc_test

import (
	"fmt"
	"time"

	"github.com/waylen888/alarm"
	"github.com/waylen888/alarm/spc"
)

// A metric with no fixed normal range, judged against its own recent
// behaviour. The baseline is estimated from the 30 observations preceding the
// ones under test, so the shift cannot pollute the limits it is judged
// against on any single evaluation.
//
// The rule is shaped the way a paging rule has to be. The rules are a required
// argument and there is no set quiet enough to be a safe default; the package
// documentation has the measured false-alarm rates. ClearFor is set longer
// than an incident is expected to last, because a trailing baseline follows
// a sustained shift into the reference period and the condition goes quiet
// again after about ref observations whether or not anything was fixed.
// StaleAfter is a small multiple of the sampling interval, so a degraded
// feed becomes Stale instead of silently stretching what "recent" means.
func Example() {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	engine := alarm.New(func(ev alarm.Event) {
		fmt.Printf("%s %s/%s z=%.1f\n", ev.Kind, ev.RuleID, ev.Key, ev.Value)
	}, alarm.WithClock(func() time.Time { return now }))

	engine.SetRules([]alarm.Rule{{
		ID: "latency-shift",
		Levels: []alarm.Level{{
			Severity: alarm.SeverityWarn,
			// Nine consecutive samples on one side of the centre line: a
			// sustained shift that never comes close to a three-sigma limit.
			Condition: spc.Nelson(spc.TrailingRobust(30), 30, []spc.Rule{spc.Rule2}),
		}},
		ClearFor:    15 * time.Minute,
		StaleAfter:  5 * time.Second,
		VanishAfter: time.Hour,
	}})

	observe := func(v float64) {
		engine.Observe("latency-shift", "api", v, now)
		now = now.Add(time.Second)
	}

	// A steady process: 30 reference samples plus 9 under test, so the
	// condition has the 39 observations it declares.
	jitter := []float64{0.3, -0.5, 0.8, -0.2, 0.1, -0.9, 0.6, -0.4}
	for i := 0; i < 39; i++ {
		observe(100 + jitter[i%len(jitter)])
	}

	// Nine samples a little above the centre line, none of them remarkable
	// on its own.
	for i := 0; i < 9; i++ {
		observe(100.9)
	}

	// Output:
	// fire latency-shift/api z=1.6
}
