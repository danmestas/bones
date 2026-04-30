package telemetry

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstRunNotice_PrintsOnceThenSilent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var buf bytes.Buffer
	FirstRunNotice(&buf, "https://signoz.example.com/v1/traces")
	first := buf.String()
	if !strings.Contains(first, "telemetry enabled") {
		t.Fatalf("first call did not print notice: %q", first)
	}
	if !strings.Contains(first, "https://signoz.example.com/v1/traces") {
		t.Errorf("notice missing endpoint: %q", first)
	}

	// Second call: ack file exists, notice is silent.
	buf.Reset()
	FirstRunNotice(&buf, "https://signoz.example.com/v1/traces")
	if buf.Len() != 0 {
		t.Errorf("second call printed: %q", buf.String())
	}

	// Ack file exists.
	ack := filepath.Join(os.Getenv("HOME"), telemetryAckFile)
	if _, err := os.Stat(ack); err != nil {
		t.Errorf("ack file missing: %v", err)
	}
}

func TestFirstRunNotice_RearmedAfterAckRemoval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var buf bytes.Buffer
	FirstRunNotice(&buf, "https://x")
	if buf.Len() == 0 {
		t.Fatal("first call should print")
	}

	// Remove ack — notice should re-arm.
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), telemetryAckFile)); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	FirstRunNotice(&buf, "https://x")
	if buf.Len() == 0 {
		t.Error("expected notice after ack removal, got nothing")
	}
}
