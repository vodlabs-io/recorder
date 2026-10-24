package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckContainerDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}

	t.Setenv("VLREC_CONTAINER", "")
	if err := checkContainerDir(locked); err != nil {
		t.Errorf("checked outside a container: %v", err)
	}

	t.Setenv("VLREC_CONTAINER", "1")
	if err := checkContainerDir(dir); err != nil {
		t.Errorf("writable folder rejected: %v", err)
	}
	err := checkContainerDir(locked)
	if err == nil {
		t.Fatal("unwritable folder accepted")
	}
	for _, want := range []string{"uid ", "gid ", "chown", "user:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}
