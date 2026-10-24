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

func TestEnvConfigLists(t *testing.T) {
	t.Setenv("VLREC_WEB_TRUSTED_PROXIES", " 10.0.0.0/8, ,172.16.0.0/12")
	got, ok := EnvConfig()["web.trusted_proxies"].([]string)
	if !ok || len(got) != 2 || got[0] != "10.0.0.0/8" || got[1] != "172.16.0.0/12" {
		t.Errorf("web.trusted_proxies = %#v", EnvConfig()["web.trusted_proxies"])
	}
}
