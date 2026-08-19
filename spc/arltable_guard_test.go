package spc_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The guards on the table machinery, exercised against a throwaway copy of
// doc.go rather than against the measurement, so they run in short mode. Each
// one exists because of a specific way -update corrupted the documentation or
// could have.

func withDocCopy(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.go")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := docPath
	docPath = path
	t.Cleanup(func() { docPath = old })
}

// docWith builds a minimal doc.go around a table block.
func docWith(block string) string {
	return "// Package spc.\n//\n" + block + "//\n// Prose after the table.\npackage spc\n"
}

var guardRows = []arlRow{
	{filled: true, rules: "rule 1", baseline: "Fixed", cols: [3]int{347, -1, -1}},
	{filled: true, rules: "all eight", baseline: "Trailing(50)", cols: [3]int{47, 393, 727}},
}

func TestARLTableRoundTrips(t *testing.T) {
	rendered := renderARLTable(guardRows)
	if err := checkARLRoundTrip(rendered, guardRows); err != nil {
		t.Fatalf("the renderer and the parser disagree: %v", err)
	}
	withDocCopy(t, docWith(rendered))
	got, rows, err := readARLTable(len(guardRows))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got != rendered {
		t.Errorf("read back %q, wrote %q", got, rendered)
	}
	for i, want := range guardRows {
		if !sameARLRow(rows[i], want) {
			t.Errorf("row %d parsed as %s / %s %v", i, rows[i].rules, rows[i].baseline, rows[i].cols)
		}
	}
}

// The defect this whole arrangement exists to stop shipping: a row of zeros,
// which is what a Goexit out of a parallel subtest leaves behind.
func TestARLTableRejectsAZeroRow(t *testing.T) {
	if err := checkMeasured(
		[]arlConfig{{rules: "rule 1", baseline: "Fixed"}},
		[]arlRow{{}},
	); err == nil {
		t.Error("an unfilled row must not be accepted")
	}
	if err := checkMeasured(
		[]arlConfig{{rules: "rule 1", baseline: "Fixed"}},
		[]arlRow{{filled: true, rules: "rule 1", baseline: "Fixed"}},
	); err == nil {
		t.Error("a filled row of zero counts must not be accepted")
	}
	// And from the other direction: a zero row already committed to doc.go.
	if _, err := parseARLRow(renderARLTable([]arlRow{{filled: true, rules: "x", baseline: "y"}})[len(arlPrefix+arlHeader)+1:]); err == nil {
		t.Error("a zero count must not parse")
	}
}

// A measurement written into the wrong slot keeps every row filled and every
// count plausible, so only the labels catch it.
func TestARLTableRejectsAMisplacedRow(t *testing.T) {
	configs := []arlConfig{{rules: "a", baseline: "b"}, {rules: "c", baseline: "d"}}
	rows := []arlRow{
		{filled: true, rules: "c", baseline: "d", cols: [3]int{1, 1, 1}},
		{filled: true, rules: "a", baseline: "b", cols: [3]int{1, 1, 1}},
	}
	if err := checkMeasured(configs, rows); err == nil {
		t.Error("rows in the wrong slots must not be accepted")
	}
}

// The over-run that deleted an ordinary comment written under the table.
func TestARLTableLeavesAnAdjacentCommentAlone(t *testing.T) {
	const neighbour = "//\tSee also the EWMA section.\n"
	rendered := renderARLTable(guardRows)
	withDocCopy(t, docWith(rendered+neighbour))

	if err := replaceARLTable(rendered, len(guardRows)); err != nil {
		t.Fatalf("replacing: %v", err)
	}
	after, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), neighbour) {
		t.Errorf("the comment below the table was deleted:\n%s", after)
	}
}

// A thirteenth row sits outside a twelve-row block, would be compared against
// nothing, and would survive -update unchanged and wrong.
func TestARLTableRejectsAnExtraRow(t *testing.T) {
	extra := renderARLTable([]arlRow{{filled: true, rules: "extra", baseline: "Fixed", cols: [3]int{1, 1, 1}}})
	withDocCopy(t, docWith(renderARLTable(guardRows)+strings.TrimPrefix(extra, "//\t"+arlHeader+"\n")))
	if _, _, err := readARLTable(len(guardRows)); err == nil {
		t.Error("a row beyond the block must be an error, not something to ignore")
	}
}

func TestARLTableRejectsADuplicateHeader(t *testing.T) {
	rendered := renderARLTable(guardRows)
	withDocCopy(t, docWith(rendered)+"\n"+docWith(rendered))
	if _, _, err := readARLTable(len(guardRows)); err == nil {
		t.Error("two tables must be an error; rewriting one while comparing the other passes and is wrong")
	}
}

func TestARLTableRejectsATruncatedBlock(t *testing.T) {
	withDocCopy(t, docWith(renderARLTable(guardRows[:1])))
	if _, _, err := readARLTable(len(guardRows)); err == nil {
		t.Error("a block with too few rows must be an error")
	}
}

// The write must be atomic and must keep the file's mode: an interrupted
// -update used to leave doc.go truncated and unbuildable.
func TestARLTableWritePreservesMode(t *testing.T) {
	rendered := renderARLTable(guardRows)
	withDocCopy(t, docWith(rendered))
	if err := os.Chmod(docPath, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := replaceARLTable(rendered, len(guardRows)); err != nil {
		t.Fatalf("replacing: %v", err)
	}
	info, err := os.Stat(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o664 {
		t.Errorf("mode is %v after the rewrite, want 0664", got)
	}
	// No temporary file left behind for the go tool to try to build.
	entries, err := os.ReadDir(filepath.Dir(docPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".doc.go.tmp-") {
			t.Errorf("a temporary file survived the rewrite: %s", e.Name())
		}
	}
}

// The figures the prose quotes outside the table are duplicated across six
// files and two languages, and the golden table guards none of them. This
// does: if a measurement moves, every place that repeats it has to move too.
//
// It is deliberately read-only and deliberately loose — it asserts the number
// is present, not where or why — because the failure it exists to catch is a
// figure that was updated in one file and forgotten in the others.
func TestQuotedFiguresAppearWhereTheyAreQuoted(t *testing.T) {
	if testing.Short() {
		t.Skip("reads the measurement out of doc.go, which the long test maintains")
	}
	_, rows, err := readARLTable(len(arlConfigs()))
	if err != nil {
		t.Fatalf("reading the table out of doc.go: %v", err)
	}
	cell := func(rules, baseline string) int {
		for _, r := range rows {
			if r.rules == rules && r.baseline == baseline {
				return r.cols[0]
			}
		}
		t.Fatalf("no row for %s / %s", rules, baseline)
		return 0
	}

	quoted := []struct {
		what  string
		value int
		files []string
	}{
		{"all eight over Trailing(50)", cell("all eight", "Trailing(50)"),
			[]string{"doc.go", "nelson.go", "conditions.go", "example_test.go", "../README.md", "../README.zh-TW.md"}},
		{"rule 1 over Trailing(50)", cell("rule 1", "Trailing(50)"),
			[]string{"doc.go", "nelson.go", "conditions.go", "example_test.go", "../README.md", "../README.zh-TW.md"}},
		{"rule 1 over Fixed", cell("rule 1", "Fixed"), []string{"doc.go"}},
		{"all eight over Fixed", cell("all eight", "Fixed"), []string{"doc.go"}},
	}
	for _, q := range quoted {
		re := regexp.MustCompile(`\b` + strconv.Itoa(q.value) + `\b`)
		for _, f := range q.files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if !re.Match(b) {
				t.Errorf("%s no longer mentions %d, the measured rate for %s — "+
					"the figure moved and this file was not updated with it", f, q.value, q.what)
			}
		}
	}
}
