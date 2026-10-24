package mountinfo

import (
	"reflect"
	"strings"
	"testing"
)

// A Docker Desktop container with compose volumes, a read-only archive, a
// secret file, and a path with a space.
const fixture = `541 470 0:178 / / rw,relatime master:220 - overlay overlay rw,lowerdir=/x
542 541 0:181 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
543 541 0:182 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
548 541 0:186 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro
553 541 254:1 /docker/volumes/x/_data /cache rw,relatime - ext4 /dev/vda1 rw
554 541 0:42 /Users/me/rec/config /config rw,relatime - fakeowner grpcfuse rw
555 541 0:42 /Users/me/rec/recordings /recordings rw,relatime - fakeowner grpcfuse rw
556 541 0:42 /Volumes/NAS/vods /archive ro,relatime - fakeowner grpcfuse rw
557 541 0:42 /Users/me/old\040vods /mnt/old\040vods rw,relatime - fakeowner grpcfuse rw
558 541 254:1 /docker/containers/abc/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/vda1 rw
559 541 0:190 /token /run/secrets/web_token ro,relatime - tmpfs tmpfs rw
560 541 0:42 /Users/me/rec/media /media/vods rw,relatime - fakeowner grpcfuse rw
561 541 0:42 /Users/me/rec/media /media/vods rw,relatime - fakeowner grpcfuse rw
562 541 0:42 /Users/me/rec/movie.mkv /media/movie.mkv rw,relatime - fakeowner grpcfuse rw
`

func TestParse(t *testing.T) {
	isDir := func(p string) bool { return !strings.HasSuffix(p, ".mkv") }
	writable := func(p string) bool { return p != "/mnt/old vods" }
	got := Parse(strings.NewReader(fixture), []string{"/recordings"}, isDir, writable)
	want := []Mount{
		{Path: "/archive", Writable: false},
		{Path: "/media/vods", Writable: true},
		{Path: "/mnt/old vods", Writable: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse =\n%+v\nwant\n%+v", got, want)
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		`/a\040b`:  "/a b",
		`/tab\011`: "/tab\t",
		`/plain`:   "/plain",
		`/bad\09`:  `/bad\09`,
		`/end\04`:  `/end\04`,
	} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}
