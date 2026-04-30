package cli

import (
	"fmt"
	"net"
	"testing"
)

func TestPickPeekPort_ReturnsAvailablePort(t *testing.T) {
	got := pickPeekPort()
	if got < peekPortRangeStart || got > peekPortRangeEnd {
		t.Fatalf("pickPeekPort returned %d, want in [%d,%d]",
			got, peekPortRangeStart, peekPortRangeEnd)
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", got))
	if err != nil {
		t.Errorf("returned port %d isn't bindable: %v", got, err)
		return
	}
	_ = l.Close()
}

func TestPickPeekPort_SkipsBoundPorts(t *testing.T) {
	// Hold the entire preferred range open. pickPeekPort must return
	// 0 (the "let fossil pick" sentinel) rather than spuriously hand
	// back a port we know is in use.
	var holders []net.Listener
	defer func() {
		for _, l := range holders {
			_ = l.Close()
		}
	}()
	for p := peekPortRangeStart; p <= peekPortRangeEnd; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			t.Skipf("port %d already in use; skipping (real env)", p)
		}
		holders = append(holders, l)
	}

	if got := pickPeekPort(); got != 0 {
		t.Errorf("range full: got %d, want 0 (fallback)", got)
	}
}

func TestPickPeekPort_PrefersLowestFree(t *testing.T) {
	// Bind the first port. pickPeekPort should skip it and return
	// the next free one.
	first, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", peekPortRangeStart))
	if err != nil {
		t.Skipf("port %d already in use; skipping (real env)", peekPortRangeStart)
	}
	defer func() { _ = first.Close() }()

	got := pickPeekPort()
	if got == peekPortRangeStart {
		t.Errorf("returned bound port %d", got)
	}
	if got < peekPortRangeStart+1 || got > peekPortRangeEnd {
		t.Errorf("got %d, want in (%d,%d]",
			got, peekPortRangeStart, peekPortRangeEnd)
	}
}
