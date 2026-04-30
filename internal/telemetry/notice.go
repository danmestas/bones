package telemetry

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// telemetryAckFile records that the operator has been shown the
// first-run notice. Idempotency marker only: deletion of the file
// re-arms the notice.
const telemetryAckFile = ".bones/telemetry-acknowledged"

// FirstRunNotice prints a one-line notice to w if the operator has not
// yet acknowledged that telemetry is on. The acknowledgment is recorded
// at ~/.bones/telemetry-acknowledged so subsequent invocations stay
// quiet. Removing that file (or pointing $HOME elsewhere) re-arms the
// notice.
//
// Always loud, never silent: the notice is the operator's signal that
// data will leave their machine. If the ack file can't be written, the
// notice still prints — better to nag than to silently export.
func FirstRunNotice(w io.Writer, endpoint string) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeNotice(w, endpoint)
		return
	}
	path := filepath.Join(home, telemetryAckFile)
	if _, err := os.Stat(path); err == nil {
		return
	}
	writeNotice(w, endpoint)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte("acknowledged\n"), 0o644)
}

func writeNotice(w io.Writer, endpoint string) {
	_, _ = fmt.Fprintf(w,
		"bones: anonymous usage telemetry enabled — exporting to %s. "+
			"Opt out: bones telemetry disable\n",
		endpoint)
}
