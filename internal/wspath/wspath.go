// Package wspath defines the typed coordination key for a workspace
// file. A Path is an absolute, syntactically clean filesystem path
// that the coordination layer uses as a hold key, hold-release key,
// and the value carried in coord.File. The substrate (holds) and the
// domain (coord, swarm, dispatch) agree on the same key by
// construction; no caller hand-rolls path normalization.
//
// Paths are produced by exactly two constructors: New, which wraps an
// already-absolute path, and NewRelative, which joins a workspace-
// relative path onto an absolute workspace root and rejects any
// result that escapes the root via "..". Both return ErrInvalid on
// bad input rather than producing a Path that fails downstream.
//
// The package re-exports as coord.Path via a type alias so the public
// vocabulary stays "coord.Path" while the implementation stays
// dependency-free. Holds depends on wspath directly.
package wspath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Path is the typed coordination key for a workspace file. The zero
// value is invalid — use IsZero to test for it.
type Path struct {
	abs string
}

// ErrInvalid wraps every constructor failure. Callers can errors.Is
// against it to distinguish bad input from substrate errors.
var ErrInvalid = errors.New("wspath: invalid path")

// New wraps an absolute filesystem path in a Path. The input is
// passed through filepath.Clean, so "/a//b/", "/a/./b", and "/a/b"
// all produce the same canonical Path. Returns an ErrInvalid-wrapped
// error when input is empty or not absolute.
func New(abs string) (Path, error) {
	if abs == "" {
		return Path{}, fmt.Errorf("%w: empty input", ErrInvalid)
	}
	if !filepath.IsAbs(abs) {
		return Path{}, fmt.Errorf(
			"%w: %q is not absolute", ErrInvalid, abs,
		)
	}
	return Path{abs: filepath.Clean(abs)}, nil
}

// NewRelative joins workspaceDir and rel into a Path anchored inside
// workspaceDir. workspaceDir must itself be absolute. The cleaned
// join must lie at or under workspaceDir; a rel that escapes via
// ".." is rejected. rel must not be absolute (use New for that path).
func NewRelative(workspaceDir, rel string) (Path, error) {
	if workspaceDir == "" {
		return Path{}, fmt.Errorf(
			"%w: workspaceDir is empty", ErrInvalid,
		)
	}
	if !filepath.IsAbs(workspaceDir) {
		return Path{}, fmt.Errorf(
			"%w: workspaceDir %q is not absolute",
			ErrInvalid, workspaceDir,
		)
	}
	if rel == "" {
		return Path{}, fmt.Errorf("%w: rel is empty", ErrInvalid)
	}
	if filepath.IsAbs(rel) {
		return Path{}, fmt.Errorf(
			"%w: rel %q is absolute (use New)", ErrInvalid, rel,
		)
	}
	root := filepath.Clean(workspaceDir)
	joined := filepath.Clean(filepath.Join(root, rel))
	if joined != root && !strings.HasPrefix(
		joined, root+string(filepath.Separator),
	) {
		return Path{}, fmt.Errorf(
			"%w: %q escapes workspace %q", ErrInvalid, rel, root,
		)
	}
	return Path{abs: joined}, nil
}

// AsAbsolute returns the canonical absolute path string.
func (p Path) AsAbsolute() string { return p.abs }

// AsKey returns the coordination-key form of the path. Currently
// equal to AsAbsolute; kept distinct so a future schema change to
// the hold-bucket key format can stay invisible to callers.
func (p Path) AsKey() string { return p.abs }

// String implements fmt.Stringer; returns AsAbsolute.
func (p Path) String() string { return p.abs }

// IsZero reports whether p is the zero Path. The zero value is
// invalid and must not be passed across coordination seams.
func (p Path) IsZero() bool { return p.abs == "" }

// Must wraps New and panics on error. Intended for tests and for
// programs whose input is a compile-time-known absolute path; every
// other caller should use New and handle ErrInvalid.
func Must(abs string) Path {
	p, err := New(abs)
	if err != nil {
		panic(fmt.Errorf("wspath.Must: %w", err))
	}
	return p
}
