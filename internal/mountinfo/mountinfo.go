// Package mountinfo lists the folders mounted into a Linux container, for
// suggesting where its recordings can go: in a container only a mounted
// volume outlives the container and reaches the host.
package mountinfo

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mount is one suggested folder.
type Mount struct {
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
}

// skipped are the container's own folders, never a place for recordings.
var skipped = []string{"/proc", "/sys", "/dev", "/etc", "/run", "/usr", "/config", "/cache"}

// Parse reads mountinfo and returns its directory mounts, leaving out the
// root, the container's own folders and exclude. isDir and writable test a
// mount point; Folders passes the real filesystem.
func Parse(r io.Reader, exclude []string, isDir, writable func(string) bool) []Mount {
	excluded := map[string]bool{}
	for _, e := range exclude {
		if e != "" {
			excluded[filepath.Clean(e)] = true
		}
	}
	seen := map[string]bool{}
	var out []Mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// id parent major:minor root mountpoint options [optional...] - fstype source superopts
		if len(f) < 6 {
			continue
		}
		p := filepath.Clean(unescape(f[4]))
		if p == "/" || seen[p] || excluded[p] || isSkipped(p) || !isDir(p) {
			continue
		}
		seen[p] = true
		ro := false
		for _, o := range strings.Split(f[5], ",") {
			if o == "ro" {
				ro = true
			}
		}
		out = append(out, Mount{Path: p, Writable: !ro && writable(p)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func isSkipped(p string) bool {
	for _, s := range skipped {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

// unescape decodes mountinfo's octal escapes (\040 for a space, and so on).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1:i+4]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := range 3 {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// Folders lists this process's mounted folders, or nil where there is no
// mountinfo (anything but Linux).
func Folders(exclude ...string) []Mount {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	return Parse(f, exclude, isDir, canWrite)
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func canWrite(p string) bool {
	f, err := os.CreateTemp(p, ".vlrec-writable-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}
