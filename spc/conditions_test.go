package spc_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/waylen888/alarm"
	"github.com/waylen888/alarm/spc"
)

// Compile-time proof that a constructor's result is an alarm.Condition. The
// remaining hints are optional interfaces the engine type-asserts at runtime,
// so they are checked the same way the engine checks them.
var (
	_ alarm.Condition = spc.Nelson(spc.Fixed(0, 1), 2, spc.DefaultRules())
	_ alarm.Condition = spc.EWMA(spc.Fixed(0, 1), 2, 0.2, 3)
)

func TestConditionsImplementTheRightHints(t *testing.T) {
	for name, c := range map[string]alarm.Condition{
		"Nelson": spc.Nelson(spc.Trailing(30), 30, []spc.Rule{spc.Rule1}),
		"EWMA":   spc.EWMA(spc.Trailing(30), 30, 0.2, 3),
	} {
		if _, ok := c.(alarm.PointsHinter); !ok {
			t.Errorf("%s does not implement alarm.PointsHinter; the engine would size its window to DefaultMinPoints", name)
		}
		if _, ok := c.(alarm.Measurer); !ok {
			t.Errorf("%s does not implement alarm.Measurer; events would carry the last raw observation", name)
		}
		if _, ok := c.(alarm.SpanHinter); ok {
			t.Errorf("%s implements alarm.SpanHinter, but these conditions are point-count based and have no meaningful span to declare", name)
		}
	}
}

func minPoints(t *testing.T, c alarm.Condition) int {
	t.Helper()
	h, ok := c.(alarm.PointsHinter)
	if !ok {
		t.Fatal("condition does not implement alarm.PointsHinter")
	}
	return h.MinPoints()
}

// MinPoints must count the reference observations as well as the rule's own,
// or the window is sized for half of what the condition reads.
func TestNelsonMinPoints(t *testing.T) {
	cases := []struct {
		name string
		c    alarm.Condition
		want int
	}{
		{"rule 7 over Trailing(50): 15 + 50", spc.Nelson(spc.Trailing(50), 50, []spc.Rule{spc.Rule7}), 65},
		{"rule 1 over Trailing(50): 1 + 50", spc.Nelson(spc.Trailing(50), 50, []spc.Rule{spc.Rule1}), 51},
		{"rules 1 and 2: the larger wins", spc.Nelson(spc.Trailing(20), 20, []spc.Rule{spc.Rule1, spc.Rule2}), 29},
		{"AllRules: rule 7 is the largest", spc.Nelson(spc.Trailing(20), 20, spc.AllRules()), 35},
		{"Fixed asks for nothing, but ref still stands", spc.Nelson(spc.Fixed(0, 1), 10, []spc.Rule{spc.Rule2}), 19},
		{"a baseline needing more than ref wins", spc.Nelson(spc.Trailing(50), 10, []spc.Rule{spc.Rule7}), 65},
		{"Fixed reads no reference, so rule 1 alone needs one point", spc.Nelson(spc.Fixed(0, 1), 0, []spc.Rule{spc.Rule1}), 1},
	}
	for _, c := range cases {
		if got := minPoints(t, c.c); got != c.want {
			t.Errorf("%s: MinPoints = %d, want %d", c.name, got, c.want)
		}
	}
}

// A caller-supplied Baseline that does not declare its reference size gets
// the MinRefPoints floor: there is no way to tell whether it can work with
// fewer, and a condition that is never true is the worse guess.
func TestAnUndeclaredBaselineGetsTheReferenceFloor(t *testing.T) {
	if got, want := minPoints(t, spc.Nelson(undeclared{}, 0, []spc.Rule{spc.Rule1})), 3; got != want {
		t.Errorf("MinPoints = %d, want %d", got, want)
	}
	if got, want := minPoints(t, spc.Nelson(undeclared{}, 30, []spc.Rule{spc.Rule1})), 31; got != want {
		t.Errorf("MinPoints = %d, want %d", got, want)
	}
}

// undeclared is a Baseline that does not implement spc.RefSizer, standing in
// for the store-backed baseline a caller would write.
type undeclared struct{}

func (undeclared) Estimate(ref []float64) (float64, float64, bool) { return 0, 1, true }

func TestEWMAMinPointsCountsTheBaseline(t *testing.T) {
	// EWMAMinPoints(0.2) is 21.
	if got, want := minPoints(t, spc.EWMA(spc.Trailing(50), 50, 0.2, 3)), 71; got != want {
		t.Errorf("MinPoints = %d, want %d", got, want)
	}
	// EWMAMinPoints(0.5) is 7.
	if got, want := minPoints(t, spc.EWMA(spc.Fixed(0, 1), 10, 0.5, 3)), 17; got != want {
		t.Errorf("MinPoints = %d, want %d", got, want)
	}
}

// Every condition must declare a MinPoints a window can actually reach,
// however badly the caller argued for the opposite.
func TestMinPointsStaysInsideTheWindowCap(t *testing.T) {
	cases := map[string]alarm.Condition{
		"a tiny lambda":        spc.EWMA(spc.Fixed(0, 1), 2, 1e-9, 3),
		"a NaN lambda":         spc.EWMA(spc.Fixed(0, 1), 2, math.NaN(), 3),
		"an enormous ref":      spc.Nelson(spc.Fixed(0, 1), 1<<20, []spc.Rule{spc.Rule7}),
		"an enormous baseline": spc.Nelson(spc.Trailing(1<<20), 2, []spc.Rule{spc.Rule7}),
	}
	for name, c := range cases {
		if got := minPoints(t, c); got > alarm.MaxWindowPoints {
			t.Errorf("%s: MinPoints = %d, above the %d cap", name, got, alarm.MaxWindowPoints)
		}
	}
}

func TestEWMAClampsItsArguments(t *testing.T) {
	// lambda above 1 clamps to 1, whose derived point count is 1.
	if got, want := minPoints(t, spc.EWMA(spc.Fixed(0, 1), 4, 5, 3)), 5; got != want {
		t.Errorf("lambda 5: MinPoints = %d, want %d", got, want)
	}
	// A non-finite lambda falls back to DefaultLambda, whose count is 21.
	if got, want := minPoints(t, spc.EWMA(spc.Fixed(0, 1), 4, math.Inf(1), 3)), 25; got != want {
		t.Errorf("infinite lambda: MinPoints = %d, want %d", got, want)
	}
	// A non-positive L falls back to DefaultL and must still be able to fire.
	c := spc.EWMA(spc.Fixed(100, 1), 2, 1, -1)
	if !c.Breach(seriesWindow(100, 100, 110)) {
		t.Error("an L of -1 should fall back to DefaultL, not disable the condition")
	}
}

// The effective configuration, including anything clamped at construction,
// must be recoverable from the value the constructor returned.
func TestConditionsReportTheirEffectiveConfiguration(t *testing.T) {
	// L is clamped from -1 to DefaultL, and the baseline's 50 wins over the
	// requested ref of 10.
	got := fmt.Sprint(spc.EWMA(spc.Trailing(50), 10, 0.2, -1))
	for _, want := range []string{"ref=50", "points=21", "lambda=0.2", "L=3"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not mention %q; a clamped argument is invisible to the caller", got, want)
		}
	}

	// The case worth seeing: a lambda too small for any window is raised to
	// the floor, which leaves so little room that Trailing(50) is squeezed
	// down to 2 reference points and can never estimate. The condition is
	// permanently false, and the only place that is visible is here.
	squeezed := fmt.Sprint(spc.EWMA(spc.Trailing(50), 50, 1e-9, 3))
	if !strings.Contains(squeezed, "ref=2") {
		t.Errorf("%q should show the reference squeezed down to 2", squeezed)
	}
	if got := fmt.Sprint(spc.Nelson(spc.Fixed(0, 1), 4, []spc.Rule{spc.Rule2})); !strings.Contains(got, "rule2") {
		t.Errorf("%q does not name the enabled rules", got)
	}
}

// Input that is entirely wrong must not select the noisiest condition the
// package can build. A mistyped rule constant in a configuration file falls
// back to rule 1, not to all eight.
// String's only job is its format, so it is the one place a golden string is
// the right instrument: nothing weaker distinguishes the intended output from
// a mangled one, which is how a corrupted format literal shipped. The
// fragment assertions above cover the clamping semantics; this covers the
// rendering, and duplicate rules being dropped while order is preserved.
func TestConditionStringsRenderExactly(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{fmt.Sprint(spc.Nelson(spc.Trailing(50), 10, []spc.Rule{spc.Rule2, spc.Rule2, spc.Rule1})),
			"spc.Nelson(ref=50, points=9, rules=[rule2 rule1])"},
		{fmt.Sprint(spc.EWMA(spc.Trailing(50), 10, 0.2, -1)),
			"spc.EWMA(ref=50, points=21, lambda=0.2, L=3)"},
	} {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
}

func TestNelsonWithOnlyUnknownRulesFallsBackToTheDefault(t *testing.T) {
	// DefaultRules is rule 1, so one point under test plus two reference.
	if got, want := minPoints(t, spc.Nelson(spc.Fixed(0, 1), 2, []spc.Rule{spc.Rule(0), spc.Rule(99)})), 3; got != want {
		t.Errorf("MinPoints = %d, want %d", got, want)
	}
	if got, want := minPoints(t, spc.Nelson(spc.Fixed(0, 1), 2, nil)), 3; got != want {
		t.Errorf("an empty rule set: MinPoints = %d, want %d", got, want)
	}
	// All eight is still available, by asking for it.
	if got, want := minPoints(t, spc.Nelson(spc.Fixed(0, 1), 2, spc.AllRules())), 17; got != want {
		t.Errorf("AllRules: MinPoints = %d, want %d", got, want)
	}
}

// A condition given too few observations must report false rather than
// judging a partial series.
func TestConditionsAreFalseUntilTheWindowFills(t *testing.T) {
	c := spc.Nelson(spc.Fixed(100, 1), 2, []spc.Rule{spc.Rule1})
	if c.Breach(seriesWindow(200)) {
		t.Error("breached with fewer observations than MinPoints")
	}
	if got := c.(alarm.Measurer).Measure(seriesWindow(200)); got != 0 {
		t.Errorf("Measure = %v with an unfilled window, want 0", got)
	}
	// MinPoints is 3: two reference points (unused by Fixed) and one under test.
	if !c.Breach(seriesWindow(100, 100, 200)) {
		t.Error("did not breach once the window filled")
	}
}

func TestNelsonMeasureReportsSigmaDistance(t *testing.T) {
	c := spc.Nelson(spc.Fixed(100, 2), 2, []spc.Rule{spc.Rule1}).(alarm.Measurer)
	if got := c.Measure(seriesWindow(100, 100, 107)); math.Abs(got-3.5) > 1e-9 {
		t.Errorf("Measure = %v, want 3.5", got)
	}
}

func TestEWMAMeasureReportsTheStatistic(t *testing.T) {
	// lambda 0.5 needs EWMAMinPoints(0.5) = 7 observations under test. Seeded
	// at the first of them, the statistic over 100 x6 then 107 is
	// 0.5*107 + 0.5*100 = 103.5 — nothing like the last raw observation, which
	// is the value this method exists not to report. A lambda of 1 would
	// collapse the two and the assertion would hold against a Measure that
	// ignored the statistic entirely.
	c := spc.EWMA(spc.Fixed(100, 1), 2, 0.5, 3).(alarm.Measurer)
	w := seriesWindow(100, 100, 100, 100, 100, 100, 100, 100, 107)
	if got := c.Measure(w); math.Abs(got-103.5) > 1e-9 {
		t.Errorf("Measure = %v, want 103.5 (the statistic, not the last observation)", got)
	}
}

// A baseline that cannot estimate must leave the condition false rather than
// dividing by a sigma of zero.
func TestAnUnusableBaselineNeverBreaches(t *testing.T) {
	flat := make([]float64, 40)
	for i := range flat {
		flat[i] = 100 // no variance, so Trailing cannot produce a sigma
	}
	flat[len(flat)-1] = 500

	for name, c := range map[string]alarm.Condition{
		"Nelson": spc.Nelson(spc.Trailing(20), 20, []spc.Rule{spc.Rule1}),
		"EWMA":   spc.EWMA(spc.Trailing(20), 20, 0.5, 3),
	} {
		if c.Breach(seriesWindow(flat...)) {
			t.Errorf("%s breached against a baseline that reported false", name)
		}
	}
}

// Only rules completing at the newest observation breach. A spike that has
// scrolled back into the series is not a live breach.
func TestNelsonOnlyFiresAtTheNewestObservation(t *testing.T) {
	c := spc.Nelson(spc.Fixed(100, 1), 2, []spc.Rule{spc.Rule2})
	shifted := []float64{100, 100} // reference, unused by Fixed
	for i := 0; i < 9; i++ {
		shifted = append(shifted, 101)
	}
	if !c.Breach(seriesWindow(shifted...)) {
		t.Fatal("nine points above the centre should breach")
	}
	recovered := append(append([]float64(nil), shifted...), 99)
	if c.Breach(seriesWindow(recovered...)) {
		t.Error("the run was broken by the newest observation; the condition should have cleared")
	}
}

// With rules of different lengths the series under test is longer than the
// shorter rule needs, so a rule can complete at a position that is no longer
// the newest observation. Those must not breach.
func TestNelsonIgnoresRulesThatCompletedEarlierInTheSeries(t *testing.T) {
	c := spc.Nelson(spc.Fixed(100, 1), 2, []spc.Rule{spc.Rule1, spc.Rule7})

	// Two reference points, then fifteen under test: a four-sigma spike at
	// the oldest of them and quiet ever since. Rule 1 completed at the spike,
	// fourteen observations ago; rule 7 is broken by it.
	stale := []float64{100, 100, 104}
	for len(stale) < 17 {
		stale = append(stale, 100.1)
	}
	if c.Breach(seriesWindow(stale...)) {
		t.Error("a spike fourteen observations old is not a live breach")
	}

	// The same spike as the newest observation does breach.
	fresh := []float64{100, 100}
	for len(fresh) < 16 {
		fresh = append(fresh, 100.1)
	}
	fresh = append(fresh, 104)
	if !c.Breach(seriesWindow(fresh...)) {
		t.Error("a four-sigma spike at the newest observation should breach rule 1")
	}
}

func TestEWMADetectsASmallSustainedShift(t *testing.T) {
	c := spc.EWMA(spc.Fixed(100, 1), 2, 0.2, 3)
	// A shift of one sigma, sustained. Rule 1 would never see it.
	vals := []float64{100, 100}
	for i := 0; i < 21; i++ {
		vals = append(vals, 101)
	}
	if !c.Breach(seriesWindow(vals...)) {
		t.Error("a sustained one-sigma shift should leave the EWMA limits")
	}

	steady := []float64{100, 100}
	for i := 0; i < 21; i++ {
		if i%2 == 0 {
			steady = append(steady, 100.5)
		} else {
			steady = append(steady, 99.5)
		}
	}
	// The same shift downward. An EWMA chart is two-sided, and every other
	// test here drives the statistic upward, so a one-sided comparison —
	// dropping the math.Abs in Breach — passes all of them while detecting
	// half of what the chart exists for.
	down := []float64{100, 100}
	for i := 0; i < 21; i++ {
		down = append(down, 99)
	}
	if !c.Breach(seriesWindow(down...)) {
		t.Error("a sustained one-sigma shift below the centre should leave the EWMA limits")
	}

	if c.Breach(seriesWindow(steady...)) {
		t.Error("a process centred on the centre line should not breach")
	}
}

// The startup correction must reach the condition, not just the function.
// At n=21 with lambda 0.2 the corrected half-width is about 4% narrower than
// the steady-state one, so a shift sized into that gap breaches only if
// Breach passed len(test) rather than a steady-state n.
func TestEWMABreachUsesTheStartupCorrectedLimits(t *testing.T) {
	const lambda, l, sigma = 0.2, 3.0, 1.0
	n := spc.EWMAMinPoints(lambda)
	corrected, ok := spc.EWMAControlLimits(sigma, lambda, l, n)
	if !ok {
		t.Fatal("EWMAControlLimits reported false")
	}
	steady, ok := spc.EWMAControlLimits(sigma, lambda, l, 1<<20)
	if !ok {
		t.Fatal("EWMAControlLimits reported false")
	}
	if !(corrected < steady) {
		t.Fatalf("the correction should narrow the limits: %v vs %v", corrected, steady)
	}

	// A constant offset makes the statistic equal to that offset, so place it
	// between the two half-widths.
	offset := (corrected + steady) / 2
	vals := []float64{100, 100}
	for i := 0; i < n; i++ {
		vals = append(vals, 100+offset)
	}
	if !spc.EWMA(spc.Fixed(100, sigma), 2, lambda, l).Breach(seriesWindow(vals...)) {
		t.Errorf("a statistic %v from the centre is outside the corrected limit %v "+
			"but inside the steady-state limit %v; Breach used the wrong n",
			offset, corrected, steady)
	}
}

// The integration the hint interfaces exist for: a Nelson condition whose
// rules need 15 observations and whose baseline needs 50 must actually breach
// when driven through a real engine, which sizes the window from MinPoints
// alone.
func TestWindowSizingThroughTheEngine(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var fired []alarm.Event
	engine := alarm.New(func(ev alarm.Event) { fired = append(fired, ev) },
		alarm.WithClock(func() time.Time { return now }))

	engine.SetRules([]alarm.Rule{{
		ID: "spc",
		Levels: []alarm.Level{{
			Severity: alarm.SeverityWarn,
			// Rule 7 needs 15 observations, Trailing(50) needs 50: 65 in all,
			// well past alarm.DefaultMinPoints of 64.
			Condition: spc.Nelson(spc.Trailing(50), 50, []spc.Rule{spc.Rule7}),
		}},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	// A noisy reference period, then fifteen observations hugging the centre
	// line: rule 7's "sigma is too wide for this process" signal.
	off := []float64{3, -5, 8, -2, 1, -9, 6, -4, 2, -7, 5, -1, 9, -3, 4, -6}
	for i := 0; i < 50; i++ {
		engine.Observe("spc", "k", 100+off[i%len(off)], now)
		now = now.Add(time.Second)
	}
	if len(fired) != 0 {
		t.Fatalf("the reference period alone should not fire: %v", fired)
	}
	for i := 0; i < 15; i++ {
		engine.Observe("spc", "k", 100.1, now)
		now = now.Add(time.Second)
	}

	if len(fired) == 0 {
		t.Fatal("no event: MinPoints did not account for both the rule and the baseline, " +
			"so the window never held enough observations for the condition to be true")
	}
	if fired[0].Kind != alarm.EventFire {
		t.Errorf("first event is %v, want fire", fired[0].Kind)
	}
	if state, _, _ := engine.State("spc", "k"); state != alarm.StateFiring {
		t.Errorf("state is %v, want firing", state)
	}
	t.Logf("fired with value (sigma distance) %.3f", fired[0].Value)
}

// The documented way to tell which rule fired: one alarm.Level per rule.
// The engine sizes a rule's window from the largest MinPoints among its
// levels, so the levels share one window rather than one each, and Severity
// identifies what fired.
func TestOneLevelPerRuleIdentifiesWhatFired(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var fired []alarm.Event
	engine := alarm.New(func(ev alarm.Event) { fired = append(fired, ev) },
		alarm.WithClock(func() time.Time { return now }))

	engine.SetRules([]alarm.Rule{{
		ID: "spc",
		Levels: []alarm.Level{
			{Severity: alarm.SeverityError, Condition: spc.Nelson(spc.Trailing(30), 30, []spc.Rule{spc.Rule1})},
			{Severity: alarm.SeverityWarn, Condition: spc.Nelson(spc.Trailing(30), 30, []spc.Rule{spc.Rule2})},
		},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	observe := func(v float64) {
		engine.Observe("spc", "k", v, now)
		now = now.Add(time.Second)
	}
	jitter := []float64{0.3, -0.5, 0.8, -0.2, 0.1, -0.9, 0.6, -0.4}
	for i := 0; i < 39; i++ {
		observe(100 + jitter[i%len(jitter)])
	}
	if len(fired) != 0 {
		t.Fatalf("a steady process should not fire: %v", fired)
	}

	// Nine samples a little above the centre: rule 2, and nothing near 3σ.
	for i := 0; i < 9; i++ {
		observe(100.9)
	}
	if len(fired) == 0 {
		t.Fatal("a nine-point shift should have fired rule 2's level")
	}
	if got := fired[0].Severity; got != alarm.SeverityWarn {
		t.Errorf("first event severity = %v, want warn — the level identifies the rule", got)
	}

	// A gross excursion: rule 1's level is higher and takes over.
	observe(140)
	last := fired[len(fired)-1]
	if last.Severity != alarm.SeverityError {
		t.Errorf("after a gross excursion severity = %v, want error; events: %v", last.Severity, fired)
	}
	if last.Kind != alarm.EventEscalate {
		t.Errorf("last event kind = %v, want escalate", last.Kind)
	}
}

// seriesWindow builds an alarm.Window over vals, one second apart, by feeding
// them through an engine and capturing the window the condition receives.
// Going through the engine rather than a hand-written fake keeps the tests
// honest about what a real window returns.
func seriesWindow(vals ...float64) alarm.Window {
	var got alarm.Window
	capture := conditionFunc(func(w alarm.Window) bool {
		got = w
		return false
	})

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	engine := alarm.New(func(alarm.Event) {}, alarm.WithClock(func() time.Time { return now }))
	engine.SetRules([]alarm.Rule{{
		ID:          "capture",
		Levels:      []alarm.Level{{Severity: alarm.SeverityWarn, Condition: capture}},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})
	for _, v := range vals {
		engine.Observe("capture", "k", v, now)
		now = now.Add(time.Second)
	}
	return got
}

type conditionFunc func(alarm.Window) bool

func (f conditionFunc) Breach(w alarm.Window) bool { return f(w) }

func (f conditionFunc) MinPoints() int { return alarm.MaxWindowPoints }
