package vlcli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func (a *App) configCommand() *cli.Command {
	return &cli.Command{
		Name:    "config",
		Aliases: []string{"cfg"},
		Usage:   "View or edit the main config file",
		Before:  a.connectDaemon,
		Commands: []*cli.Command{
			a.configPathCommand(),
			a.configEditCommand(),
			a.configErrorReportsCommand(),
		},
	}
}

func (a *App) configPathCommand() *cli.Command {
	return &cli.Command{
		Name:   "path",
		Usage:  "Print the absolute path to the config file",
		Action: a.runConfigPath,
	}
}

func (a *App) configEditCommand() *cli.Command {
	return &cli.Command{
		Name:   "edit",
		Usage:  "Open the config file in a text editor",
		Action: a.runConfigEdit,
	}
}

func (a *App) streamerConfigCommand() *cli.Command {
	return &cli.Command{
		Name:    "config",
		Aliases: []string{"cfg"},
		Usage:   "View or edit the streamers config file",
		Commands: []*cli.Command{
			a.streamerConfigPathCommand(),
			a.streamerConfigEditCommand(),
		},
	}
}

func (a *App) streamerConfigPathCommand() *cli.Command {
	return &cli.Command{
		Name:   "path",
		Usage:  "Print the absolute path to the streamers file",
		Action: a.runStreamerConfigPath,
	}
}

func (a *App) streamerConfigEditCommand() *cli.Command {
	return &cli.Command{
		Name:   "edit",
		Usage:  "Open the streamers file in a text editor",
		Action: a.runStreamerConfigEdit,
	}
}

func (a *App) runConfigPath(ctx context.Context, cmd *cli.Command) error {
	info, err := vlapi.Request[any, vlapi.DaemonInfo](a.nc, "info", nil)
	if err != nil {
		return err
	}
	fmt.Println(info.ConfigFilePath)
	return nil
}

func (a *App) runConfigEdit(ctx context.Context, cmd *cli.Command) error {
	info, err := vlapi.Request[any, vlapi.DaemonInfo](a.nc, "info", nil)
	if err != nil {
		return err
	}
	if err := editConfigFile(info.ConfigFilePath, vlconfig.EnsureConfigFileExists); err != nil {
		return err
	}
	fmt.Println("Note: changes to this file won't take effect until the daemon is restarted (vod restart).")
	return nil
}

func (a *App) runStreamerConfigPath(ctx context.Context, cmd *cli.Command) error {
	info, err := vlapi.Request[any, vlapi.DaemonInfo](a.nc, "info", nil)
	if err != nil {
		return err
	}
	fmt.Println(info.StreamersFilePath)
	return nil
}

func (a *App) runStreamerConfigEdit(ctx context.Context, cmd *cli.Command) error {
	info, err := vlapi.Request[any, vlapi.DaemonInfo](a.nc, "info", nil)
	if err != nil {
		return err
	}
	return editConfigFile(info.StreamersFilePath, vlconfig.EnsureStreamersFileExists)
}

func editConfigFile(path string, ensure func(string) error) error {
	if err := ensure(path); err != nil {
		return err
	}
	return editFile(path)
}

func editFile(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor != "" {
		c := exec.Command(editor, path)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}

	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", path)
	case "windows":
		c = exec.Command("cmd", "/c", "start", "", path)
	default:
		c = exec.Command("xdg-open", path)
	}
	return c.Run()
}
