package daemon

import (
	"context"
	"fmt"

	"github.com/vodlabs-io/recorder/internal/webui"
)

// VODFiles implements webui.SessionLister: a finished VOD's files in play
// order.
func (a *App) VODFiles(ctx context.Context, dir string) ([]string, error) {
	if !a.vodReady.Load() {
		return nil, webui.ErrNotReady
	}
	session, err := a.readySession(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrNotFound, err)
	}
	names := make([]string, 0, len(session.Files))
	for _, f := range session.Files {
		names = append(names, f.Name)
	}
	return names, nil
}

// Compile-time check that App plays whole recordings in the Web UI.
var _ webui.SessionLister = (*App)(nil)
