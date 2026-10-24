package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/vodlabs-io/recorder/internal/webtoken"
	"github.com/vodlabs-io/recorder/internal/webui"
	"github.com/vodlabs-io/recorder/web"
)

// startWebUI brings up the Web UI when [web] is enabled. A failure stops the
// daemon: a container that can't serve its only interface should restart
// with the error in its logs, not report healthy.
func (a *App) startWebUI() error {
	cfg := a.appCfg.Web
	if !cfg.Enabled {
		return nil
	}
	dir := filepath.Dir(a.appCfg.Daemon.ConfigFilePath)

	tok, err := webtoken.Resolve(cfg, dir)
	if err != nil {
		return fmt.Errorf("web UI token: %w", err)
	}
	dist := web.Dist()
	if dist == nil {
		a.log.Warn("This build has no Web UI assets; serving a placeholder page")
	}

	srv, err := webui.Start(webui.Config{
		Host: cfg.Host,
		Port: cfg.Port,
		Dir:  dir,
		ResolveToken: func() (string, error) {
			t, err := webtoken.Resolve(cfg, dir)
			return t.Value, err
		},
		AllowedOrigins: cfg.AllowedOrigins,
		TrustedProxies: cfg.TrustedProxies,
		Dist:           dist,
		Dial:           a.natsServer.InProcessConn,
		Files:          a,
		Log:            a.log.With("component", "webui"),
	})
	if err != nil {
		return fmt.Errorf("web UI: %w", err)
	}
	a.webSrv = srv

	// The token itself is never logged: logs get pasted into bug reports.
	a.log.Info("Web UI started", "addr", srv.Addr(), "token_source", tok.Describe())
	if tok.Created {
		a.log.Info("Web UI sign-in token generated; print a sign-in link with "+
			"`docker compose exec recorder vod web url` (or `vod web url`)", "token_file", tok.Path)
	}
	return nil
}
