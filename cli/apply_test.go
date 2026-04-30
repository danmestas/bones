package cli

import (
	"strings"
	"testing"

	libfossilcli "github.com/danmestas/libfossil/cli"
)

func TestApplyCmd_StubReturnsNotImplemented(t *testing.T) {
	cmd := &ApplyCmd{}
	err := cmd.Run(&libfossilcli.Globals{})
	if err == nil {
		t.Fatal("expected an error from stub Run, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("expected 'not yet implemented' in error, got: %v", err)
	}
}
