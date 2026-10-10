//go:build !windows

package misc

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteCredentialFile_TightensExistingFile(t *testing.T) {
	oldMask := syscall.Umask(0o022)
	defer syscall.Umask(oldMask)

	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("old contents that are longer"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := WriteCredentialFile(path, []byte("new")); err != nil {
		t.Fatalf("WriteCredentialFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("contents = %q, want %q", data, "new")
	}
}
