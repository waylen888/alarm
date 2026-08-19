package spc_test

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The measured figures this repository quotes in prose, and the sentence each
// one lives in.
//
// The sigma distances the baseline comparison quotes are checked next to
// their own measurement, in baseline_test.go, because that is where the one
// copy of those numbers lives.
//
// Most such figures were removed rather than guarded: a rate repeated in five
// godoc comments is a maintenance hazard whose cheapest fix is not to repeat
// it. What is left is the reader-facing argument — the two READMEs, where a
// pointer to the package documentation would be materially worse than the
// number, and doc.go's own prose, which reasons from cells of its table.
//
// Each claim is a phrase, not a bare number. A bare \b47\b passes on a file
// that still says 47 when the measurement moved to 50, because Trailing(50)
// contains a 50 between two non-word characters; and \b65\b cannot tell the
// rate for all eight over Fixed from the MinPoints 65 in README.md, which is
// 50+15 and must not move with it. Both were live defects in the check this
// replaces.
//
// It runs in short mode. It reads committed files and matches regexps.

// flatten joins wrapped lines and strips comment and list markers, so a
// pattern can be written the way the sentence reads. Every quoted sentence
// here wraps somewhere between a figure and its noun: grep for "once every 47
// observations" in README.md and the answer is zero.
//
// Lines join with a space only when the characters on either side of the
// break are both ASCII. Traditional Chinese wraps without one, and
// README.zh-TW.md breaks mid-sentence in exactly that way.
func flatten(b []byte) string {
	var out []rune
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "//")
		line = strings.TrimPrefix(line, "- ")
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(out) > 0 && !isCJK(out[len(out)-1]) && !isCJK(r[0]) {
			out = append(out, ' ')
		}
		out = append(out, r...)
	}
	return string(out)
}

func TestDocumentedFigures(t *testing.T) {
	_, rows, err := readARLTable(len(arlConfigs()))
	if err != nil {
		t.Fatalf("reading the table out of doc.go: %v", err)
	}
	cell := func(rules, baseline string, col int) int {
		for _, r := range rows {
			if r.rules == rules && r.baseline == baseline {
				return r.cols[col]
			}
		}
		t.Fatalf("no row for %s / %s", rules, baseline)
		return 0
	}
	allTrail := cell("all eight", "Trailing(50)", 0)
	oneTrail := cell("rule 1", "Trailing(50)", 0)
	allFixed := cell("all eight", "Fixed", 0)
	oneFixed := cell("rule 1", "Fixed", 0)

	claims := []struct{ file, why, pattern string }{
		// doc.go's prose against doc.go's table. Not circular: the table
		// cannot satisfy these sentences, only the prose can.
		{"doc.go", "the Fixed-baseline comparison",
			fmt.Sprintf(`false-alarms once every %d observations, against %d for rule 1 on the same baseline`, allFixed, oneFixed)},
		{"doc.go", "the read-it-as-a-ratio paragraph",
			fmt.Sprintf(`depends on %d rather than \d+; it depends on %d being an order of magnitude below %d, on %d to %d being a factor of about 1\.4`,
				allTrail, allTrail, oneFixed, allFixed, allTrail)},

		{"../README.md", "the required-rules bullet",
			fmt.Sprintf(`false-alarms once every %d observations on an in-control process against %d for rule 1 alone`, allTrail, oneTrail)},

		{"../README.zh-TW.md", "the required-rules bullet",
			fmt.Sprintf(`每 %d 筆誤報一次，只用規則 1 則是每 %d 筆`, allTrail, oneTrail)},
	}

	texts := map[string]string{}
	for _, c := range claims {
		if _, ok := texts[c.file]; ok {
			continue
		}
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		texts[c.file] = flatten(b)
	}
	for _, c := range claims {
		if !regexp.MustCompile(c.pattern).MatchString(texts[c.file]) {
			t.Errorf("%s: %s no longer reads as the measurement says it should.\n"+
				"want a sentence matching:\n\t%s\n"+
				"A figure moved and this file was not updated with it.", c.file, c.why, c.pattern)
		}
	}

	// The ratio claims are claims about the measurement rather than about the
	// digits, so they are checked as arithmetic. A shift that leaves every
	// sentence above matching can still falsify these.
	if got := math.Round(float64(allFixed)/float64(allTrail)*10) / 10; got != 1.4 {
		t.Errorf("doc.go says estimating the baseline costs a factor of about 1.4; %d to %d is %.1f",
			allFixed, allTrail, got)
	}
	if got := float64(oneFixed) / float64(allTrail); got < 5 {
		t.Errorf("doc.go says %d is an order of magnitude below %d; the ratio is %.1f", allTrail, oneFixed, got)
	}
}

// The two READMEs are one document in two languages and they have drifted
// apart before. A figure updated in English and forgotten in Chinese is the
// failure the per-file patterns above cannot see, because each one only
// proves its own file is self-consistent.
func TestREADMEsAreParallel(t *testing.T) {
	section := func(path, heading string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_, rest, ok := strings.Cut(string(b), heading)
		if !ok {
			t.Fatalf("%s: no heading %q", path, heading)
		}
		if end := strings.Index(rest, "\n## "); end >= 0 {
			rest = rest[:end]
		}
		return rest
	}
	en := section("../README.md", "\n## Statistical process control: the spc subpackage")
	zh := section("../README.zh-TW.md", "\n## 統計製程管制：spc 子套件")

	if a, b := strings.Count(en, "\n- **"), strings.Count(zh, "\n- **"); a != b {
		t.Errorf("the spc section has %d bullets in README.md and %d in README.zh-TW.md", a, b)
	}
	num := regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)
	tally := func(s string) map[string]int {
		m := map[string]int{}
		for _, n := range num.FindAllString(s, -1) {
			m[n]++
		}
		return m
	}
	ten, tzh := tally(en), tally(zh)
	for n, c := range ten {
		if tzh[n] != c {
			t.Errorf("README.md quotes %q %d time(s) in the spc section, README.zh-TW.md %d", n, c, tzh[n])
		}
	}
	for n, c := range tzh {
		if ten[n] == 0 {
			t.Errorf("README.zh-TW.md quotes %q %d time(s) in the spc section, README.md never", n, c)
		}
	}
}

// isCJK reports whether r is a character that wraps without a space. The test
// is not "not ASCII": the documentation writes sigma distances as "2.2σ", and
// σ is Greek, so a not-ASCII rule joined "1.8σ" to the next line with no
// space and every pattern stopped matching.
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x303F) || // CJK symbols and punctuation
		(r >= 0xFF00 && r <= 0xFFEF) // fullwidth forms
}
