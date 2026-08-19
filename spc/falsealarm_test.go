package spc_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/waylen888/alarm"
	"github.com/waylen888/alarm/spc"
)

// The false-alarm rates quoted in the package documentation. Run with
//
//	go test ./spc/ -run TestFalseAlarmRates -v
//
// Skipped in short mode: it judges 200,000 in-control observations under
// twelve configurations.
//
// The documentation is the golden file. There is no second copy of the table
// in this file to drift away from it, and every cell of every column is
// compared, not just the first. When a change legitimately moves the numbers,
// the failure prints the new table and it is pasted into doc.go by hand —
// twice a year at most, and a good deal less machinery than a writer that
// edits a source file from inside a test.
//
// What is counted is episodes, not observations. Consecutive evaluations of a
// run-based rule share all but one point, so a single false alarm persists
// for several observations and would be counted several times; an operator
// sees one alert. Rule 1's episodes are one observation long almost always,
// so counting observations flatters it against every other rule. The For
// columns are the same episode count restricted to episodes long enough to
// survive a Rule.For of that many sampling intervals.
//
// The rates are driven through the real conditions rather than a
// reimplementation of them, so that the documentation's claim to measure
// things "the way these conditions judge" cannot quietly stop being true.
func TestFalseAlarmRates(t *testing.T) {
	if testing.Short() {
		t.Skip("long-running measurement")
	}

	series := inControl(1, 200000)
	configs := arlConfigs()
	measured := make([]arlRow, len(configs))
	breaches := make([][]bool, len(configs))

	// The return value matters. A subtest that fails leaves its slot either
	// unwritten or written from a measurement it has just called wrong, and
	// neither is fit to compare against doc.go, let alone write into it.
	measuredOK := t.Run("measure", func(t *testing.T) {
		for i, c := range configs {
			t.Run(c.rules+"/"+c.baseline, func(t *testing.T) {
				t.Parallel()
				row, verdicts, err := measure(t, c, series)
				if err != nil {
					t.Fatal(err)
				}
				measured[i], breaches[i] = row, verdicts
			})
		}
	})
	if !measuredOK {
		t.Fatal("the measurement failed; doc.go was neither compared nor rewritten")
	}
	// Belt as well as braces. t.Run reports true for a subtest that skips, and
	// a skip leaves the slot zero; this also catches a row written into the
	// wrong slot, which no amount of pass-or-fail bookkeeping would.
	if err := checkMeasured(configs, measured); err != nil {
		t.Fatalf("the measurement is incomplete, so doc.go was neither compared nor rewritten: %v", err)
	}

	// Before -update, not after. These are the only assertions that know a
	// number is wrong rather than merely different, and -update used to skip
	// every one of them — so the mode that writes the documentation was the
	// mode in which the package's own correctness argument did not run.
	checkARLInvariants(t, series, configs, measured, breaches)

	rendered := renderARLTable(measured)
	if err := checkARLRoundTrip(rendered, measured); err != nil {
		t.Fatalf("the rendered table does not parse back to what produced it: %v", err)
	}

	documented, documentedRows, err := readARLTable(len(measured))
	if err != nil {
		t.Fatalf("reading the table out of doc.go: %v", err)
	}
	// Compared twice on purpose. The row comparison names the cell that moved;
	// the text comparison catches drift the parser normalises away.
	for i, want := range measured {
		if got := documentedRows[i]; !sameARLRow(got, want) {
			t.Errorf("doc.go row %d: %s / %s %v, measured %s / %s %v",
				i, got.rules, got.baseline, got.cols, want.rules, want.baseline, want.cols)
		}
	}
	if documented != rendered {
		t.Errorf("the measurement and the table in doc.go disagree.\n"+
			"doc.go:\n%s\nmeasured:\n%s\n"+
			"If the change was deliberate, paste the measured block over the one in doc.go "+
			"and update every figure the prose quotes from it.",
			documented, rendered)
	}
	t.Logf("\n%s", rendered)
}

// checkMeasured reports whether every configuration produced its own row in
// its own slot. It is not a restatement of the subtests' verdicts: a subtest
// can end without failing and still leave nothing behind.
func checkMeasured(configs []arlConfig, rows []arlRow) error {
	if len(rows) != len(configs) {
		return fmt.Errorf("%d rows for %d configurations", len(rows), len(configs))
	}
	for i, c := range configs {
		r := rows[i]
		if !r.filled {
			return fmt.Errorf("no row for %s / %s", c.rules, c.baseline)
		}
		if r.rules != c.rules || r.baseline != c.baseline {
			return fmt.Errorf("row %d holds %s / %s, want %s / %s — a measurement wrote to the wrong slot",
				i, r.rules, r.baseline, c.rules, c.baseline)
		}
		for j, col := range r.cols {
			if col == 0 || col < -1 {
				return fmt.Errorf("%s / %s: column %d is %d, which no measurement produces",
					c.rules, c.baseline, j, col)
			}
		}
	}
	return nil
}

// checkARLInvariants holds the assertions that decide the numbers are
// physical rather than merely reproducible.
func checkARLInvariants(t *testing.T, series []float64, configs []arlConfig, measured []arlRow, breaches [][]bool) {
	t.Helper()
	arlOf := func(rules, baseline string) int {
		for _, r := range measured {
			if r.rules == rules && r.baseline == baseline {
				return r.cols[0]
			}
		}
		t.Fatalf("no measurement for %s / %s", rules, baseline)
		return 0
	}

	// Rule 1 against a Fixed baseline is the one row with an answer from
	// outside this code: a three-sigma limit on an in-control normal process
	// has an in-control ARL of 370.4. If this lands far from it, the series
	// generator, the evaluation count or the newest-point gate is wrong — not
	// the library.
	if got := arlOf("rule 1", "Fixed"); got < 300 || got > 450 {
		t.Errorf("rule 1 over Fixed: measured in-control ARL %d, want about 370 "+
			"(Shewhart, three sigma)", got)
	}

	// Fixed ignores the reference observations, so all three rule sets judge
	// every observation against identical limits and Check is a union over
	// rules: every rule 1 breach must reappear under every superset. This is
	// an invariant of the code for any seed.
	//
	// It does not hold for the trailing baselines, and asserting it there
	// would be a mistake rather than a stricter test. The observations under
	// test are the last max(Points()) of the window, so a larger rule set
	// pushes the reference period further back — fifteen observations for
	// rule 7 against one for rule 1 — and the two are judged against
	// different centres and sigmas.
	one := breachIndex(configs, breaches, "rule 1", "Fixed")
	for _, superset := range []string{"rules 1,2", "all eight"} {
		bigger := breachIndex(configs, breaches, superset, "Fixed")
		for i := 65; i < len(series); i++ {
			if one[i] && !bigger[i] {
				t.Fatalf("Fixed, observation %d: rule 1 breached but %s did not — "+
					"the larger rule set is not judging the same window", i, superset)
			}
		}
	}

	// An estimated baseline carries its own error and false alarms more often
	// than a known one. The measured ratio is around 0.62 to 0.72; the margin
	// is 0.9, roughly three seed-to-seed standard deviations away.
	// Trailing(200) is deliberately not compared: its gap to Fixed is about
	// 1.4 standard deviations, and asserting an ordering that thin would be
	// encoding sampling noise as a law.
	for _, rules := range []string{"rule 1", "rules 1,2", "all eight"} {
		if est, known := arlOf(rules, "Trailing(50)"), arlOf(rules, "Fixed"); float64(est) > 0.9*float64(known) {
			t.Errorf("%s: Trailing(50)=%d against Fixed=%d, want appreciably noisier", rules, est, known)
		}
	}
}

// arlConfig is one row of the measurement.
type arlConfig struct {
	rules    string
	baseline string
	ref      int
	b        spc.Baseline
	set      []spc.Rule
}

func arlConfigs() []arlConfig {
	rulesets := []struct {
		name string
		set  []spc.Rule
	}{
		{"rule 1", []spc.Rule{spc.Rule1}},
		{"rules 1,2", []spc.Rule{spc.Rule1, spc.Rule2}},
		{"all eight", spc.AllRules()},
	}
	baselines := []struct {
		name string
		ref  int
		b    spc.Baseline
	}{
		{"Fixed", 50, spc.Fixed(100, 1)},
		{"Trailing(50)", 50, spc.Trailing(50)},
		{"Trailing(200)", 200, spc.Trailing(200)},
		{"TrailingRange(50)", 50, spc.TrailingRange(50)},
	}
	var out []arlConfig
	for _, rs := range rulesets {
		for _, bl := range baselines {
			out = append(out, arlConfig{rules: rs.name, baseline: bl.name, ref: bl.ref, b: bl.b, set: rs.set})
		}
	}
	return out
}

// inControl returns n observations of a process centred on 100 with unit
// variance, from a pinned seed. The figures the documentation quotes are
// exact for this series rather than estimates of a population quantity; a
// different seed moves them by a few percent, and by much more for anything
// that fires as rarely as rule 8.
func inControl(seed int64, n int) []float64 {
	xs := make([]float64, n)
	r := rand.New(rand.NewSource(seed))
	for i := range xs {
		xs[i] = 100 + r.NormFloat64()
	}
	return xs
}

// measure drives one configuration's real condition through a real engine,
// one observation at a time, and returns the row for the table together with
// the per-observation verdicts.
// Preconditions come back as an error rather than as t.Fatalf. Fatalf inside
// a parallel subtest ends that goroutine, so the caller's assignment never
// runs and the caller keeps a zero row it cannot tell from a real one. The
// judgements below stay on t: they are opinions about the measurement, not
// failures to make one, and they leave the row assigned.
func measure(t *testing.T, c arlConfig, series []float64) (arlRow, []bool, error) {
	t.Helper()
	cond := spc.Nelson(c.b, c.ref, c.set)
	hinter, ok := cond.(alarm.PointsHinter)
	if !ok {
		return arlRow{}, nil, fmt.Errorf("%s / %s: the condition no longer declares MinPoints, so the window cannot be sized", c.rules, c.baseline)
	}
	need := hinter.MinPoints()

	// A probe wraps the condition so the engine sizes and fills the window
	// exactly as it would in production while the probe's own verdict keeps
	// alert state out of the measurement.
	var breached bool
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	engine := alarm.New(func(alarm.Event) {}, alarm.WithClock(func() time.Time { return now }))
	engine.SetRules([]alarm.Rule{{
		ID: "measure",
		Levels: []alarm.Level{{
			Severity: alarm.SeverityWarn,
			Condition: probeCondition{points: need, judge: func(w alarm.Window) bool {
				breached = cond.Breach(w)
				return false
			}},
		}},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	verdicts := make([]bool, len(series))
	var episodes []int
	evals, run := 0, 0
	for i, v := range series {
		breached = false
		engine.Observe("measure", "k", v, now)
		now = now.Add(time.Second)
		verdicts[i] = breached
		if i+1 < need {
			continue // the window cannot judge yet
		}
		evals++
		if breached {
			run++
			continue
		}
		if run > 0 {
			episodes = append(episodes, run)
			run = 0
		}
	}
	if run > 0 {
		episodes = append(episodes, run)
	}
	if len(episodes) == 0 {
		return arlRow{}, nil, fmt.Errorf("%s / %s: no false alarm in %d evaluations, which no rule set does on an in-control process",
			c.rules, c.baseline, evals)
	}

	// Episode lengths, asserted as fractions rather than as absolutes. Against
	// a known baseline consecutive points are independent and a rule 1
	// episode of three is worth about 7e-6, but an estimated baseline breaks
	// that: successive evaluations share all but one reference point, so a low
	// sigma estimate persists across the window and breaches correlate.
	// TrailingRange(50), the smallest-sigma baseline here, produces one such
	// episode in about a thousand at some seeds. One percent is two orders of
	// magnitude above that and two below the failure it guards, which is an
	// episode counter recording lengths rather than runs.
	if c.rules == "rule 1" {
		if f := longFraction(episodes, 3); f > 0.01 {
			t.Errorf("rule 1 / %s: %.2f%% of episodes survive For=2, want under 1%% — "+
				"episodes are probably not being counted as runs of consecutive breaches",
				c.baseline, 100*f)
		}
	}

	return arlRow{
		filled:   true,
		rules:    c.rules,
		baseline: c.baseline,
		cols:     [3]int{arlAt(evals, episodes, 0), arlAt(evals, episodes, 2), arlAt(evals, episodes, 3)},
	}, verdicts, nil
}

// arlAt returns observations per episode long enough to survive a Rule.For of
// forN sampling intervals — an episode of forN+1 consecutive breaches — or -1
// when none is.
func arlAt(evals int, episodes []int, forN int) int {
	k := 0
	for _, l := range episodes {
		if l >= forN+1 {
			k++
		}
	}
	if k == 0 {
		return -1
	}
	return evals / k
}

func longFraction(episodes []int, minLen int) float64 {
	k := 0
	for _, l := range episodes {
		if l >= minLen {
			k++
		}
	}
	return float64(k) / float64(len(episodes))
}

func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func breachIndex(configs []arlConfig, breaches [][]bool, rules, baseline string) []bool {
	for i, c := range configs {
		if c.rules == rules && c.baseline == baseline {
			return breaches[i]
		}
	}
	panic("no such configuration: " + rules + " / " + baseline)
}
