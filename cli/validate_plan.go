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
	"path/filepath"
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
// Output contract (deliberately strict so orchestrator scripts can
// pipe stdout through `jq` / `python -c json.load` without
// stripping prose first):
//
//	stdout : always a single JSON object {errors, slots} — empty
//	         arrays on a clean validation, non-empty Errors on a
//	         violation. Even parse failures emit a JSON object so
//	         consumers don't have to special-case stdout shape.
//	stderr : human-readable prose, one violation per line, on
//	         failures. Empty on a clean run.
//	exit   : 0 = valid, 1 = plan-level violations, 2 = parse / IO error.
//
// --list-slots is retained as a no-op flag for backwards compatibility
// with orchestrator scripts authored against the older "JSON only on
// success when --list-slots set" shape. It emits a one-line stderr
// hint the first time it's used so callers know the flag is
// deprecated; behavior is identical with or without it.
type ValidatePlanCmd struct {
	Path      string `arg:"" type:"existingfile" help:"Markdown plan path"`
	ListSlots bool   `name:"list-slots" help:"deprecated no-op (JSON is always emitted on stdout)"`
}

// ValidateResult is the on-stdout JSON shape emitted by every
// invocation of ValidatePlanCmd. Fields are always present; Errors
// is non-empty iff exit is non-zero. Slots is best-effort: even on
// validation failure we still emit whatever slot annotations the
// parser saw, so an orchestrator that wants to dispatch the valid
// slots alongside reporting the failures has the data without
// re-parsing.
type ValidateResult struct {
	Errors []string    `json:"errors"`
	Slots  []slotEntry `json:"slots"`
}

func (c *ValidatePlanCmd) Run(g *libfossilcli.Globals) error {
	if c.ListSlots {
		fmt.Fprintln(os.Stderr,
			"validate-plan: --list-slots is a no-op; JSON is now always emitted on stdout")
	}
	res, exitCode := runValidatePlan(c.Path)
	if err := emitValidateResult(os.Stdout, res); err != nil {
		return err
	}
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, e)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

// runValidatePlan opens the plan, runs the structural checks, and
// returns the ValidateResult plus an exit code. Pulled out of Run
// so tests can drive the validation surface without forking
// processes; the os.Exit is the only side effect Run adds.
func runValidatePlan(path string) (ValidateResult, int) {
	tasks, violations, err := validatePlan(path)
	if err != nil {
		return ValidateResult{
			Errors: []string{err.Error()},
			Slots:  []slotEntry{},
		}, 2
	}
	list := buildSlotList(tasks)
	if list.Slots == nil {
		list.Slots = []slotEntry{}
	}
	res := ValidateResult{
		Errors: violations,
		Slots:  list.Slots,
	}
	if res.Errors == nil {
		res.Errors = []string{}
	}
	if len(violations) > 0 {
		return res, 1
	}
	return res, 0
}

// emitValidateResult writes the canonical indented-JSON form of res.
// Pulled out so tests can assert on the on-the-wire bytes.
func emitValidateResult(w *os.File, res ValidateResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("validate-plan: encode result: %w", err)
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
		dir := slotDir(t.files[0], t.slot)
		if dir == "" {
			violations = append(violations, fmt.Sprintf(
				"line %d: file %q has no path component matching slot %q",
				t.line, t.files[0], t.slot,
			))
			continue
		}
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
		for otherDir, otherSlot := range dirOwners {
			if otherSlot == slot {
				continue
			}
			if dirOverlap(dir, otherDir) {
				violations = append(violations, fmt.Sprintf(
					"slot %q overlap with %q: %q vs %q",
					slot, otherSlot, dir, otherDir,
				))
			}
		}
		dirOwners[dir] = slot
	}
	for _, t := range tasks {
		if t.slot == "" {
			continue
		}
		want := slotDirs[t.slot]
		for _, f := range t.files {
			got := slotDir(f, t.slot)
			if got != want {
				violations = append(violations, fmt.Sprintf(
					"line %d: file %q outside slot directory %q (slot=%s)",
					t.line, f, want, t.slot,
				))
			}
		}
	}
	return violations
}

// slotDir returns the path to (and including) the LAST component of p
// that equals slotName, e.g. slotDir("src/rendering/renderer.js",
// "rendering") = "src/rendering". Empty when no path component matches
// slotName. Lets slot directories live anywhere in the tree
// ("rendering/" at root, "src/rendering/" nested under src, "pkg/foo/
// rendering/" nested arbitrarily) without forcing the validator to
// guess; the slot name in the plan IS the disambiguator.
func slotDir(p, slotName string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	lastIdx := -1
	for i, part := range parts {
		if part == slotName {
			lastIdx = i
		}
	}
	if lastIdx < 0 {
		return ""
	}
	return strings.Join(parts[:lastIdx+1], "/")
}

// dirOverlap returns true if a and b are equal, or one is a strict
// path-component prefix of the other ("src/foo" overlaps "src/foo/bar"
// but NOT "src/foobar"). Used to detect slot directories that would
// shadow each other at the filesystem level.
func dirOverlap(a, b string) bool {
	if a == b {
		return true
	}
	if strings.HasPrefix(a+"/", b+"/") {
		return true
	}
	if strings.HasPrefix(b+"/", a+"/") {
		return true
	}
	return false
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
