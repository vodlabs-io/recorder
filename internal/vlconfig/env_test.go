package vlconfig

import "testing"

func TestEnvConfig(t *testing.T) {
	t.Setenv("VLREC_DAEMON_PORT", " 14222 ")
	t.Setenv("VLREC_LOG_MODE", "")
	got := EnvConfig()
	if got["daemon.port"] != "14222" {
		t.Errorf("daemon.port = %v, want 14222", got["daemon.port"])
	}
	if _, ok := got["logger.mode"]; ok {
		t.Error("an empty variable became a config value")
	}
}
