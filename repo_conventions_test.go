package agentinfra_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tab width is 4, matching golangci-lint's lll default. See issue #181.
// If you change one, change the other — the dual-enforcement model only
// works when both checks agree on what counts as a column.

func TestRepoGoFiles_Max100Columns(t *testing.T) {
	violations, err := findGoLineLengthViolations(100)
	if err != nil {
		t.Fatalf("findGoLineLengthViolations: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("found line-length violations:\n%s", strings.Join(violations, "\n"))
	}
}

// lineColumns returns the rendered column count of line, treating each
// tab as tabWidth columns. Matches the convention golangci-lint's lll
// linter uses by default. Without this, a leading-tab line that lll
// flags can pass the in-test check, or vice-versa — see issue #181.
func lineColumns(line string, tabWidth int) int {
	cols := 0
	for _, r := range line {
		if r == '\t' {
			cols += tabWidth
		} else {
			cols++
		}
	}
	return cols
}

func findGoLineLengthViolations(max int) ([]string, error) {
	const tabWidth = 4
	roots := []string{"cmd", "internal", "examples"}
	var out []string
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if filepath.Ext(path) != ".go" {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			s := bufio.NewScanner(file)
			lineNo := 0
			for s.Scan() {
				lineNo++
				line := s.Text()
				if cols := lineColumns(line, tabWidth); cols > max {
					out = append(out, fmt.Sprintf("%s:%d (%d cols)", path, lineNo, cols))
				}
			}
			scanErr := s.Err()
			closeErr := file.Close()
			if scanErr != nil {
				return scanErr
			}
			return closeErr
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// TestLineColumns_TabWidthMatchesLll guards the convention #181 pinned:
// a line of 25 leading tabs followed by 1 space renders as 101 columns
// when tabs are 4 cols wide (25*4 + 1 = 101) and so should violate the
// 100-col limit. Pre-#181 the in-test check counted tabs as 1, so the
// same line came out as 26 cols and silently passed.
func TestLineColumns_TabWidthMatchesLll(t *testing.T) {
	line := strings.Repeat("\t", 25) + " "
	if got, want := lineColumns(line, 4), 101; got != want {
		t.Fatalf("lineColumns(25 tabs + 1 space, tabWidth=4) = %d, want %d", got, want)
	}
	// And the inverse: under tabs=1 the same line is 26 cols, which is
	// what made the dual-enforcement mismatch possible.
	if got, want := lineColumns(line, 1), 26; got != want {
		t.Fatalf("lineColumns(25 tabs + 1 space, tabWidth=1) = %d, want %d", got, want)
	}
}
