package wspath_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/danmestas/bones/internal/wspath"
)

func TestNew_Absolute(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/a/b.txt", "/a/b.txt"},
		{"/a//b.txt", "/a/b.txt"},
		{"/a/./b.txt", "/a/b.txt"},
		{"/a/b/", "/a/b"},
		{"/a/b/../c.txt", "/a/c.txt"},
		{"/a/b/../../c.txt", "/c.txt"},
		{"/", "/"},
	}
	for _, c := range cases {
		p, err := wspath.New(c.in)
		if err != nil {
			t.Errorf("New(%q): unexpected error %v", c.in, err)
			continue
		}
		if got := p.AsAbsolute(); got != c.want {
			t.Errorf("New(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNew_RejectsBadInput(t *testing.T) {
	cases := []string{"", "relative/path", "./foo", "../escape"}
	for _, in := range cases {
		_, err := wspath.New(in)
		if err == nil {
			t.Errorf("New(%q): expected error", in)
			continue
		}
		if !errors.Is(err, wspath.ErrInvalid) {
			t.Errorf("New(%q): got %v, want ErrInvalid", in, err)
		}
	}
}

func TestNewRelative_HappyPath(t *testing.T) {
	cases := []struct {
		ws, rel, want string
	}{
		{"/work", "src/foo.go", "/work/src/foo.go"},
		{"/work/", "src/foo.go", "/work/src/foo.go"},
		{"/work", "./src/foo.go", "/work/src/foo.go"},
		{"/work", "src/../lib/bar.go", "/work/lib/bar.go"},
		{"/work/proj", "a", "/work/proj/a"},
	}
	for _, c := range cases {
		p, err := wspath.NewRelative(c.ws, c.rel)
		if err != nil {
			t.Errorf(
				"NewRelative(%q, %q): unexpected error %v",
				c.ws, c.rel, err,
			)
			continue
		}
		if got := p.AsAbsolute(); got != c.want {
			t.Errorf(
				"NewRelative(%q, %q): got %q, want %q",
				c.ws, c.rel, got, c.want,
			)
		}
	}
}

func TestNewRelative_RejectsEscape(t *testing.T) {
	cases := []struct {
		ws, rel string
	}{
		{"/work", "../escape"},
		{"/work", "src/../../escape"},
		{"/work/proj", "../sibling"},
		// Escapes that resolve to root.
		{"/work", "../.."},
	}
	for _, c := range cases {
		_, err := wspath.NewRelative(c.ws, c.rel)
		if err == nil {
			t.Errorf(
				"NewRelative(%q, %q): expected escape error",
				c.ws, c.rel,
			)
			continue
		}
		if !errors.Is(err, wspath.ErrInvalid) {
			t.Errorf(
				"NewRelative(%q, %q): got %v, want ErrInvalid",
				c.ws, c.rel, err,
			)
		}
	}
}

func TestNewRelative_RejectsBadArgs(t *testing.T) {
	cases := []struct {
		name, ws, rel string
	}{
		{"empty workspace", "", "a"},
		{"relative workspace", "work", "a"},
		{"empty rel", "/work", ""},
		{"absolute rel", "/work", "/foo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := wspath.NewRelative(c.ws, c.rel)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !errors.Is(err, wspath.ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
		})
	}
}

func TestPath_Accessors(t *testing.T) {
	p, err := wspath.New("/a/b/c.txt")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.AsAbsolute(); got != "/a/b/c.txt" {
		t.Errorf("AsAbsolute: %q", got)
	}
	if got := p.AsKey(); got != "/a/b/c.txt" {
		t.Errorf("AsKey: %q", got)
	}
	if got := p.String(); got != "/a/b/c.txt" {
		t.Errorf("String: %q", got)
	}
	if p.IsZero() {
		t.Errorf("IsZero on non-zero")
	}
	var zero wspath.Path
	if !zero.IsZero() {
		t.Errorf("IsZero on zero: false")
	}
}

func TestPath_Equality(t *testing.T) {
	p1, _ := wspath.New("/a/b")
	p2, _ := wspath.New("/a//b")
	p3, _ := wspath.New("/a/c")
	if p1 != p2 {
		t.Errorf("expected equal: %v %v", p1, p2)
	}
	if p1 == p3 {
		t.Errorf("expected unequal: %v %v", p1, p3)
	}
}

func TestNewRelative_CrossPlatform(t *testing.T) {
	// Sanity: filepath.Separator is what we expect on this platform.
	if filepath.Separator == 0 {
		t.Skip("filepath.Separator is zero — odd platform")
	}
}
