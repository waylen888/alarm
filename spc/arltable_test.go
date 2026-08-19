package spc_test

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The table of false-alarm rates that lives in the package documentation, and
// the machinery that keeps it and the measurement in agreement. The table in
// doc.go is the golden file: there is no second copy here to drift away from
// it, and TestFalseAlarmRates rewrites it under -update.
//
// It is read-only. An earlier version rewrote doc.go in place under an
// -update flag, and every defect the last two reviews found in this package
// was downstream of that writer: it wrote a row of zeros when a measurement
// failed, it deleted an ordinary comment written under the table, and the
// test that claimed to guard its atomicity passed when the atomicity was
// taken out. A table that moves twice a year does not need a writer — the
// measurement prints the new block and a person pastes it.

// arlRow is one line of the table. A column of -1 renders as "never".
//
// filled distinguishes a row a measurement produced from the zero value a
// failed subtest leaves behind. Without it, -update wrote a row of zeros into
// doc.go: the measurement's preconditions used t.Fatalf, which Goexits out of
// the parallel subtest, so the caller's assignment never ran and the caller
// had no way to tell the slot from a real one.
type arlRow struct {
	filled   bool
	rules    string
	baseline string
	cols     [3]int
}

// The table's geometry, in one place. The format and the header are derived
// from the same widths, so a column cannot widen in the rows without widening
// in the header — they used to be two constants that had to agree with each
// other and with doc.go, checked by nothing.
const (
	arlPrefix        = "//\t"
	arlRulesWidth    = 11
	arlBaselineWidth = 18
	arlCellWidth     = 8
	arlRowWidth      = arlRulesWidth + arlBaselineWidth + 3*arlCellWidth
)

var (
	arlFormat = fmt.Sprintf("%%-%ds%%-%ds%%%ds%%%ds%%%ds",
		arlRulesWidth, arlBaselineWidth, arlCellWidth, arlCellWidth, arlCellWidth)
	arlHeader = fmt.Sprintf(arlFormat, "rules", "baseline", "For=0", "For=2", "For=3")
)

// docPath is a variable so the guards below can be exercised against a
// throwaway copy in a temporary directory. The measurement itself always uses
// the real file, and go test runs a package's tests in its source directory.
var docPath = "doc.go"

func renderARLTable(rows []arlRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s\n", arlPrefix, arlHeader)
	for _, r := range rows {
		cells := make([]string, len(r.cols))
		for i, c := range r.cols {
			if c < 0 {
				cells[i] = "never"
			} else {
				cells[i] = strconv.Itoa(c)
			}
		}
		fmt.Fprintf(&b, arlPrefix+arlFormat+"\n", r.rules, r.baseline, cells[0], cells[1], cells[2])
	}
	return b.String()
}

// parseARLRow is the exact inverse of one rendered line. It slices by column
// rather than splitting on whitespace, because a rule set's name is not one
// token: "rule 1" is two fields and "rules 1,2" is one, so a split would have
// to guess where the name ends.
//
// The width check is load-bearing. A name longer than its column does not
// truncate under %-11s, it pushes the rest of the line right, and the round
// trip turns that into a failure where the change was made rather than a
// table whose columns no longer line up with their own header.
func parseARLRow(line string) (arlRow, error) {
	body, ok := strings.CutPrefix(line, arlPrefix)
	if !ok {
		return arlRow{}, fmt.Errorf("%q does not begin with a comment and a tab", line)
	}
	if len(body) != arlRowWidth {
		return arlRow{}, fmt.Errorf("%q is %d columns wide, want %d — it does not match the row format",
			body, len(body), arlRowWidth)
	}
	r := arlRow{
		filled:   true,
		rules:    strings.TrimSpace(body[:arlRulesWidth]),
		baseline: strings.TrimSpace(body[arlRulesWidth : arlRulesWidth+arlBaselineWidth]),
	}
	if r.rules == "" || r.baseline == "" {
		return arlRow{}, fmt.Errorf("%q has an empty rules or baseline column", body)
	}
	off := arlRulesWidth + arlBaselineWidth
	for i := range r.cols {
		cell := strings.TrimSpace(body[off+i*arlCellWidth : off+(i+1)*arlCellWidth])
		if cell == "never" {
			r.cols[i] = -1
			continue
		}
		n, err := strconv.Atoi(cell)
		if err != nil {
			return arlRow{}, fmt.Errorf("%q: column %d is %q, want a count or \"never\"", body, i, cell)
		}
		if n < 1 {
			// A count is evals/k with k at most evals, so it is at least one.
			// Zero is the zero value and nothing else, which is the row this
			// whole arrangement exists to stop shipping.
			return arlRow{}, fmt.Errorf("%q: column %d is %d, and no measurement produces a count below one", body, i, n)
		}
		r.cols[i] = n
	}
	return r, nil
}

func sameARLRow(a, b arlRow) bool {
	return a.rules == b.rules && a.baseline == b.baseline && a.cols == b.cols
}

// checkARLRoundTrip confirms that what was rendered parses back to the rows
// that produced it. Called before every write, so a change to the format can
// never put a block into doc.go that the parser cannot read — a failure that
// would otherwise land on whoever ran the tests next rather than on whoever
// made the change.
func checkARLRoundTrip(rendered string, rows []arlRow) error {
	lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	if len(lines) != len(rows)+1 {
		return fmt.Errorf("rendered %d lines for %d rows and a header", len(lines), len(rows))
	}
	if lines[0] != arlPrefix+arlHeader {
		return fmt.Errorf("rendered header %q, want %q", lines[0], arlPrefix+arlHeader)
	}
	for i, l := range lines[1:] {
		got, err := parseARLRow(l)
		if err != nil {
			return err
		}
		if !sameARLRow(got, rows[i]) {
			return fmt.Errorf("row %d rendered as %q, which parses back as %s / %s %v",
				i, l, got.rules, got.baseline, got.cols)
		}
	}
	return nil
}

// findARLTable locates the block and returns the rows it holds, insisting on
// exactly one header and exactly wantRows parseable rows.
//
// The block ends by count, not by scanning for the first line that stops
// matching the comment-and-tab prefix. That scan swallowed an ordinary
// tab-indented comment written under the table, and -update then deleted it.
// The count comes from the measurement, which knows how many rows it made, so
// an unrelated indented line below the table is now out of reach by
// construction rather than by luck.
func findARLTable(wantRows int) (lines []string, start, end int, rows []arlRow, err error) {
	f, err := os.Open(docPath)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, nil, err
	}

	// Every match, not the first. Two headers means a table was pasted rather
	// than edited, and rewriting one copy while the reader compares the other
	// is the worst outcome available, because it passes.
	header := arlPrefix + arlHeader
	start = -1
	for i, l := range lines {
		if l != header {
			continue
		}
		if start >= 0 {
			return nil, 0, 0, nil, fmt.Errorf("%s: the table header appears at lines %d and %d; there must be exactly one",
				docPath, start+1, i+1)
		}
		start = i
	}
	if start < 0 {
		return nil, 0, 0, nil, fmt.Errorf("%s: no table header %q; if the column widths just changed, "+
			"doc.go has to be fixed by hand once before -update can find the block again", docPath, arlHeader)
	}

	end = start + 1 + wantRows
	if end > len(lines) {
		return nil, 0, 0, nil, fmt.Errorf("%s: the table has %d rows, want %d — the block is truncated",
			docPath, len(lines)-start-1, wantRows)
	}
	rows = make([]arlRow, 0, wantRows)
	for i := start + 1; i < end; i++ {
		r, err := parseARLRow(lines[i])
		if err != nil {
			return nil, 0, 0, nil, fmt.Errorf("%s:%d: %w", docPath, i+1, err)
		}
		rows = append(rows, r)
	}
	// A further row would sit outside the block, be compared against nothing
	// and survive -update unchanged and wrong. Anything that is not
	// row-shaped is what is supposed to be there and is left alone.
	if end < len(lines) {
		if _, err := parseARLRow(lines[end]); err == nil {
			return nil, 0, 0, nil, fmt.Errorf("%s:%d: a table row follows the %d-row block; "+
				"the table and the measurement disagree on how many configurations there are",
				docPath, end+1, wantRows)
		}
	}
	return lines, start, end, rows, nil
}

// readARLTable returns the block verbatim together with the rows parsed from
// it. The text is compared as well as the rows, because the parser normalises
// away exactly the drift — a column that has lost its alignment — that the
// text comparison catches.
func readARLTable(wantRows int) (string, []arlRow, error) {
	lines, start, end, rows, err := findARLTable(wantRows)
	if err != nil {
		return "", nil, err
	}
	return strings.Join(lines[start:end], "\n") + "\n", rows, nil
}
