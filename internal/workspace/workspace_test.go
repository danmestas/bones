package workspace

import "testing"

func TestPackageBuilds(t *testing.T) {
	// Sanity: exported symbols compile and sentinel errors are distinct.
	errs := []error{ErrAlreadyInitialized, ErrNoWorkspace, ErrLeafUnreachable, ErrLeafStartTimeout}
	seen := map[error]bool{}
	for _, e := range errs {
		if e == nil {
			t.Fatal("nil sentinel")
		}
		if seen[e] {
			t.Fatalf("duplicate sentinel: %v", e)
		}
		seen[e] = true
	}
}
