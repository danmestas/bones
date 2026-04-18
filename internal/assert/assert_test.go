package assert

import (
	"errors"
	"strings"
	"testing"
)

func requirePanic(t *testing.T, fn func(), wantSubstr string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q, got no panic", wantSubstr)
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected panic value to be string, got %T (%v)", r, r)
		}
		if !strings.HasPrefix(msg, "assert: ") {
			t.Fatalf("expected panic message to have %q prefix, got %q", "assert: ", msg)
		}
		if !strings.Contains(msg, wantSubstr) {
			t.Fatalf("expected panic message %q to contain %q", msg, wantSubstr)
		}
	}()
	fn()
}

func requireNoPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("expected no panic, got %v", r)
		}
	}()
	fn()
}

func TestPrecondition(t *testing.T) {
	cases := []struct {
		name       string
		cond       bool
		msg        string
		args       []any
		wantPanic  bool
		wantSubstr string
	}{
		{"true no panic", true, "should not fire", nil, false, ""},
		{"false plain", false, "bad state", nil, true, "bad state"},
		{"false formatted", false, "x=%d", []any{42}, true, "x=42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := func() { Precondition(tc.cond, tc.msg, tc.args...) }
			if tc.wantPanic {
				requirePanic(t, fn, tc.wantSubstr)
			} else {
				requireNoPanic(t, fn)
			}
		})
	}
}

func TestPostcondition(t *testing.T) {
	cases := []struct {
		name       string
		cond       bool
		msg        string
		args       []any
		wantPanic  bool
		wantSubstr string
	}{
		{"true no panic", true, "should not fire", nil, false, ""},
		{"false plain", false, "exit invariant broken", nil, true, "exit invariant broken"},
		{"false formatted", false, "count=%d", []any{7}, true, "count=7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := func() { Postcondition(tc.cond, tc.msg, tc.args...) }
			if tc.wantPanic {
				requirePanic(t, fn, tc.wantSubstr)
			} else {
				requireNoPanic(t, fn)
			}
		})
	}
}

func TestNoError(t *testing.T) {
	boom := errors.New("boom")
	t.Run("nil no panic", func(t *testing.T) {
		requireNoPanic(t, func() { NoError(nil, "should not fire") })
	})
	t.Run("non-nil plain", func(t *testing.T) {
		requirePanic(t, func() { NoError(boom, "load failed") }, "load failed: boom")
	})
	t.Run("non-nil formatted", func(t *testing.T) {
		requirePanic(t, func() { NoError(boom, "op=%s", "read") }, "op=read: boom")
	})
}

func TestNotNil(t *testing.T) {
	t.Run("untyped nil panics", func(t *testing.T) {
		requirePanic(t, func() { NotNil(nil, "v") }, "v")
	})
	t.Run("typed nil ptr panics", func(t *testing.T) {
		var p *int
		requirePanic(t, func() { NotNil(p, "p") }, "p")
	})
	t.Run("typed nil slice panics", func(t *testing.T) {
		var s []int
		requirePanic(t, func() { NotNil(s, "s") }, "s")
	})
	t.Run("typed nil map panics", func(t *testing.T) {
		var m map[string]int
		requirePanic(t, func() { NotNil(m, "m") }, "m")
	})
	t.Run("typed nil chan panics", func(t *testing.T) {
		var c chan int
		requirePanic(t, func() { NotNil(c, "c") }, "c")
	})
	t.Run("typed nil func panics", func(t *testing.T) {
		var f func()
		requirePanic(t, func() { NotNil(f, "f") }, "f")
	})
	t.Run("non-nil ptr no panic", func(t *testing.T) {
		x := 7
		requireNoPanic(t, func() { NotNil(&x, "x") })
	})
	t.Run("non-nil value no panic", func(t *testing.T) {
		requireNoPanic(t, func() { NotNil(42, "int") })
		requireNoPanic(t, func() { NotNil("hi", "s") })
	})
	t.Run("formatted message", func(t *testing.T) {
		var p *int
		requirePanic(t, func() { NotNil(p, "field=%s", "taskID") }, "field=taskID")
	})
}

func TestNotEmpty(t *testing.T) {
	cases := []struct {
		name       string
		s          string
		msg        string
		args       []any
		wantPanic  bool
		wantSubstr string
	}{
		{"non-empty no panic", "x", "should not fire", nil, false, ""},
		{"empty plain", "", "agentID required", nil, true, "agentID required"},
		{"empty formatted", "", "field=%s", []any{"taskID"}, true, "field=taskID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := func() { NotEmpty(tc.s, tc.msg, tc.args...) }
			if tc.wantPanic {
				requirePanic(t, fn, tc.wantSubstr)
			} else {
				requireNoPanic(t, fn)
			}
		})
	}
}

func TestPrefixOnEveryPanic(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
	}{
		{"Precondition", func() { Precondition(false, "x") }},
		{"Postcondition", func() { Postcondition(false, "x") }},
		{"NoError", func() { NoError(errors.New("e"), "x") }},
		{"NotNil", func() { NotNil(nil, "x") }},
		{"NotEmpty", func() { NotEmpty("", "x") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requirePanic(t, tc.fn, "")
		})
	}
}
