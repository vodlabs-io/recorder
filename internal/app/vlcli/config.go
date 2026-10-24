package vlcli

import (
	"context"
	"fmt"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/cliflagv3"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func (a *App) LoadConfig(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	cfgFile := cmd.String("config")
	var k = koanf.New(".")

	k.Load(confmap.Provider(vlconfig.Default, "."), nil)

	if err := vlconfig.EnsureConfigFileExists(cfgFile); err != nil {
		return nil, fmt.Errorf("failed to ensure config file exists: %w", err)
	}

	// The file overrides the defaults.
	if err := k.Load(file.Provider(cfgFile), toml.Parser()); err != nil {
		return nil, fmt.Errorf("failed to load config file: %w", err)
	}

	// The environment overrides the file, as it does for the daemon.
	if err := k.Load(confmap.Provider(vlconfig.EnvConfig(), "."), nil); err != nil {
		return nil, fmt.Errorf("failed to load environment config: %w", err)
	}

	// cliflagv3.Provider prefixes keys with the command lineage (e.g. "vod.daemon.port").
	// Load into a separate instance and Cut the root command prefix before merging,
	// so flag keys align with the flat keys used by the TOML file and defaults.
	kflags := koanf.New(".")
	if err := kflags.Load(cliflagv3.Provider(cmd, "."), nil); err != nil {
		return nil, fmt.Errorf("failed to load config from CLI flags: %w", err)
	}
	if err := k.Merge(kflags.Cut(RootCommandName)); err != nil {
		return nil, fmt.Errorf("failed to merge CLI flags into config: %w", err)
	}

	var cfg vlconfig.DaemonConfig
	if err := k.Unmarshal("daemon", &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	a.cfg = cfg
	a.natsURL = fmt.Sprintf("nats://%s:%d", a.cfg.Host, a.cfg.Port)

	return ctx, nil
}
