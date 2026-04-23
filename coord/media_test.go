package coord

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmestas/edgesync/leaf/agent/notify"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

func TestPostMedia_HappyPath(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	sharedRepo := filepath.Join(t.TempDir(), "shared-code.fossil")
	cA := newCoordWithCodeRepo(t, url, "agent-A", sharedRepo)
	cB := newCoordWithCodeRepo(t, url, "agent-B", sharedRepo)

	events, closeSub, err := cB.Subscribe(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = closeSub() }()

	wantData := []byte("fake-png-bytes")
	if err := cA.PostMedia(
		context.Background(),
		"t1",
		"image/png",
		wantData,
	); err != nil {
		t.Fatalf("PostMedia: %v", err)
	}

	select {
	case evt, ok := <-events:
		if !ok {
			t.Fatal("events closed before media delivery")
		}
		media, isMedia := evt.(MediaMessage)
		if !isMedia {
			t.Fatalf("event type=%T, want MediaMessage", evt)
		}
		if media.From() != "agent-A" {
			t.Fatalf("From=%q, want agent-A", media.From())
		}
		if media.Thread() == "" {
			t.Fatal("Thread empty")
		}
		if media.MIMEType() != "image/png" {
			t.Fatalf("MIMEType=%q, want image/png", media.MIMEType())
		}
		if media.Size() != len(wantData) {
			t.Fatalf("Size=%d, want %d", media.Size(), len(wantData))
		}
		if media.Path() == "" {
			t.Fatal("Path empty")
		}
		if media.Rev() == "" {
			t.Fatal("Rev empty")
		}
		if media.Timestamp().IsZero() {
			t.Fatal("Timestamp zero")
		}
		gotData, err := cB.OpenFile(
			context.Background(),
			media.Rev(),
			media.Path(),
		)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		if !bytes.Equal(gotData, wantData) {
			t.Fatalf("OpenFile bytes=%q, want %q", gotData, wantData)
		}
	case <-time.After(subscribeDeliveryTimeout):
		t.Fatalf("no MediaMessage within %s", subscribeDeliveryTimeout)
	}
}

func TestMediaFromMessage_MalformedFallsThrough(t *testing.T) {
	msg := notify.Message{
		ID:        "msg-test-malformed-media-0001",
		Thread:    "thread-test-uuid-0001",
		From:      "agent-A",
		Body:      "MEDIA:not-json",
		Timestamp: time.Now().UTC(),
	}
	if _, ok := mediaFromMessage(msg); ok {
		t.Fatalf("mediaFromMessage: got ok=true for malformed body")
	}
	evt := eventFromMessage(msg)
	cm, isChat := evt.(ChatMessage)
	if !isChat {
		t.Fatalf("eventFromMessage: got %T, want ChatMessage", evt)
	}
	if cm.Body() != msg.Body {
		t.Fatalf("ChatMessage.Body=%q, want %q", cm.Body(), msg.Body)
	}
}

func TestPostMedia_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_ = c.PostMedia(context.Background(), "t1", "text/plain", []byte("x"))
	}, "coord is closed")
}

func TestPostMedia_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.PostMedia(nilCtx, "t1", "text/plain", []byte("x"))
		}, "ctx is nil")
	})
	t.Run("empty thread", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.PostMedia(context.Background(), "", "text/plain", []byte("x"))
		}, "thread is empty")
	})
	t.Run("empty mime", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.PostMedia(context.Background(), "t1", "", []byte("x"))
		}, "mimeType is empty")
	})
	t.Run("empty data", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.PostMedia(context.Background(), "t1", "text/plain", nil)
		}, "data is empty")
	})
}
