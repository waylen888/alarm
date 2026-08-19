package spc_test

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/waylen888/alarm"
	"github.com/waylen888/alarm/spc"
)

var updateDoc = flag.Bool("update", false, "rewrite the false-alarm table in doc.go from the measurement")

// The false-alarm rates quoted in the package documentation. Run with
//
//	go test ./spc/ -run TestFalseAlarmRates -v
//
// and, after a change that legitimately moves the numbers,
//
//	go test ./spc/ -run TestFalseAlarmRates -update
//
// which rewrites the table in doc.go in place. Skipped in short mode: it
// judges 200,000 in-control observations under twelve configurations.
//
// The documentation is the golden file. There is no second copy of the table
// in this file to drift away from it, and every cell of every column is
// compared, not just the first.
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

	// One engine per configuration, because each needs its own window size and
	// its own state. The series is generated once and shared, so that every
	// configuration sees the identical realisation — the comparisons between
	// rows below depend on that.
	t.Run("measure", func(t *testing.T) {
		for i, c := range configs {
			t.Run(c.rules+"/"+c.baseline, func(t *testing.T) {
				t.Parallel()
				measured[i], breaches[i] = measure(t, c, series)
			})
		}
	})

	rendered := renderARLTable(measured)
	if *updateDoc {
		if err := replaceARLTable(rendered); err != nil {
			t.Fatalf("rewriting doc.go: %v", err)
		}
		t.Logf("doc.go updated:\n%s", rendered)
		return
	}

	documented, err := readARLTable()
	if err != nil {
		t.Fatalf("reading the table out of doc.go: %v", err)
	}
	if documented != rendered {
		t.Errorf("the measurement and the table in doc.go disagree.\n"+
			"doc.go:\n%s\nmeasured:\n%s\n"+
			"Re-run with -update if the change was deliberate, and read the diff before committing it.",
			documented, rendered)
	}
	t.Logf("\n%s", rendered)

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

	for _, baseline := range []string{"Fixed", "Trailing(50)", "Trailing(200)", "TrailingRange(50)"} {
		// Not containment away from Fixed, for the reason above, but the
		// counts still differ by a wide factor. The failure this catches is a
		// point count that makes the larger set judge rule 1's window, which
		// drives the ratio to 1.
		n1 := countTrue(breachIndex(configs, breaches, "rule 1", baseline))
		n8 := countTrue(breachIndex(configs, breaches, "all eight", baseline))
		if n8 < 3*n1 {
			t.Errorf("%s: all eight breached %d observations against rule 1's %d, want at least three times as many",
				baseline, n8, n1)
		}

		// An estimated baseline carries its own error and false alarms more
		// often than a known one. The measured ratio is around 0.62 to 0.72;
		// the margin is 0.9, which is roughly three seed-to-seed standard
		// deviations away. Trailing(200) is deliberately not compared: its
		// gap to Fixed is about 1.4 standard deviations, and asserting an
		// ordering that thin would be encoding sampling noise as a law.
		if baseline != "Trailing(50)" {
			continue
		}
		for _, rules := range []string{"rule 1", "rules 1,2", "all eight"} {
			if est, known := arlOf(rules, "Trailing(50)"), arlOf(rules, "Fixed"); float64(est) > 0.9*float64(known) {
				t.Errorf("%s: Trailing(50)=%d against Fixed=%d, want appreciably noisier",
					rules, est, known)
			}
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
func measure(t *testing.T, c arlConfig, series []float64) (arlRow, []bool) {
	t.Helper()
	cond := spc.Nelson(c.b, c.ref, c.set)
	hinter, ok := cond.(alarm.PointsHinter)
	if !ok {
		t.Fatalf("%s / %s: the condition no longer declares MinPoints, so the window cannot be sized", c.rules, c.baseline)
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
		t.Fatalf("%s / %s: no false alarm in %d evaluations, which no rule set does on an in-control process",
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
	switch c.rules {
	case "rule 1":
		if f := longFraction(episodes, 3); f > 0.01 {
			t.Errorf("rule 1 / %s: %.2f%% of episodes survive For=2, want under 1%% — "+
				"episodes are probably not being counted as runs of consecutive breaches",
				c.baseline, 100*f)
		}
	case "all eight":
		if f := longFraction(episodes, 4); f < 0.02 {
			t.Errorf("all eight / %s: only %.2f%% of episodes survive For=3, want at least 2%% — "+
				"the episode counter is probably recording lengths, not runs", c.baseline, 100*f)
		}
	}

	return arlRow{
		rules:    c.rules,
		baseline: c.baseline,
		cols:     [3]int{arlAt(evals, episodes, 0), arlAt(evals, episodes, 2), arlAt(evals, episodes, 3)},
	}, verdicts
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

// arlRow is one line of the documented table. A column of -1 renders as
// "never".
type arlRow struct {
	rules    string
	baseline string
	cols     [3]int
}

const (
	arlHeader = "rules      baseline             For=0   For=2   For=3"
	arlFormat = "%-11s%-18s%8s%8s%8s"
)

func renderARLTable(rows []arlRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "//\t%s\n", arlHeader)
	for _, r := range rows {
		cells := make([]string, 3)
		for i, c := range r.cols {
			if c < 0 {
				cells[i] = "never"
			} else {
				cells[i] = strconv.Itoa(c)
			}
		}
		fmt.Fprintf(&b, "//\t"+arlFormat+"\n", r.rules, r.baseline, cells[0], cells[1], cells[2])
	}
	return b.String()
}

const docPath = "doc.go"

// readARLTable returns the table block from doc.go verbatim, from the header
// line to the last row.
func readARLTable() (string, error) {
	lines, start, end, err := findARLTable()
	if err != nil {
		return "", err
	}
	return strings.Join(lines[start:end], "\n") + "\n", nil
}

func replaceARLTable(rendered string) error {
	lines, start, end, err := findARLTable()
	if err != nil {
		return err
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, strings.Split(strings.TrimRight(rendered, "\n"), "\n")...)
	out = append(out, lines[end:]...)
	return os.WriteFile(docPath, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

func findARLTable() (lines []string, start, end int, err error) {
	f, err := os.Open(docPath)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, err
	}
	start = -1
	for i, l := range lines {
		if l == "//\t"+arlHeader {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, 0, 0, fmt.Errorf("no table header %q in %s", arlHeader, docPath)
	}
	end = start + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "//\t") {
		end++
	}
	return lines, start, end, nil
}
