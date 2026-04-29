package githook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallFreshGit(t *testing.T) {
	gitDir := setupGitDir(t)

	if err := Install(gitDir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	if !strings.Contains(string(data), Marker) {
		t.Errorf("hook missing marker: %q", Marker)
	}
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&0o100 == 0 {
		t.Errorf("hook not executable: mode=%o", info.Mode())
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	gitDir := setupGitDir(t)

	if err := Install(gitDir); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	first, _ := os.ReadFile(hookPath)
	if err := Install(gitDir); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	second, _ := os.ReadFile(hookPath)
	if string(first) != string(second) {
		t.Error("re-install changed hook content")
	}
	saved := hookPath + SavedSuffix
	if _, err := os.Stat(saved); err == nil {
		t.Error("re-install over bones hook created bogus saved file")
	}
}

func TestInstallPreservesUserHook(t *testing.T) {
	gitDir := setupGitDir(t)
	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	userHook := "#!/bin/sh\necho 'user hook'\n"
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte(userHook), 0o755); err != nil {
		t.Fatalf("write user hook: %v", err)
	}

	if err := Install(gitDir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	saved, err := os.ReadFile(hookPath + SavedSuffix)
	if err != nil {
		t.Fatalf("read saved: %v", err)
	}
	if string(saved) != userHook {
		t.Errorf("saved hook mismatch: got %q want %q", saved, userHook)
	}
	bones, _ := os.ReadFile(hookPath)
	if !strings.Contains(string(bones), Marker) {
		t.Error("bones hook not installed at primary path")
	}
}

func TestUninstallRestoresSaved(t *testing.T) {
	gitDir := setupGitDir(t)
	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	userHook := "#!/bin/sh\necho 'user'\n"
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte(userHook), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Install(gitDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := Uninstall(gitDir); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read post-uninstall: %v", err)
	}
	if string(got) != userHook {
		t.Errorf("user hook not restored: got %q", got)
	}
	if _, err := os.Stat(hookPath + SavedSuffix); err == nil {
		t.Error("saved file leaked after restore")
	}
}

func TestUninstallNoBonesHookIsNoop(t *testing.T) {
	gitDir := setupGitDir(t)
	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	userHook := "#!/bin/sh\necho 'user'\n"
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte(userHook), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := Uninstall(gitDir); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	got, _ := os.ReadFile(hookPath)
	if string(got) != userHook {
		t.Errorf("user hook clobbered: got %q", got)
	}
}

func TestIsInstalled(t *testing.T) {
	gitDir := setupGitDir(t)
	if ok, err := IsInstalled(gitDir); err != nil || ok {
		t.Errorf("fresh repo: got (%v, %v), want (false, nil)", ok, err)
	}
	if err := Install(gitDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if ok, err := IsInstalled(gitDir); err != nil || !ok {
		t.Errorf("after Install: got (%v, %v), want (true, nil)", ok, err)
	}
}

func TestFindGitDir(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	got := FindGitDir(deep)
	if got != gitDir {
		t.Errorf("FindGitDir: got %q want %q", got, gitDir)
	}

	other := t.TempDir()
	if got := FindGitDir(other); got != "" {
		t.Errorf("non-git tree: got %q want empty", got)
	}
}

func setupGitDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "hooks"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return gitDir
}
