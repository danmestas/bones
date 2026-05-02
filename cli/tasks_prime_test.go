package cli

import (
	"testing"

	"github.com/danmestas/bones/internal/coord"
)

// TestIsEmptyPrime_AllEmpty pins the no-noise case: zero of
// everything (no peers either) is empty.
func TestIsEmptyPrime_AllEmpty(t *testing.T) {
	if !isEmptyPrime(coord.PrimeResult{}) {
		t.Errorf("zero PrimeResult should be empty")
	}
}

// TestIsEmptyPrime_SelfOnlyPeers pins the documented edge case:
// presence-of-self in the peers list isn't useful to attach, so
// len(Peers) == 1 still counts as empty.
func TestIsEmptyPrime_SelfOnlyPeers(t *testing.T) {
	r := coord.PrimeResult{
		Peers: make([]coord.Presence, 1),
	}
	if !isEmptyPrime(r) {
		t.Errorf("self-only peers should still be empty")
	}
}

// TestIsEmptyPrime_NonEmptyOpenTasks pins the negative case: a
// single open task makes the result worth attaching.
func TestIsEmptyPrime_NonEmptyOpenTasks(t *testing.T) {
	r := coord.PrimeResult{
		OpenTasks: make([]coord.Task, 1),
	}
	if isEmptyPrime(r) {
		t.Errorf("open tasks present, should not be empty")
	}
}

// TestIsEmptyPrime_TwoPeers pins the threshold: more than one peer
// means at least one other agent is online — worth attaching.
func TestIsEmptyPrime_TwoPeers(t *testing.T) {
	r := coord.PrimeResult{
		Peers: make([]coord.Presence, 2),
	}
	if isEmptyPrime(r) {
		t.Errorf("two peers means another agent is online; not empty")
	}
}
