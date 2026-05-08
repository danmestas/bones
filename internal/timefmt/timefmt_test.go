package timefmt

import (
	"regexp"
	"testing"
	"time"
)

// withLocal swaps time.Local for the duration of fn. Restores on exit
// even if fn panics. Tests that exercise Display need this because
// Display's output depends on time.Local, and the host CI runner may
// not be in the zone the test asserts on.
func withLocal(t *testing.T, zone string) func() {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("load zone %q: %v", zone, err)
	}
	prev := time.Local
	time.Local = loc
	return func() { time.Local = prev }
}

// TestLogged_AlwaysUTC pins the load-bearing guarantee: Logged returns
// the same wall-clock string regardless of the system zone. This is
// what makes log lines correlatable across hosts.
func TestLogged_AlwaysUTC(t *testing.T) {
	// Pick an instant whose UTC representation is unambiguous.
	instant := time.Date(2026, 5, 8, 12, 30, 45, 0, time.UTC)
	want := "2026-05-08T12:30:45Z"

	for _, zone := range []string{"UTC", "America/Los_Angeles", "Asia/Tokyo"} {
		zone := zone
		t.Run(zone, func(t *testing.T) {
			restore := withLocal(t, zone)
			defer restore()
			if got := Logged(instant); got != want {
				t.Errorf("Logged in zone=%s = %q, want %q", zone, got, want)
			}
		})
	}
}

// TestLogged_DSTBoundary pins behavior across the spring-forward and
// fall-back transitions. The operator-facing wall clock changes; the
// underlying instant in UTC does not, so Logged's output reflects the
// monotonic instant rather than the wall-clock skip/repeat.
func TestLogged_DSTBoundary(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load LA: %v", err)
	}

	// Spring-forward 2026: LA jumps from 2026-03-08 02:00 PST to
	// 2026-03-08 03:00 PDT. The instant at 10:00 UTC sits squarely on
	// the PDT side of the transition.
	springInstant := time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)
	if got, want := Logged(springInstant.In(la)), "2026-03-08T10:00:00Z"; got != want {
		t.Errorf("Logged on spring-forward = %q, want %q", got, want)
	}

	// Fall-back 2026: LA falls from 2026-11-01 02:00 PDT to
	// 2026-11-01 01:00 PST. The instant at 09:30 UTC sits on the PST
	// side of the transition.
	fallInstant := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	if got, want := Logged(fallInstant.In(la)), "2026-11-01T09:30:00Z"; got != want {
		t.Errorf("Logged on fall-back = %q, want %q", got, want)
	}
}

// TestDisplay_UTCSystem pins the UTC-system case: an operator running
// with TZ=UTC sees a "UTC" zone abbreviation, not a blank.
func TestDisplay_UTCSystem(t *testing.T) {
	restore := withLocal(t, "UTC")
	defer restore()

	instant := time.Date(2026, 5, 8, 12, 30, 45, 0, time.UTC)
	got := Display(instant)
	if got != "12:30:45 UTC" {
		t.Errorf("Display in UTC = %q, want %q", got, "12:30:45 UTC")
	}
}

// TestDisplay_LosAngeles pins the canonical PST/PDT case: an operator
// in California sees their wall clock plus the appropriate three-letter
// zone abbreviation depending on whether DST is in effect.
func TestDisplay_LosAngeles(t *testing.T) {
	restore := withLocal(t, "America/Los_Angeles")
	defer restore()

	// 2026-01-15 is solidly inside PST (UTC-8). 20:30:45 UTC = 12:30:45
	// local, abbreviation PST.
	winter := time.Date(2026, 1, 15, 20, 30, 45, 0, time.UTC)
	if got, want := Display(winter), "12:30:45 PST"; got != want {
		t.Errorf("Display PST = %q, want %q", got, want)
	}

	// 2026-07-15 is solidly inside PDT (UTC-7). 19:30:45 UTC = 12:30:45
	// local, abbreviation PDT.
	summer := time.Date(2026, 7, 15, 19, 30, 45, 0, time.UTC)
	if got, want := Display(summer), "12:30:45 PDT"; got != want {
		t.Errorf("Display PDT = %q, want %q", got, want)
	}
}

// TestDisplay_Tokyo pins a non-DST zone with a non-three-letter
// abbreviation in Go's tzdata: JST.
func TestDisplay_Tokyo(t *testing.T) {
	restore := withLocal(t, "Asia/Tokyo")
	defer restore()

	instant := time.Date(2026, 5, 8, 3, 30, 45, 0, time.UTC)
	if got, want := Display(instant), "12:30:45 JST"; got != want {
		t.Errorf("Display JST = %q, want %q", got, want)
	}
}

// TestDisplay_FormatShape asserts the rendered string always matches
// "HH:MM:SS <abbr>" so downstream snapshot tests can rely on a stable
// regex even when the host zone changes.
func TestDisplay_FormatShape(t *testing.T) {
	for _, zone := range []string{"UTC", "America/Los_Angeles", "Asia/Tokyo"} {
		zone := zone
		t.Run(zone, func(t *testing.T) {
			restore := withLocal(t, zone)
			defer restore()

			got := Display(time.Date(2026, 5, 8, 12, 30, 45, 0, time.UTC))
			re := regexp.MustCompile(`^\d{2}:\d{2}:\d{2} \S+$`)
			if !re.MatchString(got) {
				t.Errorf("Display in %s = %q, want HH:MM:SS <abbr>", zone, got)
			}
		})
	}
}

// TestLoggedShort_Shape pins the format of the (currently unused)
// short helper. Defined so a future addition has a regression
// signature to match.
func TestLoggedShort_Shape(t *testing.T) {
	instant := time.Date(2026, 5, 8, 12, 30, 45, 0, time.UTC)
	if got, want := LoggedShort(instant), "12:30:45Z"; got != want {
		t.Errorf("LoggedShort = %q, want %q", got, want)
	}
}

// TestLogged_RoundTrip pins that Logged's output is parseable as
// RFC3339 and round-trips back to the same instant. This is a sanity
// check on the helper, not the spec — the spec is "RFC3339 in UTC".
func TestLogged_RoundTrip(t *testing.T) {
	instant := time.Date(2026, 5, 8, 12, 30, 45, 0, time.UTC)
	got := Logged(instant)
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("Logged output %q failed to parse as RFC3339: %v", got, err)
	}
	if !parsed.Equal(instant) {
		t.Errorf("round-trip mismatch: %v != %v", parsed, instant)
	}
}
