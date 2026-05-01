package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintFix(t *testing.T) {
	var buf bytes.Buffer
	printFix(&buf, "bones swarm close --slot=foo --result=fail")
	got := buf.String()
	if !strings.Contains(got, "Fix:") {
		t.Fatalf("expected Fix: prefix, got %q", got)
	}
	if !strings.Contains(got, "bones swarm close --slot=foo --result=fail") {
		t.Fatalf("expected command in output, got %q", got)
	}
}

func TestFixForStaleSlot(t *testing.T) {
	got := FixForStaleSlot("auth")
	if !strings.Contains(got, "bones") {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(got, "auth") {
		t.Fatalf("slot name missing from fix, got %q", got)
	}
}

func TestFixForRemoteSlot(t *testing.T) {
	got := FixForRemoteSlot("box42")
	if !strings.Contains(got, "box42") {
		t.Fatalf("host missing from fix, got %q", got)
	}
}

func TestFixForMissingHook(t *testing.T) {
	got := FixForMissingHook()
	if !strings.Contains(got, "bones") {
		t.Fatalf("got %q", got)
	}
}

func TestFixForScaffoldDrift(t *testing.T) {
	got := FixForScaffoldDrift()
	if !strings.Contains(got, "bones") {
		t.Fatalf("got %q", got)
	}
}

func TestFixForFossilDrift(t *testing.T) {
	got := FixForFossilDrift()
	if !strings.Contains(got, "bones") {
		t.Fatalf("got %q", got)
	}
}

func TestFixForHubDown(t *testing.T) {
	got := FixForHubDown()
	if !strings.Contains(got, "bones") {
		t.Fatalf("got %q", got)
	}
}
