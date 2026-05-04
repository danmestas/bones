package coord

// Test-only: register the modernc SQLite driver so libfossil's
// transitive uses (via EdgeSync's hub.NewHub and agent.New, both of
// which open SQLite-backed fossil repos internally) can find a driver
// during `go test ./internal/coord/...`. Production binaries register
// the driver at cmd/bones/main.go.
import _ "github.com/danmestas/libfossil/db/driver/modernc"
