package coord

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/danmestas/EdgeSync/leaf/agent/notify"
)

// TestLeaf_PostMedia_HappyPath posts an opaque media blob through
// Leaf.PostMedia and asserts (a) the chat substrate delivers a
// MediaMessage envelope to a Subscribe consumer, (b) the rev/path on
// the envelope round-trip the original bytes from the leaf's libfossil
// repo. The same Leaf is both producer and consumer; cross-process
// propagation is covered by leaf_commit_test.go's hub-tip assertion.
func TestLeaf_PostMedia_HappyPath(t *testing.T) {
	l, _ := openLeafFixture(t, "slot-M")
	ctx := context.Background()

	events, closeSub, err := l.coord.Subscribe(ctx, "t1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = closeSub() }()

	wantData := []byte("fake-png-bytes")
	if err := l.PostMedia(ctx, "t1", "image/png", wantData); err != nil {
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
		gotData := readLeafArtifact(t, l, string(media.Rev()), media.Path())
		if !bytes.Equal(gotData, wantData) {
			t.Fatalf("ReadFile bytes=%q, want %q", gotData, wantData)
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

func TestLeaf_PostMedia_InvariantPanics(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var l *Leaf
		requirePanic(t, func() {
			_ = l.PostMedia(context.Background(), "t1", "text/plain", []byte("x"))
		}, "receiver is nil")
	})
	t.Run("nil ctx", func(t *testing.T) {
		l, _ := openLeafFixture(t, "slot-pm-ctx")
		requirePanic(t, func() {
			_ = l.PostMedia(nilCtx, "t1", "text/plain", []byte("x"))
		}, "ctx is nil")
	})
	t.Run("empty thread", func(t *testing.T) {
		l, _ := openLeafFixture(t, "slot-pm-thread")
		requirePanic(t, func() {
			_ = l.PostMedia(context.Background(), "", "text/plain", []byte("x"))
		}, "thread is empty")
	})
	t.Run("empty mime", func(t *testing.T) {
		l, _ := openLeafFixture(t, "slot-pm-mime")
		requirePanic(t, func() {
			_ = l.PostMedia(context.Background(), "t1", "", []byte("x"))
		}, "mimeType is empty")
	})
	t.Run("empty data", func(t *testing.T) {
		l, _ := openLeafFixture(t, "slot-pm-data")
		requirePanic(t, func() {
			_ = l.PostMedia(context.Background(), "t1", "text/plain", nil)
		}, "data is empty")
	})
}
