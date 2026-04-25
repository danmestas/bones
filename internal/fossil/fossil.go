// Package fossil wraps libfossil with the coord-facing subset of
// operations needed by ADR 0010: open a per-agent checkout, commit files
// as the agent, read files at a rev, diff, navigate, and merge branches.
//
// The package is internal — callers are coord and its tests. Revs are
// opaque UUID strings (Fossil's 40-char SHA-1); no libfossil int64 rids
// cross this package's API.
//
// Concurrency: a single Manager is not safe for concurrent use at the
// commit/checkout layer. libfossil's *Checkout is documented as
// single-threaded, and this package inherits that contract. Close is
// idempotent and safe to call from any goroutine.
package fossil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Sentinel errors returned by Manager methods.
var (
	// ErrClosed is returned by any method called after Close.
	ErrClosed = errors.New("fossil: manager closed")

	// ErrNoCheckout is returned by Checkout (navigation) when called
	// before CreateCheckout has primed the checkout directory.
	ErrNoCheckout = errors.New("fossil: no checkout; call CreateCheckout first")

	// ErrRevNotFound is returned when a rev UUID is not present in the
	// repo.
	ErrRevNotFound = errors.New("fossil: rev not found")

	// ErrBranchNotFound is returned when a referenced branch name is not
	// present in the repo.
	ErrBranchNotFound = errors.New("fossil: branch not found")

	// ErrFileNotFound is returned by OpenFile when the file is not
	// tracked in the given rev.
	ErrFileNotFound = errors.New("fossil: file not found in rev")
)

// Config has three required fields. There are no silent defaults.
type Config struct {
	// AgentID is the author stamped on every commit, and also the
	// sub-directory under CheckoutRoot where this Manager writes.
	AgentID string

	// RepoPath is the absolute path to the shared fossil repo DB.
	// If the file does not exist, Open creates a new repo there.
	RepoPath string

	// CheckoutRoot is the absolute directory under which per-agent
	// checkouts live. The Manager writes to CheckoutRoot/<AgentID>/.
	CheckoutRoot string
}

// File is a single file to commit. Path is relative to the checkout
// root with no leading slash; Content is the raw bytes to write.
type File struct {
	Path    string
	Content []byte
}

// Manager wraps a single per-agent checkout against a shared fossil
// repo. Construct via Open; release with Close.
type Manager struct {
	cfg      Config
	repo     *libfossil.Repo
	checkout *libfossil.Checkout // nil until CreateCheckout succeeds
	dir      string              // cfg.CheckoutRoot/cfg.AgentID
	done     atomic.Bool
}

// Open creates (if needed) and opens the fossil repo at cfg.RepoPath.
// It does NOT create the checkout — call CreateCheckout for that. The
// Manager takes ownership of the opened *libfossil.Repo and is
// responsible for closing it in Close.
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	assert.NotNil(ctx, "fossil.Open: ctx is nil")
	assert.NotEmpty(cfg.AgentID, "fossil.Open: cfg.AgentID is empty")
	assert.NotEmpty(cfg.RepoPath, "fossil.Open: cfg.RepoPath is empty")
	assert.NotEmpty(cfg.CheckoutRoot, "fossil.Open: cfg.CheckoutRoot is empty")

	var (
		repo *libfossil.Repo
		err  error
	)
	if _, statErr := os.Stat(cfg.RepoPath); statErr == nil {
		repo, err = libfossil.Open(cfg.RepoPath)
	} else if errors.Is(statErr, os.ErrNotExist) {
		repo, err = libfossil.Create(
			cfg.RepoPath, libfossil.CreateOpts{User: cfg.AgentID},
		)
	} else {
		return nil, fmt.Errorf("fossil.Open: stat repo: %w", statErr)
	}
	if err != nil {
		return nil, fmt.Errorf("fossil.Open: %w", err)
	}

	return &Manager{
		cfg:  cfg,
		repo: repo,
		dir:  filepath.Join(cfg.CheckoutRoot, cfg.AgentID),
	}, nil
}

// Close releases the checkout and repo. Safe to call more than once.
// Returns the first error seen but attempts to close both resources
// regardless.
func (m *Manager) Close() error {
	assert.NotNil(m, "fossil.Close: receiver is nil")
	if !m.done.CompareAndSwap(false, true) {
		return nil
	}
	var firstErr error
	if m.checkout != nil {
		if err := m.checkout.Close(); err != nil {
			firstErr = fmt.Errorf("fossil.Close: checkout: %w", err)
		}
		m.checkout = nil
	}
	if m.repo != nil {
		if err := m.repo.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("fossil.Close: repo: %w", err)
		}
		m.repo = nil
	}
	return firstErr
}

// CreateCheckout ensures the per-agent checkout directory exists and is
// linked to the repo. libfossil's checkout machinery requires at least
// one checkin in the repo before a checkout can be created, so this
// must be called after the first Commit lands a tip. Idempotent: once
// m.checkout is set, subsequent calls are no-ops.
//
// If the dir already contains a .fslckout marker, the existing
// checkout is reopened; otherwise a fresh one is created and populated
// from the current tip.
func (m *Manager) CreateCheckout(ctx context.Context) error {
	assert.NotNil(ctx, "fossil.CreateCheckout: ctx is nil")
	if m.done.Load() {
		return ErrClosed
	}
	if m.checkout != nil {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return fmt.Errorf("fossil.CreateCheckout: mkdir: %w", err)
	}

	marker := filepath.Join(m.dir, ".fslckout")
	var (
		co  *libfossil.Checkout
		err error
	)
	if _, statErr := os.Stat(marker); statErr == nil {
		co, err = m.repo.OpenCheckout(m.dir, libfossil.CheckoutOpenOpts{})
	} else if errors.Is(statErr, os.ErrNotExist) {
		co, err = m.repo.CreateCheckout(m.dir, libfossil.CheckoutCreateOpts{})
	} else {
		return fmt.Errorf("fossil.CreateCheckout: stat marker: %w", statErr)
	}
	if err != nil {
		return fmt.Errorf("fossil.CreateCheckout: %w", err)
	}
	m.checkout = co
	return nil
}

// Commit writes files into the repo as a new checkin authored by
// cfg.AgentID, chaining off the current branch tip. Returns the opaque
// UUID of the new checkin.
//
// The commit is made directly against the repo blob store, without
// routing through the checkout's vfile layer. This keeps Commit usable
// before the first checkin lands (libfossil's checkout requires a tip
// to exist, which is a chicken-and-egg for a fresh repo). An attached
// checkout is synced forward after the commit via Extract so on-disk
// state matches.
//
// If branch is non-empty, the new commit is placed on that named branch
// via a propagating "branch" tag on the artifact. Callers compose this
// with WouldFork to implement fork-on-conflict semantics at the coord
// layer (ADR 0010 §4-5). Pass "" to commit on the current branch.
func (m *Manager) Commit(
	ctx context.Context, message string, files []File, branch string,
) (string, error) {
	assert.NotNil(ctx, "fossil.Commit: ctx is nil")
	assert.NotEmpty(message, "fossil.Commit: message is empty")
	assert.Precondition(
		len(files) > 0, "fossil.Commit: files is empty",
	)
	if m.done.Load() {
		return "", ErrClosed
	}

	parent, _, err := m.tipRID()
	if err != nil {
		return "", fmt.Errorf("fossil.Commit: tip: %w", err)
	}

	toCommit := make([]libfossil.FileToCommit, 0, len(files))
	for _, f := range files {
		assert.NotEmpty(f.Path, "fossil.Commit: file.Path is empty")
		toCommit = append(toCommit, libfossil.FileToCommit{
			Name:    normalizePath(f.Path),
			Content: f.Content,
		})
	}
	opts := libfossil.CommitOpts{
		Files:    toCommit,
		Comment:  message,
		User:     m.cfg.AgentID,
		ParentID: parent,
	}
	if branch != "" {
		opts.Tags = []libfossil.TagSpec{
			{Name: "branch", Value: branch},
		}
	}
	rid, uuid, err := m.repo.Commit(opts)
	if err != nil {
		return "", fmt.Errorf("fossil.Commit: %w", err)
	}

	// If a checkout is attached, best-effort sync on-disk state forward
	// to the new tip. Extract (not Update) because the checkout's
	// vfile has no pending changes — we committed at the repo layer.
	// The commit itself is already durable at this point, so Extract
	// failures are swallowed and only leave the working copy stale.
	// Callers that need disk-sync semantics call Checkout(ctx, rev)
	// directly and observe any error there.
	if m.checkout != nil {
		_ = m.checkout.Extract(
			rid, libfossil.ExtractOpts{Force: true},
		)
	}
	return uuid, nil
}

// Pull fetches commits from the hub at hubURL and applies them to this
// Manager's repo. Repo-only — never touches the working tree. Idempotent
// on a repo already at hub's tip.
func (m *Manager) Pull(ctx context.Context, hubURL string) error {
	assert.NotNil(ctx, "fossil.Pull: ctx is nil")
	assert.NotEmpty(hubURL, "fossil.Pull: hubURL is empty")
	if m.done.Load() {
		return ErrClosed
	}
	if _, err := m.repo.Pull(ctx, hubURL, libfossil.PullOpts{}); err != nil {
		return fmt.Errorf("fossil.Pull: %w", err)
	}
	return nil
}

// Update merges repo-level changes into the attached working tree. Must be
// called after Pull and after CreateCheckout. Returns ErrNoCheckout if the
// checkout has not been created yet (Update needs a worktree to merge into).
// TargetRID=0 means "update to current branch tip", which is what coord
// uses for the retry-on-fork path.
func (m *Manager) Update(ctx context.Context) error {
	assert.NotNil(ctx, "fossil.Update: ctx is nil")
	if m.done.Load() {
		return ErrClosed
	}
	if m.checkout == nil {
		return ErrNoCheckout
	}
	if err := m.checkout.Update(libfossil.UpdateOpts{TargetRID: 0}); err != nil {
		return fmt.Errorf("fossil.Update: %w", err)
	}
	return nil
}

// Tip returns the manifest UUID at the head of the current branch's leaf
// commit, or "" if the repo has no checkins yet. Wraps the existing
// private tipRID helper. Used by the tip-broadcast subscriber to compare
// the broadcast manifest hash against local state for idempotency.
func (m *Manager) Tip(ctx context.Context) (string, error) {
	assert.NotNil(ctx, "fossil.Tip: ctx is nil")
	if m.done.Load() {
		return "", ErrClosed
	}
	_, uuid, err := m.tipRID()
	if err != nil {
		return "", fmt.Errorf("fossil.Tip: %w", err)
	}
	return uuid, nil
}

// WouldFork reports whether the next commit on the current branch
// would create a sibling leaf — i.e., another leaf already exists on
// this branch, so committing here would fork.
//
// Returns (false, nil) when no checkout is attached: a Manager that
// hasn't yet called CreateCheckout has no working-copy parent, and a
// fresh repo with no tip cannot fork by definition. Callers composing
// fork-on-conflict at a higher layer (ADR 0010 §4) invoke this before
// Commit and pass a branch name to Commit when the result is true.
func (m *Manager) WouldFork(ctx context.Context) (bool, error) {
	assert.NotNil(ctx, "fossil.WouldFork: ctx is nil")
	if m.done.Load() {
		return false, ErrClosed
	}
	if m.checkout == nil {
		return false, nil
	}
	fork, err := m.checkout.WouldFork()
	if err != nil {
		return false, fmt.Errorf("fossil.WouldFork: %w", err)
	}
	return fork, nil
}

// OpenFile returns the content of path as committed at rev. rev is an
// opaque UUID returned by Commit. Returns ErrRevNotFound if the rev
// does not exist and ErrFileNotFound if the file is not tracked in
// that rev.
func (m *Manager) OpenFile(
	ctx context.Context, rev, path string,
) ([]byte, error) {
	assert.NotNil(ctx, "fossil.OpenFile: ctx is nil")
	assert.NotEmpty(rev, "fossil.OpenFile: rev is empty")
	assert.NotEmpty(path, "fossil.OpenFile: path is empty")
	if m.done.Load() {
		return nil, ErrClosed
	}
	rid, err := m.ridFromUUID(rev)
	if err != nil {
		return nil, err
	}
	data, err := m.repo.ReadFile(rid, normalizePath(path))
	if err != nil {
		if errors.Is(err, libfossil.ErrFileNotFound) {
			return nil, fmt.Errorf(
				"fossil.OpenFile: %q in rev %s: %w",
				path, rev, ErrFileNotFound,
			)
		}
		return nil, fmt.Errorf("fossil.OpenFile: %w", err)
	}
	return data, nil
}

// Diff returns the unified diff for path between revA and revB. When
// the two revs are byte-identical for path, Diff returns an empty
// slice. revA and revB are opaque UUIDs; ErrRevNotFound is returned if
// either rev is missing.
func (m *Manager) Diff(
	ctx context.Context, revA, revB, path string,
) ([]byte, error) {
	assert.NotNil(ctx, "fossil.Diff: ctx is nil")
	assert.NotEmpty(revA, "fossil.Diff: revA is empty")
	assert.NotEmpty(revB, "fossil.Diff: revB is empty")
	assert.NotEmpty(path, "fossil.Diff: path is empty")
	if m.done.Load() {
		return nil, ErrClosed
	}
	ridA, err := m.ridFromUUID(revA)
	if err != nil {
		return nil, err
	}
	ridB, err := m.ridFromUUID(revB)
	if err != nil {
		return nil, err
	}
	entries, err := m.repo.Diff(ridA, ridB, normalizePath(path))
	if err != nil {
		return nil, fmt.Errorf("fossil.Diff: %w", err)
	}
	if len(entries) == 0 {
		return []byte{}, nil
	}
	return []byte(entries[0].Unified), nil
}

// Checkout moves the on-disk checkout to rev. Used for navigation or
// rollback. Requires CreateCheckout to have been called first;
// returns ErrNoCheckout otherwise. Returns ErrRevNotFound if the rev
// does not exist.
func (m *Manager) Checkout(ctx context.Context, rev string) error {
	assert.NotNil(ctx, "fossil.Checkout: ctx is nil")
	assert.NotEmpty(rev, "fossil.Checkout: rev is empty")
	if m.done.Load() {
		return ErrClosed
	}
	if m.checkout == nil {
		return ErrNoCheckout
	}
	rid, err := m.ridFromUUID(rev)
	if err != nil {
		return err
	}
	if err := m.checkout.Extract(
		rid, libfossil.ExtractOpts{Force: true},
	); err != nil {
		return fmt.Errorf("fossil.Checkout: extract: %w", err)
	}
	return nil
}

// Merge runs a three-way merge of branch src into branch dst with
// message as the commit message and cfg.AgentID as the author.
// Returns the UUID of the new merge commit on success. On conflict,
// returns an error for which errors.Is(err, libfossil.ErrMergeConflict)
// is true. If either branch is missing, wraps ErrBranchNotFound.
func (m *Manager) Merge(
	ctx context.Context, src, dst, message string,
) (string, error) {
	assert.NotNil(ctx, "fossil.Merge: ctx is nil")
	assert.NotEmpty(src, "fossil.Merge: src is empty")
	assert.NotEmpty(dst, "fossil.Merge: dst is empty")
	assert.NotEmpty(message, "fossil.Merge: message is empty")
	if m.done.Load() {
		return "", ErrClosed
	}
	_, uuid, err := m.repo.Merge(src, dst, message, m.cfg.AgentID)
	if err != nil {
		// libfossil wraps sql.ErrNoRows inside BranchTip; surface it
		// as ErrBranchNotFound so callers can distinguish "missing
		// branch" from merge-conflict or consistency errors.
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf(
				"fossil.Merge: %w: %v", ErrBranchNotFound, err,
			)
		}
		return "", fmt.Errorf("fossil.Merge: %w", err)
	}
	return uuid, nil
}

// tipRID returns the rid and uuid of the current branch tip, or (0,
// "", nil) if the repo has no checkins yet. A missing tip is a normal
// fresh-repo state, not an error.
func (m *Manager) tipRID() (int64, string, error) {
	var (
		rid  int64
		uuid string
	)
	// Secondary sort on rid breaks mtime ties when two commits land in
	// the same julian-day bucket — without it, the SQL engine may return
	// either row and Commit's ParentID becomes flaky. Mirrors the fix in
	// libfossil.Repo.BranchTip.
	err := m.repo.DB().QueryRow(`
		SELECT l.rid, b.uuid FROM leaf l
		JOIN event e ON e.objid=l.rid
		JOIN blob b ON b.rid=l.rid
		WHERE e.type='ci'
		ORDER BY e.mtime DESC, l.rid DESC LIMIT 1
	`).Scan(&rid, &uuid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", nil
		}
		return 0, "", err
	}
	return rid, uuid, nil
}

// normalizePath strips a single leading '/' so callers can pass
// absolute paths (required by coord Invariant 4) while libfossil stores
// repo-relative names and its checkout-extract guard (which rejects
// anything resolving outside the checkout dir) stays satisfied. The
// transform is byte-exact and single-slash on purpose: "/src/a.go" →
// "src/a.go", "src/a.go" passes through, "//x" → "/x" (still fails the
// guard — callers who want to trip that failure mode still can). We do
// NOT filepath.Clean here because OpenFile must round-trip the same
// bytes the caller passed to Commit.
func normalizePath(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}

// ridFromUUID resolves an opaque rev UUID to libfossil's internal
// int64 rid via a direct blob-table lookup. Returns ErrRevNotFound if
// the UUID is not present.
func (m *Manager) ridFromUUID(uuid string) (int64, error) {
	var rid int64
	err := m.repo.DB().QueryRow(
		`SELECT rid FROM blob WHERE uuid=?`, uuid,
	).Scan(&rid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf(
				"fossil: rev %s: %w", uuid, ErrRevNotFound,
			)
		}
		return 0, fmt.Errorf("fossil: resolve rev %s: %w", uuid, err)
	}
	return rid, nil
}
