package vlcli

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

const RootCommandName = "vod"

type App struct {
	cfg     vlconfig.DaemonConfig
	nc      *nats.Conn
	kv      jetstream.KeyValue
	closing atomic.Bool
	natsURL string
}

func (a *App) Commands() []*cli.Command {
	return []*cli.Command{
		a.statusCommand(),
		a.streamerCommand(),
		a.vodsCommand(),
		a.platformsCommand(),
		a.recordCommand(),
		a.mergeCommand(),
		a.queueCommand(),
		a.configCommand(),
		a.cloudCommand(),
		a.loginCommand(),
		a.infoCommand(),
		a.healthCommand(),
		a.flvFixCommand(),
		a.updateCommand(),
		a.restartCommand(),
		a.quitCommand(),
	}
}

func (a *App) Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:        "config",
			Aliases:     []string{"c"},
			Usage:       "Path to the configuration file",
			DefaultText: vlconfig.DefaultConfigFilePath(),
			Value:       vlconfig.DefaultConfigFilePath(),
		},
	}
}

// RootAction runs `vod` with no subcommand, which means `vod status`. An
// unrecognised argument is an error rather than a silent fallback into the
// blocking status view.
func (a *App) RootAction(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, ""); err != nil {
		return err
	}
	ctx, err := a.connectDaemon(ctx, cmd)
	if err != nil {
		return err
	}
	return a.runStatus(ctx, cmd)
}

// rejectExtraArgs errors on the first positional argument past the `taken` a
// command actually consumes. Declared Arguments are bound before this runs, so
// what is left is a word the command has no use for: for a command with
// subcommands that is a mistyped subcommand name, which would otherwise fall
// through to the Action; for a leaf command it is a stray argument, which would
// otherwise be ignored in silence. path names the command for the help hint
// ("" for the root).
func rejectExtraArgs(cmd *cli.Command, taken int, path string) error {
	args := cmd.Args().Slice()
	if len(args) <= taken {
		return nil
	}
	help := RootCommandName
	if path != "" {
		help += " " + path
	}
	if hasSubcommands(cmd) {
		return fmt.Errorf("unknown command %q. Run '%s --help' to see the available commands",
			args[taken], help)
	}
	return fmt.Errorf("unexpected argument %q. Run '%s --help' to see what this command takes",
		args[taken], help)
}

func (a *App) Cleanup(ctx context.Context, cmd *cli.Command) error {
	fmt.Print(cr())

	if a.nc != nil {
		a.closing.Store(true)
		a.nc.Close()
	}

	return nil
}

// cr returns a carriage return when stdout is a terminal, else "". It resets
// the column after a live-rendered frame, so a ctrl+C mid-render leaves no
// stray blank line. On a pipe it would be captured as data and survive command
// substitution, e.g. `$(vod config path)`.
func cr() string {
	if isTerminal(os.Stdout) {
		return "\r"
	}
	return ""
}

// hasSubcommands reports whether a command has subcommands of its own. The
// auto-generated "help" does not count: every command has it, so counting it
// would make every command look like a parent.
func hasSubcommands(cmd *cli.Command) bool {
	for _, c := range cmd.Commands {
		if c.Name != "help" {
			return true
		}
	}
	return false
}

// isTerminal reports whether f is attached to a character device, i.e. a
// terminal rather than a pipe or a file.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
