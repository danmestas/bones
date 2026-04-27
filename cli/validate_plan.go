// Package cli holds embeddable Kong commands for the bones binary.
//
// Each command type is a Kong-tagged struct with a Run method. The
// command tree is assembled in cmd/bones/cli.go alongside libfossil/cli
// and EdgeSync/cli.
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	libfossilcli "github.com/danmestas/libfossil/cli"
)

// ValidatePlanCmd parses a Markdown plan, extracts [slot: name]
// annotations, and verifies:
//  1. Every Task heading has a [slot: name].
//  2. Slots are directory-disjoint (no two slots share a directory prefix).
//  3. Each task's Files: paths begin with the slot's owned directory.
//
// Exits 0 if valid, 1 if violations are reported. With --list-slots,
// also emits a JSON slot→task mapping to stdout on success.
type ValidatePlanCmd struct {
	Path      string `arg:"" type:"existingfile" help:"Markdown plan path"`
	ListSlots bool   `name:"list-slots" help:"emit JSON slot→task list (still runs validation)"`
}

func (c *ValidatePlanCmd) Run(g *libfossilcli.Globals) error {
	tasks, violations, err := validatePlan(c.Path)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, v)
		}
		os.Exit(1)
	}
	if c.ListSlots {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(buildSlotList(tasks))
	}
	return nil
}

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

func validatePlan(path string) ([]taskInfo, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

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
		return nil, nil, err
	}
	return tasks, checkTasks(tasks), nil
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

type slotEntry struct {
	Name  string   `json:"name"`
	Tasks []string `json:"tasks"`
}

type slotList struct {
	Slots []slotEntry `json:"slots"`
}

func buildSlotList(tasks []taskInfo) slotList {
	seen := map[string]int{}
	entries := []slotEntry{}
	for _, t := range tasks {
		if t.slot == "" {
			continue
		}
		idx, ok := seen[t.slot]
		if !ok {
			idx = len(entries)
			seen[t.slot] = idx
			entries = append(entries, slotEntry{Name: t.slot})
		}
		entries[idx].Tasks = append(entries[idx].Tasks, t.heading)
	}
	return slotList{Slots: entries}
}
