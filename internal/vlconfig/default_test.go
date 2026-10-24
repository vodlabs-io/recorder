package vlconfig

import "testing"

func TestDefaults(t *testing.T) {
	for _, tc := range []struct {
		container string
		mode      string
		autoStart bool
		download  string
	}{
		{"", "file", true, ""},
		{"1", "console", false, ContainerDownloadDir},
	} {
		t.Setenv("VLREC_CONTAINER", tc.container)
		d := Defaults()
		if got := d["logger"].(map[string]any)["mode"]; got != tc.mode {
			t.Errorf("container=%q: logger.mode = %v, want %s", tc.container, got, tc.mode)
		}
		if got := d["daemon"].(map[string]any)["auto_start"]; got != tc.autoStart {
			t.Errorf("container=%q: daemon.auto_start = %v, want %v", tc.container, got, tc.autoStart)
		}
		if got := DefaultDownloadDir(); tc.download != "" && got != tc.download {
			t.Errorf("container=%q: DefaultDownloadDir() = %q, want %q", tc.container, got, tc.download)
		} else if tc.download == "" && got == ContainerDownloadDir {
			t.Errorf("host install defaults to %s", ContainerDownloadDir)
		}
	}
}
