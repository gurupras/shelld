package vt

import "testing"

// A checker that reports nothing is indistinguishable from a correct grid, so
// this drives a row into the state the grow defect produced — a painted tail
// left past occ, then default cells appended after it — and requires CheckOcc
// to name that cell.
func TestCheckOccReportsAViolatedRow(t *testing.T) {
	term := NewTerm(Config{ScrollingHistory: 8}, 6, 2, nil)
	if bad := CheckOcc(term); len(bad) != 0 {
		t.Fatalf("a fresh grid violates the bound: %v", bad)
	}

	// Paint all but the last cell and put occ before the painted ones by hand:
	// a painted tail followed by default cells is what growing a row appended,
	// and no exported operation is supposed to leave a row this way.
	row := term.Grid().Row(0)
	for c := 0; c < row.Len()-1; c++ {
		*row.at(c) = Cell{bg: 42}
	}
	row.occ = 2

	bad := CheckOcc(term)
	if len(bad) != 1 {
		t.Fatalf("CheckOcc reported %d violations, want 1: %v", len(bad), bad)
	}
	if bad[0].Line != 0 || bad[0].Column != 2 || bad[0].Occ != 2 {
		t.Errorf("reported %+v, want line 0 column 2 at occ 2", bad[0])
	}
	if bad[0].String() == "" {
		t.Error("the violation has no description")
	}

	// A row whose cells past occ all equal its last cell holds the bound, even
	// though none of them is the default cell: that is what the bound says, and
	// reporting those would fail on every row a program painted and cleared.
	*row.at(row.Len() - 1) = Cell{bg: 42}
	row.occ = 0
	if bad := CheckOcc(term); len(bad) != 0 {
		t.Errorf("a uniformly painted row is reported: %v", bad)
	}
}
