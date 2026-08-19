package spc_test

import (
	"os"
	"path/filepath"
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
