package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPortFromURL(t *testing.T) {
	cases := []struct {
		url  string
		want int
	}{
		{"http://127.0.0.1:8765", 8765},
		{"nats://127.0.0.1:4222", 4222},
		{"http://127.0.0.1:0", 0},
		{"", 0},
		{"http://127.0.0.1", 0},
		{"http://127.0.0.1:notaport", 0},
	}
	for _, c := range cases {
		if got := portFromURL(c.url); got != c.want {
			t.Errorf("portFromURL(%q) = %d, want %d", c.url, got, c.want)
		}
	}
}

func TestPickFreePort(t *testing.T) {
	port, err := pickFreePort()
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 || port > 65535 {
		t.Errorf("port %d out of range", port)
	}
}

func TestResolvePorts_AllocatesFreeWhenZero(t *testing.T) {
	dir := t.TempDir()
	p, err := newPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := opts{fossilPort: 0, natsPort: 0}
	if err := resolvePorts(p, &o); err != nil {
		t.Fatal(err)
	}
	if o.fossilPort == 0 || o.natsPort == 0 {
		t.Errorf("ports not assigned: %+v", o)
	}
	if o.fossilPort == o.natsPort {
		t.Errorf("collision: both ports = %d", o.fossilPort)
	}
	if FossilURL(dir) == "" {
		t.Errorf("fossil URL file not written")
	}
	if NATSURL(dir) == "" {
		t.Errorf("nats URL file not written")
	}
}

func TestResolvePorts_ReadsRecordedURL(t *testing.T) {
	dir := t.TempDir()
	p, err := newPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-record URLs as if a previous hub had run.
	if err := os.WriteFile(p.fossilURL,
		[]byte("http://127.0.0.1:9001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.natsURL,
		[]byte("nats://127.0.0.1:9002\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := opts{fossilPort: 0, natsPort: 0}
	if err := resolvePorts(p, &o); err != nil {
		t.Fatal(err)
	}
	if o.fossilPort != 9001 {
		t.Errorf("fossilPort = %d, want 9001 (recorded)", o.fossilPort)
	}
	if o.natsPort != 9002 {
		t.Errorf("natsPort = %d, want 9002 (recorded)", o.natsPort)
	}
}

func TestResolvePorts_PreservesExplicitPort(t *testing.T) {
	dir := t.TempDir()
	p, err := newPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := opts{fossilPort: 7777, natsPort: 7778}
	if err := resolvePorts(p, &o); err != nil {
		t.Fatal(err)
	}
	if o.fossilPort != 7777 || o.natsPort != 7778 {
		t.Errorf("explicit ports clobbered: %+v", o)
	}
	// URL files should still reflect the explicit port.
	if got := FossilURL(dir); got != "http://127.0.0.1:7777" {
		t.Errorf("FossilURL = %q, want http://127.0.0.1:7777", got)
	}
}

func TestFossilURL_ReturnsEmptyWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	if got := FossilURL(dir); got != "" {
		t.Errorf("FossilURL = %q, want empty", got)
	}
	if got := NATSURL(dir); got != "" {
		t.Errorf("NATSURL = %q, want empty", got)
	}
}

func TestFossilURL_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p, err := newPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:8765"
	if err := os.WriteFile(p.fossilURL, []byte(want+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FossilURL(dir); got != want {
		t.Errorf("FossilURL = %q, want %q", got, want)
	}
}

// TestStartStopWritesAndRemovesURLFiles is a thin assert layered on the
// existing round-trip: after Start binds, the URL files exist; after
// Stop, they're gone.
func TestStartStopWritesAndRemovesURLFiles(t *testing.T) {
	root := t.TempDir()
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := opts{fossilPort: 0, natsPort: 0}
	if err := resolvePorts(p, &o); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{p.fossilURL, p.natsURL} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("URL file %s missing after resolvePorts: %v",
				filepath.Base(path), err)
		}
	}
	want := fmt.Sprintf("http://127.0.0.1:%d", o.fossilPort)
	if got := FossilURL(root); got != want {
		t.Errorf("FossilURL = %q, want %q", got, want)
	}
	// Stop removes URL files.
	if err := Stop(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{p.fossilURL, p.natsURL} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("URL file %s still present after Stop", filepath.Base(path))
		}
	}
}
