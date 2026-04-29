//go:build race

package hubleafe2e

// raceDetectorEnabled reports whether the binary was built with -race.
// Set via build-tag pair (race_on_test.go / race_off_test.go) so the e2e
// test can skip itself under `make race` without losing coverage in the
// non-race `make test` path.
const raceDetectorEnabled = true
