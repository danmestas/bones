// Package banner emits the bones ASCII banner on stdout. Marquee
// verbs (bones up, bones init) call Print at the top of their
// output when stdout is a TTY; the banner is suppressed when output
// is redirected so script consumers see only the structured output
// they expect.
package banner

import (
	"fmt"
	"os"
)

// Text is the ASCII banner. Standard figlet "Big" font; pure ASCII
// so it renders identically across every terminal.
const Text = ` ____   ___  _   _ _____ ____
| __ ) / _ \| \ | | ____/ ___|
|  _ \| | | |  \| |  _| \___ \
| |_) | |_| | |\  | |___ ___) |
|____/ \___/|_| \_|_____|____/`

// Tagline is the one-line subtitle printed beneath Text.
const Tagline = "             multi-agent orchestration"

// Print writes the banner to stdout when stdout is a TTY. When
// stdout is redirected (file, pipe, or non-character device) the
// call is a silent no-op so structured output stays clean for
// downstream tools.
func Print() {
	if !stdoutIsTerminal() {
		return
	}
	fmt.Println()
	fmt.Println(Text)
	fmt.Println(Tagline)
	fmt.Println()
}

// stdoutIsTerminal reports whether stdout is connected to a
// character device (terminal). Implementation uses Stat()'s mode
// bits so we don't pull in golang.org/x/term as a direct dep —
// equivalent for the purpose of detecting interactive output.
func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
