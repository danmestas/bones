package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/libfossil"

	"github.com/danmestas/agent-infra/internal/assert"
)

// PostMedia stores opaque bytes in the leaf's libfossil repo and
// publishes an in-band media reference on the chat thread. Subscribers
// receive a MediaMessage event.
//
// All writes route through l.agent.Repo() — the only *libfossil.Repo
// handle to leaf.fossil in this process. After the commit, SyncNow
// triggers a sync round so the artifact propagates to the hub before
// the chat reference is published; subscribers can then resolve the
// reference against any peer that has caught up.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), receiver non-nil, thread/mimeType non-empty, data
// non-empty.
//
// Operator errors returned: any error from the libfossil commit, the
// JSON envelope encode, or the chat substrate Send — surfaced wrapped
// with the coord.Leaf.PostMedia prefix.
func (l *Leaf) PostMedia(
	ctx context.Context,
	thread, mimeType string,
	data []byte,
) error {
	assert.NotNil(l, "coord.Leaf.PostMedia: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.PostMedia: ctx is nil")
	assert.NotEmpty(thread, "coord.Leaf.PostMedia: thread is empty")
	assert.NotEmpty(mimeType, "coord.Leaf.PostMedia: mimeType is empty")
	assert.Precondition(len(data) > 0, "coord.Leaf.PostMedia: data is empty")
	now := time.Now().UTC()
	path := mediaArtifactPath(l.slotID, now)
	repo := l.agent.Repo()
	_, uuid, err := repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: normalizeLeadingSlash(path), Content: data},
		},
		Comment: fmt.Sprintf("post media to %s (%s)", thread, mimeType),
		User:    l.slotID,
	})
	if err != nil {
		return fmt.Errorf("coord.Leaf.PostMedia: commit media: %w", err)
	}
	l.agent.SyncNow()
	body, err := mediaBody(uuid, path, mimeType, len(data))
	if err != nil {
		return fmt.Errorf("coord.Leaf.PostMedia: encode body: %w", err)
	}
	if err := l.coord.sendChat(ctx, thread, body); err != nil {
		return fmt.Errorf("coord.Leaf.PostMedia: %w", err)
	}
	return nil
}

// mediaArtifactPath builds a unique repo-relative path under
// `media/<slotID>/<unix-nanos>.bin` so concurrent PostMedia calls in
// the same slot do not collide.
func mediaArtifactPath(slotID string, now time.Time) string {
	return fmt.Sprintf("media/%s/%d.bin", slotID, now.UnixNano())
}

// mediaBody renders the in-band chat envelope referencing the
// just-committed media artifact. The mediaBodyPrefix sentinel lets
// chat subscribers detect the envelope and decode the JSON tail.
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
