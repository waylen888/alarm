package spc

import (
	"math/rand"
	"strconv"
	"testing"
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
// column is the same episode count restricted to episodes long enough to
// survive a Rule.For of that many sampling intervals.
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
		rules []Rule
	}{
		{"rule 1", []Rule{Rule1}},
		{"rules 1,2", []Rule{Rule1, Rule2}},
		{"all eight", nil},
	}
	baselines := []struct {
		name string
		ref  int
		b    Baseline
	}{
		{"Fixed", 50, Fixed(100, 1)},
		{"Trailing(50)", 50, Trailing(50)},
		{"Trailing(200)", 200, Trailing(200)},
		{"TrailingRange(50)", 50, TrailingRange(50)},
	}

	t.Logf("%-11s %-18s %10s %10s %10s %10s", "rules", "baseline", "For=0", "For=2", "For=3", "per obs")
	for _, rs := range rulesets {
		test := 1
		for _, x := range rs.rules {
			if p := x.Points(); p > test {
				test = p
			}
		}
		if len(rs.rules) == 0 {
			test = 15
		}
		for _, bl := range baselines {
			var episodes []int
			evals, breaches, run := 0, 0, 0
			for i := bl.ref + test; i <= n; i++ {
				w := series[i-bl.ref-test:]
				c, s, ok := bl.b.Estimate(w[:bl.ref])
				if !ok {
					continue
				}
				evals++
				tv := w[bl.ref : bl.ref+test]
				last := len(tv) - 1
				fired := false
				for _, v := range Check(tv, c, s, rs.rules...) {
					if v.Index == last {
						fired = true
						break
					}
				}
				if fired {
					breaches++
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
			perObs := "never"
			if breaches > 0 {
				perObs = strconv.Itoa(evals / breaches)
			}
			t.Logf("%-11s %-18s %10s %10s %10s %10s",
				rs.name, bl.name, every(1), every(3), every(4), perObs)
		}
	}
}
