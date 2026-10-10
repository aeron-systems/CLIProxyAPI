//go:build !windows

package codex

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSaveTokenToFile_WritesOwnerOnly(t *testing.T) {
	oldMask := syscall.Umask(0o022)
	defer syscall.Umask(oldMask)

	dir := t.TempDir()
	newPath := filepath.Join(dir, "codex-new.json")
	existingPath := filepath.Join(dir, "codex-existing.json")
	if err := os.WriteFile(existingPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	if err := os.Chmod(existingPath, 0o644); err != nil {
		t.Fatalf("chmod existing file: %v", err)
	}

	for _, path := range []string{newPath, existingPath} {
		storage := &CodexTokenStorage{Type: "codex", AccessToken: "access", RefreshToken: "refresh"}
		if err := storage.SaveTokenToFile(path); err != nil {
			t.Fatalf("SaveTokenToFile(%s): %v", filepath.Base(path), err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(path), err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", filepath.Base(path), got)
		}
	}
}
