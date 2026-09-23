package vlcli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// Platform-login CLI commands, for recording login-restricted streams on hosts
// without the desktop app (e.g. a headless/docker daemon).
//
// PRIVACY: the cookies pasted here go ONLY to the local daemon over local NATS
// and are stored encrypted on this device. They are never sent to the vodlabs
// cloud.

type accountSetReq struct {
	Platform     string `json:"platform"`
	CookieHeader string `json:"cookie_header"`
}

type accountRemoveReq struct {
	Platform string `json:"platform"`
}

// accountStatus mirrors the non-secret platformauth.Status returned by the
// platformlogin.* endpoints (no cookie values).
type accountStatus struct {
	Platform    string `json:"platform"`
	Account     string `json:"account"`
	CookieCount int    `json:"cookie_count"`
	Healthy     bool   `json:"healthy"`
	ExpiresHint int64  `json:"expires_hint"`
	CanAuth     bool   `json:"can_authenticate"`
}

func (a *App) loginCommand() *cli.Command {
	return &cli.Command{
		Name:  "login",
		Usage: "Sign in to a streaming platform to record login-restricted streams",
		Description: "Paste a Cookie header copied from a signed-in browser tab, or point " +
			"--cookies-file at a cookies.txt export. The login is stored encrypted on " +
			"this device only and is never uploaded to vodlabs.",
		Before:    a.connectDaemon,
		ArgsUsage: "<platform>",
		Arguments: []cli.Argument{&cli.StringArg{Name: "platform"}},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "cookies-file",
				Usage: "Read the login from a Netscape cookies.txt export instead of a paste",
			},
		},
		Action: a.runLogin,
		Commands: []*cli.Command{
			{
				Name:   "list",
				Usage:  "List stored platform logins",
				Flags:  []cli.Flag{jsonFlag()},
				Before: a.connectDaemon,
				Action: a.runLoginList,
			},
			{
				Name:      "remove",
				Usage:     "Remove a stored platform login (sign out)",
				ArgsUsage: "<platform>",
				Before:    a.connectDaemon,
				Arguments: []cli.Argument{&cli.StringArg{Name: "platform"}},
				Action:    a.runLoginRemove,
			},
		},
	}
}

func (a *App) runLogin(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 1, "login"); err != nil {
		return err
	}
	platform := strings.ToLower(strings.TrimSpace(cmd.StringArg("platform")))
	if platform == "" {
		return fmt.Errorf("usage: %s login <platform>   (or: %s login list | remove <platform>)",
			RootCommandName, RootCommandName)
	}
	// Checked before the prompt: asking someone to paste a cookie header for a
	// platform that cannot use one wastes the paste and reveals nothing.
	if err := a.checkLoginPlatform(platform); err != nil {
		return err
	}
	var (
		header string
		err    error
	)
	if path := strings.TrimSpace(cmd.String("cookies-file")); path != "" {
		header, err = a.cookieHeaderFromFile(platform, path)
	} else {
		header, err = promptCookieHeader(platform)
	}
	if err != nil {
		return err
	}

	status, err := vlapi.Request[accountSetReq, accountStatus](
		a.nc, "platformlogin.set", accountSetReq{Platform: platform, CookieHeader: header})
	if err != nil {
		return err
	}
	fmt.Printf("Signed in to %s — %d cookies stored on this device only (never uploaded).\n",
		status.Platform, status.CookieCount)
	return nil
}

func promptCookieHeader(platform string) (string, error) {
	fmt.Printf("Paste the Cookie header from a signed-in %s browser tab, then press Enter:\n", platform)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("failed to read cookies: %w", err)
	}
	header := strings.TrimSpace(line)
	if header == "" {
		return "", fmt.Errorf("no cookies provided")
	}
	return header, nil
}

// cookieHeaderFromFile reads a Netscape cookies.txt export and keeps only the
// cookies belonging to this platform. An export is normally the browser's whole
// jar, and no other site's cookies have any business reaching the daemon.
func (a *App) cookieHeaderFromFile(platform, path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", path, err)
	}
	cookies := platformauth.ParseNetscapeCookies(string(raw))
	if len(cookies) == 0 {
		return "", fmt.Errorf("no cookies found in %s — expected a Netscape cookies.txt export", path)
	}

	flow := a.loginFlowFor(platform)
	if flow == nil {
		// Without the descriptor there is nothing to filter against, and
		// sending an entire browser jar is not a reasonable default.
		return "", fmt.Errorf("cannot tell which cookies belong to %s; paste the Cookie header instead", platform)
	}
	var kept []platformauth.Cookie
	for _, c := range cookies {
		if flow.AllowsHost(c.Domain) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return "", fmt.Errorf("%s holds no %s cookies — sign in to %s in your browser, then export again",
			path, platform, platform)
	}
	fmt.Printf("Read %d %s cookies from %s (%d other cookies ignored).\n",
		len(kept), platform, path, len(cookies)-len(kept))
	return platformauth.CookieHeader(kept), nil
}

// checkLoginPlatform rejects a platform the daemon does not know, or knows but
// cannot sign in to, naming the ones that can.
func (a *App) checkLoginPlatform(platform string) error {
	res, err := vlapi.Request[any, []vlconfig.Platform](a.nc, "platforms", nil)
	if err != nil {
		// The list is a convenience; a daemon that cannot answer should not
		// block a login that might still work.
		return nil
	}
	var supported []string
	for _, p := range *res {
		if p.Login != nil {
			supported = append(supported, p.Name)
		}
		if p.Name == platform {
			if p.Login == nil {
				return fmt.Errorf("%s streams don't need a sign-in, so there is no login to store", p.Name)
			}
			return nil
		}
	}
	return fmt.Errorf("unknown platform %q. Sign-in is supported for: %s\nRun '%s platforms' for the full list",
		platform, strings.Join(supported, ", "), RootCommandName)
}

// loginFlowFor returns the platform's public login descriptor, or nil when the
// daemon cannot name it.
func (a *App) loginFlowFor(platform string) interface{ AllowsHost(string) bool } {
	res, err := vlapi.Request[any, []vlconfig.Platform](a.nc, "platforms", nil)
	if err != nil || res == nil {
		return nil
	}
	for _, p := range *res {
		if p.Name == platform && p.Login != nil {
			return p.Login
		}
	}
	return nil
}

func (a *App) runLoginList(ctx context.Context, cmd *cli.Command) error {
	list, err := vlapi.Request[any, []accountStatus](a.nc, "platformlogin.list", nil)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		if list == nil {
			return emitJSON([]accountStatus{})
		}
		return emitJSON(*list)
	}
	if list == nil || len(*list) == 0 {
		fmt.Println("No platform logins stored.")
		return nil
	}
	w := newTable()
	fmt.Fprintln(w, "PLATFORM\tACCOUNT\tSTATUS\tCOOKIES")
	for _, s := range *list {
		account := s.Account
		if account == "" {
			account = "-"
		}
		health := "active"
		if !s.Healthy {
			health = "expired — sign in again"
		}
		// A jar that cannot satisfy the platform's signing scheme is stored and
		// healthy and still unlocks nothing, so it must not read as "active".
		if s.Healthy && !s.CanAuth {
			health = "active — cannot authenticate, sign in again"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", s.Platform, account, health, s.CookieCount)
	}
	return w.Flush()
}

func (a *App) runLoginRemove(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 1, "login remove"); err != nil {
		return err
	}
	platform := strings.ToLower(strings.TrimSpace(cmd.StringArg("platform")))
	if platform == "" {
		return fmt.Errorf("usage: %s login remove <platform>", RootCommandName)
	}
	if _, err := vlapi.Request[accountRemoveReq, bool](
		a.nc, "platformlogin.remove", accountRemoveReq{Platform: platform}); err != nil {
		return err
	}
	fmt.Printf("Signed out of %s (login removed from this device).\n", platform)
	return nil
}
