// Command orchestrator-validate-plan parses a Markdown plan, extracts
// [slot: name] annotations, and verifies:
//  1. Every Task heading has a [slot: name].
//  2. Slots are directory-disjoint (no two slots share a directory prefix).
//  3. Each task's Files: paths begin with the slot's owned directory.
//
// Exits with status 0 if valid, 1 if violations are reported.
package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	taskHeading = regexp.MustCompile(`^###\s+Task\s+\d+`)
	phaseSlot   = regexp.MustCompile(`\[slot:\s*([a-z][a-z0-9_-]*)\]`)
	filesLine   = regexp.MustCompile(`^\s*-\s+(?:Create|Modify|Test):\s+(\S+)`)
)

type taskInfo struct {
	heading string
	slot    string
	files   []string
	line    int
}

func validate(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tasks := []taskInfo{}
	var current *taskInfo
	inFiles := false
	lineNo := 0
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		lineNo++
		line := scan.Text()
		if taskHeading.MatchString(line) {
			if current != nil {
				tasks = append(tasks, *current)
			}
			t := taskInfo{heading: strings.TrimSpace(line), line: lineNo}
			if m := phaseSlot.FindStringSubmatch(line); m != nil {
				t.slot = m[1]
			}
			current = &t
			inFiles = false
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "**Files:**") {
			inFiles = true
			continue
		}
		if inFiles {
			if m := filesLine.FindStringSubmatch(line); m != nil {
				current.files = append(current.files, m[1])
				continue
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			inFiles = false
		}
	}
	if current != nil {
		tasks = append(tasks, *current)
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	return checkTasks(tasks), nil
}

func checkTasks(tasks []taskInfo) []string {
	violations := []string{}
	for _, t := range tasks {
		if t.slot == "" {
			violations = append(violations, fmt.Sprintf(
				"line %d: missing [slot: name] on %q", t.line, t.heading,
			))
		}
	}
	// Each slot's owned directory is the top-level directory of its first
	// listed file. Files that don't share that top dir are "outside" the slot
	// directory. Two slots claiming the same top dir overlap.
	slotDirs := map[string]string{}
	for _, t := range tasks {
		if t.slot == "" || len(t.files) == 0 {
			continue
		}
		dir := topDir(t.files[0])
		if existing, ok := slotDirs[t.slot]; ok && existing != dir {
			violations = append(violations, fmt.Sprintf(
				"line %d: slot %q used for both %q and %q",
				t.line, t.slot, existing, dir,
			))
		} else {
			slotDirs[t.slot] = dir
		}
	}
	dirOwners := map[string]string{}
	for slot, dir := range slotDirs {
		if other, ok := dirOwners[dir]; ok && other != slot {
			violations = append(violations, fmt.Sprintf(
				"slot %q overlap with %q: both own directory %q",
				slot, other, dir,
			))
		} else {
			dirOwners[dir] = slot
		}
	}
	// "Outside slot directory" means a file's top dir doesn't match the slot
	// name. The slot name is the contract; the file path must respect it.
	for _, t := range tasks {
		if t.slot == "" {
			continue
		}
		for _, f := range t.files {
			if topDir(f) != t.slot {
				violations = append(violations, fmt.Sprintf(
					"line %d: file %q outside slot directory %q (slot=%s)",
					t.line, f, t.slot, t.slot,
				))
			}
		}
	}
	return violations
}

func topDir(p string) string {
	if i := strings.IndexAny(p, "/\\"); i >= 0 {
		return p[:i]
	}
	return p
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: orchestrator-validate-plan <plan.md>")
		os.Exit(2)
	}
	violations, err := validate(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, v := range violations {
		fmt.Println(v)
	}
	if len(violations) > 0 {
		os.Exit(1)
	}
}
