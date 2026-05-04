package hub

// Test-only: register the modernc SQLite driver so libfossil's
// transitive uses (via EdgeSync's hub.NewHub, which opens SQLite-backed
// fossil repos internally) can find a driver during
// `go test ./internal/hub/...`. Production binaries register the
// driver at cmd/bones/main.go.
import _ "github.com/danmestas/libfossil/db/driver/modernc"
