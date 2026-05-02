package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/githook"
	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/sessions"
	"github.com/danmestas/bones/internal/workspace"
)

// DownCmd reverses bones up: stops the hub via hub.Stop, removes the
// workspace marker (.bones/), any leftover legacy state
// (.orchestrator/ from pre-ADR-0041 workspaces), the scaffolded skills
// (.claude/skills/{orchestrator,subagent,uninstall-bones}), and the
// bones-installed SessionStart/Stop hooks from .claude/settings.json.
// Other hooks in settings.json are left untouched.
//
// Destructive — requires --yes or an interactive y/N confirmation.
// Idempotent: re-running on a clean tree is a no-op.
type DownCmd struct {
	Yes        bool `name:"yes" short:"y" help:"skip the confirmation prompt"`
	KeepSkills bool `name:"keep-skills" help:"do not remove .claude/skills"`
	KeepHooks  bool `name:"keep-hooks" help:"do not edit .claude/settings.json"`
	KeepHub    bool `name:"keep-hub" help:"do not stop hub or remove .orchestrator/"`
	DryRun     bool `name:"dry-run" help:"print plan without executing"`
	All        bool `name:"all" help:"tear down all registered workspaces"`
}

// Run is the Kong entry point. Resolves the workspace root (walking
// up from cwd if a marker exists, falling back to cwd otherwise),
// builds an execution plan, prompts unless --yes, and executes.
func (c *DownCmd) Run(g *libfossilcli.Globals) error {
	if c.All {
		return c.runAll()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	root := resolveDownRoot(cwd)
	return runDown(root, c, os.Stdin)
}

// runAll iterates the workspace registry and tears each down. Prints
// a summary, prompts unless --yes, then invokes runDown per workspace.
func (c *DownCmd) runAll() error {
	entries, err := registry.List()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("No workspaces running.")
		return nil
	}
	fmt.Println("Will stop:")
	for _, e := range entries {
		fmt.Printf("  %-20s %s   sessions=%d\n",
			e.Name, e.Cwd, sessions.CountByWorkspace(e.Cwd))
	}
	fmt.Printf("\n%d workspaces will be terminated.\n", len(entries))

	if !c.Yes {
		if !confirm(os.Stdin, "Continue?") {
			return errors.New("down --all: aborted")
		}
	}

	var firstErr error
	for _, e := range entries {
		single := *c
		single.All = false
		single.Yes = true
		if err := runDown(e.Cwd, &single, os.Stdin); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", e.Name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// resolveDownRoot returns the workspace root to operate on. Walks up
// from cwd looking for the .bones/agent.id marker; falls back to cwd
// when no workspace exists (still useful for partial installs).
//
// Uses workspace.FindRoot (read-only) rather than workspace.Join: Join
// would lazy-start the hub, which is the opposite of what `bones down`
// wants. Without this, running `bones down` against a workspace where
// the hub is already stopped would spin a fresh hub up just to ask
// permission to tear it down — and on non-TTY (no `--yes`) the prompt
// aborts immediately, leaving the user with a hub that wasn't running
// before they ran `down` (#138 item 7).
func resolveDownRoot(cwd string) string {
	if root, err := workspace.FindRoot(cwd); err == nil {
		return root
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
	plan = append(plan, planRemoveRegistry(root)...)
	plan = append(plan, planRemoveGitHook(root)...)
	plan = append(plan, planRemoveBonesDir(root)...)
	plan = append(plan, planRemoveOrchestrator(root, c)...)
	plan = append(plan, planRemoveSkills(root, c)...)
	plan = append(plan, planRemoveAgentsMD(root)...)
	plan = append(plan, planRemoveHooks(root, c)...)
	plan = append(plan, planRemoveFossilMarkers(root)...)
	return plan
}

// planRemoveGitHook restores the user's original pre-commit (if any)
// and removes the bones-managed hook. The githook package treats a
// missing or non-bones hook as a no-op, so this is safe to call on
// partial installs.
func planRemoveGitHook(root string) []downAction {
	gitDir := githook.FindGitDir(root)
	if gitDir == "" {
		return nil
	}
	installed, err := githook.IsInstalled(gitDir)
	if err != nil || !installed {
		return nil
	}
	return []downAction{{
		description: "remove bones pre-commit hook from " + gitDir,
		do:          func() error { return githook.Uninstall(gitDir) },
	}}
}

func planStopHub(root string, c *DownCmd) []downAction {
	if c.KeepHub {
		return nil
	}
	// Always queue the stop — hub.Stop is best-effort and idempotent
	// (no-op when hub isn't running), so unconditional inclusion is
	// both correct and self-documenting in the dry-run output.
	return []downAction{{
		description: "stop hub (.bones/pids/{fossil,nats}.pid)",
		do: func() error {
			// Best-effort: an already-stopped hub must not fail down.
			_ = hub.Stop(root)
			return nil
		},
	}}
}

// planRemoveRegistry removes the workspace's cross-workspace registry entry.
// Idempotent (no-op if no entry exists). Best-effort — failure doesn't block
// other teardown actions.
func planRemoveRegistry(root string) []downAction {
	return []downAction{{
		description: "remove registry entry for " + root,
		do:          func() error { return registry.Remove(root) },
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

// planRemoveOrchestrator removes the legacy .orchestrator/ directory
// from pre-ADR-0041 workspaces. Post-ADR-0041 a fresh workspace won't
// have one, but partially-migrated or unmigrated workspaces still do —
// in which case this is the cleanup pass. No-op when absent.
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

// planRemoveAgentsMD removes the bones-managed AGENTS.md and the
// CLAUDE.md symlink (or its file fallback). Per ADR 0042 these are
// the harness-agnostic guidance channel; they pair with the hook
// entries removed by planRemoveHooks. AGENTS.md is removed only if
// it was bones-managed (first line matches the bones marker) — a
// user-authored AGENTS.md is left in place so we don't clobber
// project-specific guidance.
func planRemoveAgentsMD(root string) []downAction {
	var plan []downAction
	agentsPath := filepath.Join(root, "AGENTS.md")
	if data, err := os.ReadFile(agentsPath); err == nil && bonesOwnedAgentsMD(data) {
		plan = append(plan, downAction{
			description: "remove " + agentsPath,
			do:          func() error { return os.Remove(agentsPath) },
		})
	}
	// CLAUDE.md is unconditionally bones-managed when AGENTS.md is —
	// it's a symlink we wrote pointing at AGENTS.md (or a file fallback
	// with the same content on platforms that don't support symlinks).
	// Remove it only if it points at AGENTS.md or contains the bones
	// marker; an unrelated CLAUDE.md (user-authored, e.g. an older
	// project convention) is preserved.
	claudePath := filepath.Join(root, "CLAUDE.md")
	if target, err := os.Readlink(claudePath); err == nil && target == "AGENTS.md" {
		plan = append(plan, downAction{
			description: "remove " + claudePath,
			do:          func() error { return os.Remove(claudePath) },
		})
	} else if data, err := os.ReadFile(claudePath); err == nil && bonesOwnedAgentsMD(data) {
		plan = append(plan, downAction{
			description: "remove " + claudePath,
			do:          func() error { return os.Remove(claudePath) },
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

// removeBonesHooks edits settings.json to drop hook entries that
// bones installs:
//   - the current install: SessionStart "bones hub start" (ADR 0041);
//   - legacy installs: SessionStart "hub-bootstrap.sh", SessionEnd
//     "hub-shutdown.sh", and the older Stop "hub-shutdown.sh" shim.
//
// Other hooks are preserved verbatim. Empty hook groups and the
// "hooks" top-level key are pruned when removal leaves them empty.
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
	pruneHookEvent(hooks, "SessionStart", "hub-bootstrap.sh")         // legacy
	pruneHookEvent(hooks, "SessionStart", "bones hub start")          // current (ADR 0041)
	pruneHookEvent(hooks, "SessionStart", "bones tasks prime --json") // task priming
	pruneHookEvent(hooks, "PreCompact", "bones tasks prime --json")   // task priming
	// Current installs land under SessionEnd; the legacy Stop event
	// is also pruned so workspaces installed before the migration
	// are still cleaned up by `bones down`.
	pruneHookEvent(hooks, "SessionEnd", "hub-shutdown.sh")
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
