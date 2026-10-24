package vlcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/webtoken"
)

func (a *App) webCommand() *cli.Command {
	return &cli.Command{
		Name:  "web",
		Usage: "Print the Web UI's sign-in link or token",
		Commands: []*cli.Command{
			{
				Name:   "url",
				Usage:  "Print a sign-in link for the Web UI",
				Action: a.runWebURL,
			},
			{
				Name:  "token",
				Usage: "Print the Web UI's sign-in token, or replace it",
				Description: "--rotate replaces the generated token, which signs every browser " +
					"out. A token pinned by web.token, VLREC_WEB_TOKEN or web.token_file " +
					"is changed where it is set instead.",
				Flags: []cli.Flag{&cli.BoolFlag{
					Name:  "rotate",
					Usage: "Replace the token, signing every browser out",
				}},
				Action: a.runWebToken,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return rejectExtraArgs(cmd, 0, "web")
		},
	}
}

func (a *App) runWebURL(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "web url"); err != nil {
		return err
	}
	tok, err := a.webToken(cmd)
	if err != nil {
		return err
	}
	a.warnWebDisabled()
	fmt.Println(webLocalURL(a.web) + "signin#token=" + tok.Value)
	return nil
}

func (a *App) runWebToken(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "web token"); err != nil {
		return err
	}
	if !cmd.Bool("rotate") {
		tok, err := a.webToken(cmd)
		if err != nil {
			return err
		}
		a.warnWebDisabled()
		fmt.Println(tok.Value)
		return nil
	}
	dir := filepath.Dir(cmd.Root().String("config"))
	if err := refuseForeignOwner(dir); err != nil {
		return err
	}
	tok, err := webtoken.Rotate(a.web, dir)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Token replaced; every browser is signed out within a few seconds.")
	fmt.Println(tok.Value)
	return nil
}

// webToken resolves the token, generating it when none exists yet.
func (a *App) webToken(cmd *cli.Command) (webtoken.Token, error) {
	dir := filepath.Dir(cmd.Root().String("config"))
	if err := refuseForeignOwner(dir); err != nil {
		return webtoken.Token{}, err
	}
	return webtoken.Resolve(a.web, dir)
}

func (a *App) warnWebDisabled() {
	if !a.web.Enabled {
		fmt.Fprintln(os.Stderr, "Note: the Web UI is off (web.enabled = false), so this link won't open yet.")
	}
}
