package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
	ifossil "github.com/danmestas/agent-infra/internal/fossil"
)

// PostMedia stores opaque bytes in the shared Fossil repo and publishes an
// in-band media reference on the chat thread. Subscribers receive a
// MediaMessage event and may call OpenFile with the returned rev/path.
func (c *Coord) PostMedia(
	ctx context.Context,
	thread, mimeType string,
	data []byte,
) error {
	c.assertOpen("PostMedia")
	assert.NotNil(ctx, "coord.PostMedia: ctx is nil")
	assert.NotEmpty(thread, "coord.PostMedia: thread is empty")
	assert.NotEmpty(mimeType, "coord.PostMedia: mimeType is empty")
	assert.Precondition(len(data) > 0, "coord.PostMedia: data is empty")
	now := time.Now().UTC()
	path := mediaArtifactPath(c.cfg.AgentID, now)
	rev, err := c.sub.fossil.Commit(
		ctx,
		fmt.Sprintf("post media to %s (%s)", thread, mimeType),
		[]ifossil.File{{Path: path, Content: data}},
		"",
	)
	if err != nil {
		return fmt.Errorf("coord.PostMedia: commit media: %w", err)
	}
	body, err := mediaBody(rev, path, mimeType, len(data))
	if err != nil {
		return fmt.Errorf("coord.PostMedia: encode body: %w", err)
	}
	if err := c.sub.chat.Send(ctx, thread, body); err != nil {
		return fmt.Errorf("coord.PostMedia: %w", err)
	}
	return nil
}

func mediaArtifactPath(agentID string, now time.Time) string {
	return fmt.Sprintf("media/%s/%d.bin", agentID, now.UnixNano())
}

func mediaBody(rev, path, mimeType string, size int) (string, error) {
	payload, err := json.Marshal(mediaEnvelope{
		Rev:      rev,
		Path:     path,
		MIMEType: mimeType,
		Size:     size,
	})
	if err != nil {
		return "", err
	}
	return mediaBodyPrefix + string(payload), nil
}
