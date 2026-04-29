package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/workspace"
)

// DownCmd reverses bones up: stops the hub, removes the workspace
// marker (.bones/), the orchestrator state (.orchestrator/), the
// scaffolded skills (.claude/skills/{orchestrator,subagent,
// uninstall-bones}), and the bones-installed SessionStart/Stop hooks
// from .claude/settings.json. Other hooks in settings.json are left
// untouched.
//
// Destructive — requires --yes or an interactive y/N confirmation.
// Idempotent: re-running on a clean tree is a no-op.
type DownCmd struct {
	Yes        bool `name:"yes" short:"y" help:"skip the confirmation prompt"`
	KeepSkills bool `name:"keep-skills" help:"do not remove .claude/skills"`
	KeepHooks  bool `name:"keep-hooks" help:"do not edit .claude/settings.json"`
	KeepHub    bool `name:"keep-hub" help:"do not stop hub or remove .orchestrator/"`
	DryRun     bool `name:"dry-run" help:"print plan without executing"`
}

// Run is the Kong entry point. Resolves the workspace root (walking
// up from cwd if a marker exists, falling back to cwd otherwise),
// builds an execution plan, prompts unless --yes, and executes.
func (c *DownCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	root := resolveDownRoot(cwd)
	return runDown(root, c, os.Stdin)
}

// resolveDownRoot returns the workspace root to operate on. Tries
// workspace.Join first to walk up to a marker, falls back to cwd
// when no workspace exists (still useful for partial installs).
func resolveDownRoot(cwd string) string {
	info, err := workspace.Join(context.Background(), cwd)
	if err == nil {
		return info.WorkspaceDir
	}
	return cwd
}

// runDown is the testable entry point. confirmIn is the io.Reader
// used for the y/N prompt; tests pass a strings.Reader.
func runDown(root string, c *DownCmd, confirmIn interface {
	Read(p []byte) (n int, err error)
}) error {
	plan := planDown(root, c)
	if len(plan) == 0 {
		fmt.Println("down: nothing to remove (no bones state found)")
		return nil
	}

	fmt.Printf("down: workspace at %s\n", root)
	fmt.Println("down: will:")
	for _, a := range plan {
		fmt.Println("  -", a.description)
	}

	if c.DryRun {
		fmt.Println("down: --dry-run, not executing")
		return nil
	}

	if !c.Yes {
		if !confirm(confirmIn, "Proceed?") {
			return errors.New("down: aborted")
		}
	}

	var first error
	for _, a := range plan {
		if err := a.do(); err != nil && first == nil {
			first = fmt.Errorf("%s: %w", a.description, err)
		}
	}
	if first != nil {
		return first
	}
	fmt.Println("down: complete")
	return nil
}

// downAction is one step in the destructive plan. Each step has a
// human-readable description and a thunk that executes it. Steps
// are independent — failure of one doesn't abort the rest, but the
// first error is propagated so callers see something went wrong.
type downAction struct {
	description string
	do          func() error
}

// planDown enumerates the actions runDown will perform, given the
// flags. Skips actions whose target doesn't exist so the plan output
// reflects what's actually present.
func planDown(root string, c *DownCmd) []downAction {
	var plan []downAction
	plan = append(plan, planStopHub(root, c)...)
	plan = append(plan, planRemoveBonesDir(root)...)
	plan = append(plan, planRemoveOrchestrator(root, c)...)
	plan = append(plan, planRemoveSkills(root, c)...)
	plan = append(plan, planRemoveHooks(root, c)...)
	plan = append(plan, planRemoveFossilMarkers(root)...)
	return plan
}

func planStopHub(root string, c *DownCmd) []downAction {
	if c.KeepHub {
		return nil
	}
	script := filepath.Join(root, ".orchestrator", "scripts", "hub-shutdown.sh")
	if !fileExists(script) {
		return nil
	}
	return []downAction{{
		description: "stop hub via " + script,
		do: func() error {
			cmd := exec.Command("bash", script)
			cmd.Dir = root
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			// Best-effort: an already-stopped hub must not fail down.
			_ = cmd.Run()
			return nil
		},
	}}
}

func planRemoveBonesDir(root string) []downAction {
	dir := filepath.Join(root, ".bones")
	if !dirExists(dir) {
		return nil
	}
	return []downAction{{
		description: "remove " + dir,
		do:          func() error { return os.RemoveAll(dir) },
	}}
}

func planRemoveOrchestrator(root string, c *DownCmd) []downAction {
	if c.KeepHub {
		return nil
	}
	dir := filepath.Join(root, ".orchestrator")
	if !dirExists(dir) {
		return nil
	}
	return []downAction{{
		description: "remove " + dir,
		do:          func() error { return os.RemoveAll(dir) },
	}}
}

func planRemoveSkills(root string, c *DownCmd) []downAction {
	if c.KeepSkills {
		return nil
	}
	var plan []downAction
	for _, name := range []string{"orchestrator", "subagent", "uninstall-bones"} {
		dir := filepath.Join(root, ".claude", "skills", name)
		if !dirExists(dir) {
			continue
		}
		plan = append(plan, downAction{
			description: "remove " + dir,
			do:          func() error { return os.RemoveAll(dir) },
		})
	}
	return plan
}

func planRemoveHooks(root string, c *DownCmd) []downAction {
	if c.KeepHooks {
		return nil
	}
	settings := filepath.Join(root, ".claude", "settings.json")
	if !fileExists(settings) {
		return nil
	}
	return []downAction{{
		description: "remove bones hooks from " + settings,
		do:          func() error { return removeBonesHooks(settings) },
	}}
}

// planRemoveFossilMarkers cleans the Fossil checkout-at-root markers
// (ADR 0023). bones up creates these alongside the orchestrator
// scaffold; bones down removes them for symmetry. Fossil metadata
// only — never working-tree files.
func planRemoveFossilMarkers(root string) []downAction {
	var plan []downAction
	if path := filepath.Join(root, ".fslckout"); fileExists(path) || dirExists(path) {
		plan = append(plan, downAction{
			description: "remove " + path,
			do:          func() error { return os.RemoveAll(path) },
		})
	}
	if dir := filepath.Join(root, ".fossil-settings"); dirExists(dir) {
		plan = append(plan, downAction{
			description: "remove " + dir,
			do:          func() error { return os.RemoveAll(dir) },
		})
	}
	return plan
}

// removeBonesHooks edits settings.json to drop hook entries whose
// command references hub-bootstrap.sh or hub-shutdown.sh (the two
// scripts bones up installs). Other hooks are preserved verbatim.
// Empty hook groups and the "hooks" top-level key are pruned when
// removal leaves them empty.
func removeBonesHooks(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	var rootObj map[string]any
	if err := json.Unmarshal(data, &rootObj); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	hooks, _ := rootObj["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	pruneHookEvent(hooks, "SessionStart", "hub-bootstrap.sh")
	pruneHookEvent(hooks, "Stop", "hub-shutdown.sh")
	if len(hooks) == 0 {
		delete(rootObj, "hooks")
	} else {
		rootObj["hooks"] = hooks
	}
	out, err := json.MarshalIndent(rootObj, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// pruneHookEvent removes hook entries under hooks[event] whose
// command contains needle. Groups left with no entries are dropped;
// if the event has no groups left, the event key is removed too.
func pruneHookEvent(hooks map[string]any, event, needle string) {
	groups, _ := hooks[event].([]any)
	if groups == nil {
		return
	}
	var keep []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			keep = append(keep, g)
			continue
		}
		entries, _ := gm["hooks"].([]any)
		var keepEntries []any
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				keepEntries = append(keepEntries, e)
				continue
			}
			cmd, _ := em["command"].(string)
			if !strings.Contains(cmd, needle) {
				keepEntries = append(keepEntries, e)
			}
		}
		if len(keepEntries) == 0 {
			continue
		}
		gm["hooks"] = keepEntries
		keep = append(keep, gm)
	}
	if len(keep) == 0 {
		delete(hooks, event)
		return
	}
	hooks[event] = keep
}

// confirm prompts the user for y/N via the supplied reader. Returns
// true only on an explicit "y" or "yes" (case-insensitive). Anything
// else, including EOF or read errors, returns false.
func confirm(in interface {
	Read(p []byte) (n int, err error)
}, prompt string) bool {
	fmt.Printf("\n%s [y/N]: ", prompt)
	rdr := bufio.NewReader(in)
	line, err := rdr.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes"
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
