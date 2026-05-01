package dispatch

// plan_parser.go contains a minimal plan-markdown parser adapted from
// cli/validate_plan.go. Intentional duplication: importing cli from
// internal/dispatch would create a dependency inversion (internal
// depending on the command layer). A shared internal/plan package is
// deferred until a second consumer materializes.

import (
	"bufio"
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	dispTaskHeading = regexp.MustCompile(`^###\s+Task\s+\d+`)
	dispPhaseSlot   = regexp.MustCompile(`\[slot:\s*([a-z][a-z0-9_-]*)\]`)
	dispFilesLine   = regexp.MustCompile(`^\s*-\s+(?:Create|Modify|Test):\s+(\S+)`)
)

// parsedSlot is the minimal view of a slot extracted from a plan file.
type parsedSlot struct {
	name           string
	title          string
	files          []string
	sourceMarkdown string
}

// rawTask is an individual task block parsed from the plan markdown.
type rawTask struct {
	heading string
	slot    string
	files   []string
	lines   []string // raw markdown lines for this task block
}

// parsePlanSlots extracts per-slot information from a Markdown plan.
// The first argument (path) is unused and reserved for future use.
// data is the raw file bytes.
//
// Each slot collapses all its tasks; duplicate files are de-duplicated.
// Slots are returned in first-seen order.
func parsePlanSlots(_ string, data []byte) ([]parsedSlot, error) {
	tasks, err := scanRawTasks(data)
	if err != nil {
		return nil, err
	}
	return aggregateSlots(tasks), nil
}

// scanRawTasks parses the markdown bytes into a flat list of task blocks.
func scanRawTasks(data []byte) ([]rawTask, error) {
	var tasks []rawTask
	var current *rawTask
	inFiles := false

	scan := bufio.NewScanner(bytes.NewReader(data))
	for scan.Scan() {
		line := scan.Text()
		if dispTaskHeading.MatchString(line) {
			if current != nil {
				tasks = append(tasks, *current)
			}
			t := rawTask{heading: strings.TrimSpace(line)}
			if m := dispPhaseSlot.FindStringSubmatch(line); m != nil {
				t.slot = m[1]
			}
			t.lines = append(t.lines, line)
			current = &t
			inFiles = false
			continue
		}
		if current == nil {
			continue
		}
		current.lines = append(current.lines, line)
		if strings.HasPrefix(strings.TrimSpace(line), "**Files:**") {
			inFiles = true
			continue
		}
		if inFiles {
			if m := dispFilesLine.FindStringSubmatch(line); m != nil {
				current.files = append(current.files, m[1])
				continue
			}
			if strings.TrimSpace(line) != "" {
				inFiles = false
			}
		}
	}
	if current != nil {
		tasks = append(tasks, *current)
	}
	return tasks, scan.Err()
}

// aggregateSlots collapses rawTask entries into per-slot parsedSlot values.
// Slots are returned in first-seen order; files are de-duplicated within each slot.
func aggregateSlots(tasks []rawTask) []parsedSlot {
	seenIdx := map[string]int{}
	var slots []parsedSlot
	for _, t := range tasks {
		if t.slot == "" {
			continue
		}
		idx, ok := seenIdx[t.slot]
		if !ok {
			idx = len(slots)
			seenIdx[t.slot] = idx
			slots = append(slots, parsedSlot{name: t.slot, title: t.slot})
		}
		addFiles(&slots[idx], t.files)
		if slots[idx].title == t.slot {
			slots[idx].title = trimSlotAnnotation(t.heading)
		}
		slots[idx].sourceMarkdown += strings.Join(t.lines, "\n") + "\n"
	}
	return slots
}

// addFiles appends files to a slot, skipping duplicates.
func addFiles(s *parsedSlot, files []string) {
	seen := map[string]bool{}
	for _, f := range s.files {
		seen[f] = true
	}
	for _, f := range files {
		if !seen[f] {
			s.files = append(s.files, f)
			seen[f] = true
		}
	}
}

// trimSlotAnnotation removes the [slot: …] annotation from a heading line.
func trimSlotAnnotation(s string) string {
	cleaned := dispPhaseSlot.ReplaceAllString(s, "")
	cleaned = strings.TrimLeft(cleaned, "#")
	cleaned = strings.TrimRight(strings.TrimSpace(cleaned), ":")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return filepath.Base(s)
	}
	return cleaned
}
