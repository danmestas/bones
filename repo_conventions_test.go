package agentinfra_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoGoFiles_Max100Columns(t *testing.T) {
	violations, err := findGoLineLengthViolations(100)
	if err != nil {
		t.Fatalf("findGoLineLengthViolations: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("found line-length violations:\n%s", strings.Join(violations, "\n"))
	}
}

func findGoLineLengthViolations(max int) ([]string, error) {
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
				if len(line) > max {
					out = append(out, fmt.Sprintf("%s:%d (%d chars)", path, lineNo, len(line)))
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
