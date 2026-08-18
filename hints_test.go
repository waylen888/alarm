package alarm_test

// These tests live in package alarm_test on purpose: they may only reach
// exported identifiers, which is exactly the property under test. A condition
// defined outside package alarm must be able to declare how the engine sizes
// its window and what value it reports.

import (
	"testing"
	"time"

	"github.com/waylen888/alarm"
)

// spanCounter is a Condition defined outside package alarm that implements all
// three optional hint interfaces. It breaches once at least n observations
// fall inside window, and reports that count as the measured value.
//
// points is what it declares through PointsHinter, kept separate from n so a
// test can under-declare the point budget and leave the span hint as the only
// thing that can grow the window.
type spanCounter struct {
	n      int
	window time.Duration
	points int
}

func (c spanCounter) Breach(w alarm.Window) bool { return w.Count(c.window) >= c.n }

func (c spanCounter) MinPoints() int { return c.points }

func (c spanCounter) MinSpan() time.Duration { return c.window }

func (c spanCounter) Measure(w alarm.Window) float64 { return float64(w.Count(c.window)) }

// Compile-time proof that an out-of-package type can satisfy the interfaces.
var (
	_ alarm.Condition    = spanCounter{}
	_ alarm.PointsHinter = spanCounter{}
	_ alarm.SpanHinter   = spanCounter{}
	_ alarm.Measurer     = spanCounter{}
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newEngine returns an engine recording every event it emits.
func newEngine(t *testing.T, rules ...alarm.Rule) (*alarm.Engine, *[]alarm.Event) {
	t.Helper()
	var got []alarm.Event
	e := alarm.New(func(ev alarm.Event) { got = append(got, ev) })
	e.SetRules(rules)
	return e, &got
}

// TestExternalMinPointsHonoured proves MinPoints sizes the window. A condition
// that declares nothing gets a 64-point budget, so requiring 100 consecutive
// observations can only ever breach if the declared MinPoints was actually
// used. n is deliberately above that default: at 40 the test would pass either
// way and prove nothing.
func TestExternalMinPointsHonoured(t *testing.T) {
	const n = 100
	e, got := newEngine(t, alarm.Rule{
		ID: "r",
		Levels: []alarm.Level{{
			Severity:  alarm.SeverityError,
			Condition: consecutive{n: n},
		}},
		StaleAfter:  -1,
		VanishAfter: -1,
	})

	for i := 0; i < n-1; i++ {
		e.Observe("r", "k", 1, base.Add(time.Duration(i)*time.Second))
	}
	if len(*got) != 0 {
		t.Fatalf("fired after %d of %d observations; want silence", n-1, n)
	}

	e.Observe("r", "k", 1, base.Add(time.Duration(n-1)*time.Second))
	if len(*got) != 1 || (*got)[0].Kind != alarm.EventFire {
		t.Fatalf("events = %v; want one fire once %d observations are retained", *got, n)
	}
}

// consecutive breaches when the last n observations are all non-zero. It
// declares MinPoints and nothing else.
type consecutive struct{ n int }

func (c consecutive) Breach(w alarm.Window) bool {
	pts := w.LastN(c.n)
	if len(pts) < c.n {
		return false
	}
	for _, p := range pts {
		if p.Value == 0 {
			return false
		}
	}
	return true
}

func (c consecutive) MinPoints() int { return c.n }

// TestExternalMinSpanHonoured proves MinSpan grows the window, in isolation
// from MinPoints. The condition declares a budget of only 3 points — below the
// engine's floor of 8 — but a 10-minute span. Feeding it 200 observations
// inside that span must not evict the early ones, which can only happen if the
// span hint drove the window to grow.
func TestExternalMinSpanHonoured(t *testing.T) {
	const hits = 200
	e, got := newEngine(t, alarm.Rule{
		ID: "r",
		Levels: []alarm.Level{{
			Severity:  alarm.SeverityWarn,
			Condition: spanCounter{n: hits, window: 10 * time.Minute, points: 3},
		}},
		StaleAfter:  -1,
		VanishAfter: -1,
	})

	// hits observations, one per second, all inside the 10-minute span.
	for i := 0; i < hits; i++ {
		e.Observe("r", "k", 1, base.Add(time.Duration(i)*time.Second))
	}

	if len(*got) != 1 || (*got)[0].Kind != alarm.EventFire {
		t.Fatalf("events = %v; want one fire (window grew to hold %d points across the span)", *got, hits)
	}
}

// TestExternalMeasurerHonoured proves Event.Value comes from Measure rather
// than from the last observed value. Every observation carries the value 1,
// so a Value of 5 can only have come from the condition's own Measure.
func TestExternalMeasurerHonoured(t *testing.T) {
	const hits = 5
	e, got := newEngine(t, alarm.Rule{
		ID: "r",
		Levels: []alarm.Level{{
			Severity:  alarm.SeverityWarn,
			Condition: spanCounter{n: hits, window: time.Minute, points: hits},
		}},
		StaleAfter:  -1,
		VanishAfter: -1,
	})

	for i := 0; i < hits; i++ {
		e.Observe("r", "k", 1, base.Add(time.Duration(i)*time.Second))
	}

	if len(*got) != 1 {
		t.Fatalf("events = %v; want exactly one", *got)
	}
	ev := (*got)[0]
	if ev.Kind != alarm.EventFire {
		t.Fatalf("kind = %v; want fire", ev.Kind)
	}
	if ev.Value != hits {
		t.Fatalf("Value = %v; want %d from Measure (last observed value was 1)", ev.Value, hits)
	}
}

// TestExternalNoHintsDefaults pins the documented fallbacks: a condition
// implementing none of the hint interfaces gets the default point budget and
// reports the last observed value.
func TestExternalNoHintsDefaults(t *testing.T) {
	e, got := newEngine(t, alarm.Rule{
		ID: "r",
		Levels: []alarm.Level{{
			Severity:  alarm.SeverityError,
			Condition: bare{},
		}},
		StaleAfter:  -1,
		VanishAfter: -1,
	})

	e.Observe("r", "k", 42, base)
	if len(*got) != 1 || (*got)[0].Kind != alarm.EventFire {
		t.Fatalf("events = %v; want one fire", *got)
	}
	if v := (*got)[0].Value; v != 42 {
		t.Fatalf("Value = %v; want 42 (the last observed value)", v)
	}
}

// bare implements Condition and nothing else.
type bare struct{}

func (bare) Breach(w alarm.Window) bool {
	p, ok := w.Last()
	return ok && p.Value > 10
}
