package spc_test

import (
	"math"
	"math/rand"
	"strconv"
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

	const n = 200000
	series := make([]float64, n)
	r := rand.New(rand.NewSource(1))
	for i := range series {
		series[i] = 100 + r.NormFloat64()
	}

	rulesets := []struct {
		name  string
		rules []spc.Rule
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

	// The table as it appears in doc.go. Asserted within a band: the figures
	// are stable only for this seed and this implementation, but seed noise is
	// around 6% and a real breakage moves a row by a factor. When a deliberate
	// change moves a row, update doc.go and this table together — that
	// coupling is the point of having it here.
	documented := map[string]map[string]int{
		"rule 1":    {"Fixed": 347, "Trailing(50)": 215, "Trailing(200)": 304, "TrailingRange(50)": 186},
		"rules 1,2": {"Trailing(50)": 131},
		"all eight": {"Fixed": 65, "Trailing(50)": 47, "Trailing(200)": 61, "TrailingRange(50)": 45},
	}

	arl := map[string]map[string]int{}
	t.Logf("%-11s %-18s %10s %10s %10s", "rules", "baseline", "For=0", "For=2", "For=3")
	for _, rs := range rulesets {
		arl[rs.name] = map[string]int{}
		for _, bl := range baselines {
			cond := spc.Nelson(bl.b, bl.ref, rs.rules)
			need := cond.(alarm.PointsHinter).MinPoints()

			// The real condition, driven through a real engine, one
			// observation at a time. A probe condition wraps it so the engine
			// sizes and fills the window exactly as it would in production
			// and the verdict recorded is the condition's own.
			var breached bool
			probe := probeCondition{
				points: need,
				judge: func(w alarm.Window) bool {
					breached = cond.Breach(w)
					return false
				},
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			engine := alarm.New(func(alarm.Event) {}, alarm.WithClock(func() time.Time { return now }))
			engine.SetRules([]alarm.Rule{{
				ID:          "measure",
				Levels:      []alarm.Level{{Severity: alarm.SeverityWarn, Condition: probe}},
				StaleAfter:  -1,
				VanishAfter: -1,
			}})

			var episodes []int
			evals, run := 0, 0
			for i, v := range series {
				breached = false
				engine.Observe("measure", "k", v, now)
				now = now.Add(time.Second)
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

			every := func(minLen int) string {
				k := 0
				for _, l := range episodes {
					if l >= minLen {
						k++
					}
				}
				if k == 0 {
					return "never"
				}
				return strconv.Itoa(evals / k)
			}
			if len(episodes) > 0 {
				arl[rs.name][bl.name] = evals / len(episodes)
			}
			t.Logf("%-11s %-18s %10s %10s %10s", rs.name, bl.name, every(1), every(2+1), every(3+1))

			if want, ok := documented[rs.name][bl.name]; ok {
				got := arl[rs.name][bl.name]
				if math.Abs(float64(got-want)) > float64(want)*0.15 {
					t.Errorf("%s / %s: measured one episode every %d observations, "+
						"documented %d — doc.go and this table have diverged",
						rs.name, bl.name, got, want)
				}
			}

			// Rule 1 signals on a single point beyond three sigma, and the
			// chance the next point is also beyond it is 0.0027, so its
			// episodes are one observation long and no For gate survives.
			// An episode counter that recorded lengths rather than runs would
			// empty every For column silently; this is what notices.
			if rs.name == "rule 1" && every(3) != "never" {
				t.Errorf("rule 1 / %s: an episode survived For=2, so episodes are "+
					"not being counted as runs of consecutive breaches", bl.name)
			}
			if rs.name == "all eight" && every(4) == "never" {
				t.Errorf("all eight / %s: no episode survived For=3, so the episode "+
					"counter is recording lengths, not runs", bl.name)
			}
		}
	}

	// Rule 1 against a Fixed baseline is the one row with an answer from
	// outside this code: a three-sigma limit on an in-control normal process
	// has an in-control ARL of 370.4. If this lands far from it, the series
	// generator, the evaluation count or the newest-point gate is wrong — not
	// the library.
	if got := arl["rule 1"]["Fixed"]; got < 300 || got > 450 {
		t.Errorf("rule 1 over Fixed: measured in-control ARL %d, want about 370 "+
			"(Shewhart, three sigma)", got)
	}

	for _, bl := range []string{"Fixed", "Trailing(50)", "Trailing(200)", "TrailingRange(50)"} {
		// Adding rules can only add signals. A wrong point count for the
		// larger set — the sort of thing that makes all eight behave like
		// rule 1 — is invisible to every other check here.
		if arl["all eight"][bl] >= arl["rules 1,2"][bl] || arl["rules 1,2"][bl] >= arl["rule 1"][bl] {
			t.Errorf("%s: adding rules must lower the ARL, got rule1=%d rules1,2=%d all8=%d",
				bl, arl["rule 1"][bl], arl["rules 1,2"][bl], arl["all eight"][bl])
		}
	}
	for _, rs := range []string{"rule 1", "rules 1,2", "all eight"} {
		// An estimated baseline carries its own error and can only false
		// alarm more often than a known one; a longer reference recovers part
		// of the gap. Letting a baseline see the observations under test
		// inverts this, which is exactly what it is here to catch.
		if arl[rs]["Trailing(50)"] >= arl[rs]["Fixed"] {
			t.Errorf("%s: an estimated baseline must false alarm more often than a "+
				"known one, got Trailing(50)=%d Fixed=%d", rs, arl[rs]["Trailing(50)"], arl[rs]["Fixed"])
		}
		if arl[rs]["Trailing(200)"] <= arl[rs]["Trailing(50)"] || arl[rs]["Trailing(200)"] >= arl[rs]["Fixed"] {
			t.Errorf("%s: Trailing(200) must sit between Trailing(50) and Fixed, got %d",
				rs, arl[rs]["Trailing(200)"])
		}
	}
}

// probeCondition observes another condition's verdict without ever breaching
// itself, so the engine drives the real condition without any alert state
// getting in the way of the measurement.
type probeCondition struct {
	points int
	judge  func(alarm.Window) bool
}

func (c probeCondition) Breach(w alarm.Window) bool { return c.judge(w) }

func (c probeCondition) MinPoints() int { return c.points }
