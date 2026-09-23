package vlconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureStorageDirCreatesLeaf(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recordings")
	if err := EnsureStorageDir(dir); err != nil {
		t.Fatalf("EnsureStorageDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

func TestEnsureStorageDirExistingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureStorageDir(dir); err != nil {
		t.Fatalf("EnsureStorageDir on an existing folder: %v", err)
	}
}

// A path whose parent is missing stands for an unmounted drive. Creating the
// whole tree there would shadow the mount, so it must fail instead.
func TestEnsureStorageDirDoesNotCreateMissingParents(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "not-mounted", "recordings")
	if err := EnsureStorageDir(dir); err == nil {
		t.Fatal("expected an error for a missing parent, got nil")
	}
	if _, err := os.Stat(filepath.Join(root, "not-mounted")); !os.IsNotExist(err) {
		t.Fatalf("the missing parent was created: %v", err)
	}
}

func TestEnsureStorageDirRejectsEmpty(t *testing.T) {
	if err := EnsureStorageDir("   "); err == nil {
		t.Fatal("expected an error for an empty path, got nil")
	}
}
