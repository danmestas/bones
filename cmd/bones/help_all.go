// help-all wires `bones --help --all` (and `bones <verb> --help --all`) so
// that operators can discover every subcommand's full flag set in one pass.
//
// Default `bones --help` and `bones <verb> --help` are unchanged — Kong's
// stock printer is preserved byte-for-byte. The `--all` flag is detected
// pre-parse, stripped from os.Args, and only when present does this file
// install a custom help printer that walks the parsed Kong app tree and
// concatenates DefaultHelpPrinter output for every non-hidden node.
//
// Implementation notes (#325):
//   - `--all` is intercepted pre-parse rather than declared as a Kong flag.
//     Adding it as a real flag to every subcommand would either be repetitive
//     or rely on Kong's "embed/persist" patterns that don't cleanly bubble up
//     into help-flag handling. Pre-parse interception keeps the surface
//     minimal: zero changes to command structs, one wrapper around the
//     default help printer.
//   - Re-using kong.DefaultHelpPrinter per node means the per-command output
//     shape matches `bones <cmd> --help` exactly. We synthesize a *kong.Context
//     with just enough fields populated for DefaultHelpPrinter to run
//     (Kong + Path); other context fields are unused on the help path.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kong"
)

// detectHelpAll returns (filteredArgs, helpAll) where helpAll is true if the
// arglist contains "--all" anywhere alongside a help request ("--help" or
// "-h", or in `kong.UsageOnError()` mode the bare-args case where Kong shows
// help). When helpAll is true, "--all" is removed from filteredArgs so Kong
// itself never sees it; the `kong.Help(...)` printer is replaced with the
// recursive variant by the caller.
//
// We intentionally only honor "--all" (no "-a" short form) — single-letter
// flags collide with per-subcommand options and `--all` is unambiguous in
// every existing subtree (`bones tasks list --all` is the only existing
// "--all" and it lives behind a positional command, not after `--help`).
func detectHelpAll(args []string) (filtered []string, helpAll bool) {
	hasHelp := false
	hasAll := false
	for _, a := range args {
		switch a {
		case "--help", "-h":
			hasHelp = true
		case "--all":
			hasAll = true
		}
	}
	if !hasHelp || !hasAll {
		return args, false
	}
	filtered = make([]string, 0, len(args))
	for _, a := range args {
		if a == "--all" {
			continue
		}
		filtered = append(filtered, a)
	}
	return filtered, true
}

// recursiveHelpPrinter returns a kong.HelpPrinter that prints the selected
// node's help, then walks its non-hidden descendants and prints each one's
// help in turn, separated by a divider line.
func recursiveHelpPrinter() kong.HelpPrinter {
	return func(options kong.HelpOptions, ctx *kong.Context) error {
		// Start node: the command the user selected, or the application
		// root for bare `bones --help --all`. The isAppRoot distinction
		// matters: DefaultHelpPrinter branches on ctx.Selected() (nil →
		// printApp, non-nil → printCommand), and the synthetic Path we
		// build per node decides which branch fires. For a selected leaf
		// we MUST include its full Command chain so printCommand renders
		// the leaf's own usage — passing isRoot=true here is the bug
		// from #325 review: it stripped the Command path and routed every
		// `bones <leaf> --help --all` invocation through printApp,
		// surfacing the top-level app help instead of the leaf's.
		root := ctx.Selected()
		isAppRoot := root == nil
		if isAppRoot {
			root = ctx.Model.Node
		}

		// Print the root node first (full help block).
		if err := printNode(options, ctx, root, isAppRoot); err != nil {
			return err
		}

		// Walk descendants in a stable, depth-first order. Skip hidden
		// nodes — they're hidden from regular `--help` for a reason
		// (e.g. session-marker, dispatch parent/worker) and should
		// stay hidden from `--help --all` too.
		var walk func(n *kong.Node) error
		walk = func(n *kong.Node) error {
			for _, child := range n.Children {
				if child.Hidden {
					continue
				}
				if err := writeDivider(ctx, child); err != nil {
					return err
				}
				if err := printNode(options, ctx, child, false); err != nil {
					return err
				}
				if err := walk(child); err != nil {
					return err
				}
			}
			return nil
		}
		return walk(root)
	}
}

// printNode invokes kong.DefaultHelpPrinter against a synthetic Context
// whose Path identifies `node` as the selected node. For the application
// root (isRoot=true) we pass a single ApplicationNode path entry so
// ctx.Selected() returns nil and the printer renders the top-level usage.
func printNode(options kong.HelpOptions, ctx *kong.Context, node *kong.Node, isRoot bool) error {
	synthetic := &kong.Context{
		Kong: ctx.Kong,
		Path: buildPath(ctx, node, isRoot),
	}
	// Kong's DefaultHelpPrinter checks ctx.Empty() — for our synthetic
	// contexts we always want the full (non-summary) form.
	options.Summary = false
	return kong.DefaultHelpPrinter(options, synthetic)
}

// buildPath constructs the minimal Path slice DefaultHelpPrinter needs:
//   - For the root: a single Path{App: ...} entry, which makes
//     ctx.Selected() return nil → printApp branch.
//   - For a command node: an App entry plus a Command entry per ancestor
//     down to (and including) the target node. ctx.Selected() returns the
//     last Command in the slice → printCommand branch.
func buildPath(ctx *kong.Context, node *kong.Node, isRoot bool) []*kong.Path {
	app := ctx.Model
	path := []*kong.Path{{App: app}}
	if isRoot {
		return path
	}
	// Collect ancestors from root down to node (excluding the application).
	chain := []*kong.Node{}
	for cur := node; cur != nil && cur.Type != kong.ApplicationNode; cur = cur.Parent {
		chain = append([]*kong.Node{cur}, chain...)
	}
	for _, n := range chain {
		path = append(path, &kong.Path{Command: n})
	}
	return path
}

// writeDivider prints a blank line + a header line + a blank line to make
// each command block visually distinct in the recursive help output.
// Kong's printer emits no trailing blank line of its own, so we own
// the spacing between blocks.
func writeDivider(ctx *kong.Context, node *kong.Node) error {
	header := fmt.Sprintf("=== %s %s ===", ctx.Model.Name, node.Path())
	// Trim the path's leading whitespace defensively (Node.Path can
	// return "" for the root, which we never pass here, but be safe).
	header = strings.TrimSpace(header)
	_, err := fmt.Fprintf(ctx.Stdout, "\n%s\n\n", header)
	return err
}

// applyHelpAllArgs mutates os.Args (caller's responsibility — done before
// kong.Parse runs) and returns the kong.Help option to install. Returns
// nil option when --all wasn't requested, signaling the caller to leave
// Kong's default help wiring alone.
func applyHelpAllArgs() kong.Option {
	filtered, helpAll := detectHelpAll(os.Args)
	if !helpAll {
		return nil
	}
	os.Args = filtered
	return kong.Help(recursiveHelpPrinter())
}
