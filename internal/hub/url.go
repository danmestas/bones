package hub

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// FossilURL returns the hub Fossil HTTP URL recorded for the workspace
// at root, or "" if no hub is currently running there.
//
// Consumers (`bones up`, `swarm join/commit/close`, `tasks status`,
// etc.) read this rather than hardcoding 127.0.0.1:8765 so two bones
// workspaces can run concurrently with port allocations of their own.
func FossilURL(root string) string {
	p, err := newPaths(root)
	if err != nil {
		return ""
	}
	return readURLFile(p.fossilURL)
}

// NATSURL returns the hub NATS URL recorded for the workspace at root,
// or "" if no hub is running.
func NATSURL(root string) string {
	p, err := newPaths(root)
	if err != nil {
		return ""
	}
	return readURLFile(p.natsURL)
}

func readURLFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resolvePorts fills any zero-valued port in opts. Resolution order:
//  1. Recorded URL file (so a steady-state restart picks the same port
//     it had before, which keeps consumer URLs stable across hub
//     restarts).
//  2. Free port from the OS via pickFreePort.
//
// After resolution, the URL files are (re)written so consumers always
// see the live hub's URLs — even when the caller passed explicit ports.
func resolvePorts(p paths, o *opts) error {
	if o.repoPort == 0 {
		if recorded := portFromURL(readURLFile(p.fossilURL)); recorded != 0 {
			o.repoPort = recorded
		} else {
			port, err := pickFreePort()
			if err != nil {
				return fmt.Errorf("hub: pick fossil port: %w", err)
			}
			o.repoPort = port
		}
	}
	if o.coordPort == 0 {
		if recorded := portFromURL(readURLFile(p.natsURL)); recorded != 0 {
			o.coordPort = recorded
		} else {
			port, err := pickFreePort()
			if err != nil {
				return fmt.Errorf("hub: pick nats port: %w", err)
			}
			o.coordPort = port
		}
	}
	return writeURLFiles(p, *o)
}

// writeURLFiles records the hub's fully-resolved URLs so consumers can
// discover them without knowing the port allocation policy.
func writeURLFiles(p paths, o opts) error {
	fossilURL := fmt.Sprintf("http://127.0.0.1:%d", o.repoPort)
	natsURL := fmt.Sprintf("nats://127.0.0.1:%d", o.coordPort)
	if err := os.WriteFile(p.fossilURL, []byte(fossilURL+"\n"), 0o644); err != nil {
		return fmt.Errorf("hub: write fossil-url: %w", err)
	}
	if err := os.WriteFile(p.natsURL, []byte(natsURL+"\n"), 0o644); err != nil {
		return fmt.Errorf("hub: write nats-url: %w", err)
	}
	return nil
}

// portFromURL extracts the trailing :port from a URL like
// "http://127.0.0.1:8765" or "nats://127.0.0.1:4222". Returns 0 on any
// parse error.
func portFromURL(url string) int {
	idx := strings.LastIndex(url, ":")
	if idx < 0 || idx == len(url)-1 {
		return 0
	}
	var port int
	if _, err := fmt.Sscanf(url[idx+1:], "%d", &port); err != nil {
		return 0
	}
	return port
}

// pickFreePort asks the OS for a free TCP port. Mirrors the helper in
// internal/workspace; duplicated here to avoid an import cycle.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}
