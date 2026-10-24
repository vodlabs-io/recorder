package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	flag "github.com/spf13/pflag"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func loadConfig(k *koanf.Koanf, fs *flag.FlagSet) (vlconfig.AppConfig, error) {
	k.Load(confmap.Provider(vlconfig.Defaults(), "."), nil)

	configFilePath, err := fs.GetString("config")
	if err != nil {
		return vlconfig.AppConfig{}, fmt.Errorf("failed to get config file path: %w", err)
	}

	// Check if daemon config file exists, create it if not
	if err := vlconfig.EnsureConfigFileExists(configFilePath); err != nil {
		return vlconfig.AppConfig{}, fmt.Errorf("failed to ensure config file exists: %w", err)
	}

	// Load config from toml file, overriding defaults
	if err := k.Load(file.Provider(configFilePath), toml.Parser()); err != nil {
		return vlconfig.AppConfig{}, fmt.Errorf("failed to load config file: %w", err)
	}

	// Environment overrides the file, mainly for container deployments.
	if err := k.Load(confmap.Provider(vlconfig.EnvConfig(), "."), nil); err != nil {
		return vlconfig.AppConfig{}, fmt.Errorf("failed to load environment config: %w", err)
	}

	// Combine config with command-line flags, which take highest precedence
	if err := k.Load(posflag.Provider(fs, ".", k), nil); err != nil {
		return vlconfig.AppConfig{}, fmt.Errorf("failed to parse command-line flags: %w", err)
	}

	// Set config file path at the proper location so it gets loaded into the struct
	k.Set("daemon.config_file_path", configFilePath)

	var cfg vlconfig.AppConfig
	if err := k.Unmarshal("", &cfg); err != nil {
		return vlconfig.AppConfig{}, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	cfg.ApplyLoggerDefaults()

	// Default the download folder to the user's Downloads folder
	if cfg.Session.DownloadDir == "" {
		cfg.Session.DownloadDir = vlconfig.DefaultDownloadDir()
	}

	// Convert the download folder to an absolute path
	absolutePath, err := filepath.Abs(cfg.Session.DownloadDir)
	if err != nil {
		return vlconfig.AppConfig{}, fmt.Errorf(
			"failed to resolve absolute path for download folder: %w", err)
	}
	cfg.Session.DownloadDir = absolutePath

	return cfg, nil
}
