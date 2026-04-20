// Package workspace manages an agent-infra workspace: the .agent-infra/
// directory, its on-disk config, and the associated leaf daemon process.
//
// Two entry points:
//
//	Init creates a fresh workspace and starts a leaf daemon.
//	Join locates an existing workspace (walking up from cwd) and verifies
//	its leaf is reachable.
package workspace

import (
	"context"
	"errors"
)

// Info describes a live workspace. Returned by both Init and Join.
type Info struct {
	AgentID      string
	NATSURL      string
	LeafHTTPURL  string
	RepoPath     string
	WorkspaceDir string
}

var (
	ErrAlreadyInitialized = errors.New("workspace already initialized")
	ErrNoWorkspace        = errors.New("no agent-infra workspace found")
	ErrLeafUnreachable    = errors.New("leaf daemon not reachable")
	ErrLeafStartTimeout   = errors.New("leaf daemon failed to start within timeout")
)

// Init creates a fresh workspace rooted at cwd, starts a leaf daemon, and
// returns its connection info. Returns ErrAlreadyInitialized if .agent-infra/
// already exists in cwd.
func Init(ctx context.Context, cwd string) (Info, error) {
	return Info{}, errors.New("not implemented")
}

// Join locates the nearest .agent-infra/ walking up from cwd and verifies
// the recorded leaf is still reachable.
func Join(ctx context.Context, cwd string) (Info, error) {
	return Info{}, errors.New("not implemented")
}
