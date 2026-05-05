package cli

import (
	"fmt"
	"os"

	"github.com/danmestas/libfossil"
)

// passiveCheckpointHubFossil runs `PRAGMA wal_checkpoint(PASSIVE)` against
// the bones-managed hub fossil so vanilla fossil tooling (e.g. `fossil ui`,
// `fossil timeline`, `fossil leaves`) sees a self-contained main file
// instead of a 4 KiB stub plus a multi-hundred-KiB unmerged WAL.
//
// Why this is needed:
//
// libfossil opens the hub repo in WAL mode and writes a stub-plus-WAL on
// every `bones up` / `bones hub start`. Until SQLite checkpoints the WAL
// (which only happens on graceful close, manual checkpoint, or auto-
// checkpoint after enough page churn), the main `.bones/hub.fossil` file
// has too few pages for vanilla fossil's validity probe — `fossil` then
// rejects it with `not a valid repository` and exits 1. See bones #211 /
// #212.
//
// The PASSIVE mode is deliberate: it never blocks readers or writers, so
// it is safe to call while the hub is running. It only checkpoints frames
// that no other connection is reading. That's enough to fatten the main
// file past vanilla fossil's threshold even on a live workspace.
//
// This is a bones-side workaround. The durable fix is upstream in
// libfossil/EdgeSync's hub close path (the WAL should be checkpointed
// when the hub stops cleanly). Until that lands, every shell-out from
// bones to vanilla fossil against `hub.fossil` MUST call this helper
// first.
//
// Errors are logged to stderr but never returned: a failed checkpoint
// degrades into "vanilla fossil might fail," which is the same surface
// callers already handle. We never want a checkpoint failure to mask
// the vanilla-fossil command the user actually asked for.
func passiveCheckpointHubFossil(hubRepoPath string) {
	repo, err := libfossil.Open(hubRepoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: passive WAL checkpoint skipped (open %s: %v); "+
				"vanilla fossil may reject the repo if WAL is unmerged\n",
			hubRepoPath, err)
		return
	}
	defer func() { _ = repo.Close() }()

	// PRAGMA wal_checkpoint returns three columns: busy, log frames,
	// frames checkpointed. We don't care about the values — we only
	// care that the call ran without error.
	var busy, logFrames, ckptFrames int
	row := repo.DB().QueryRow("PRAGMA wal_checkpoint(PASSIVE)")
	if err := row.Scan(&busy, &logFrames, &ckptFrames); err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: passive WAL checkpoint of %s failed: %v "+
				"(vanilla fossil may reject the repo)\n",
			hubRepoPath, err)
	}
}
