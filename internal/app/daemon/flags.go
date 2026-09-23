package daemon

import (
	"fmt"
	"os"

	flag "github.com/spf13/pflag"

	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func parseFlags() (*flag.FlagSet, error) {
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-v" {
			fmt.Println(version.Version)
			os.Exit(0)
		}
	}
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Println(fs.FlagUsages())
		os.Exit(0)
	}

	fs.String("logger.mode", "", "Log output mode: off, file, console, both")
	fs.String("daemon.host", "", "NATS server host")
	fs.Uint16("daemon.port", 0, "NATS server port")
	fs.StringP("session.download_dir", "d", "", "Folder recordings are downloaded to, organized into sub-folders")
	fs.StringP("session.variant", "q", "", "Quality variant to record (e.g. best, worst, 720p, etc)")
	fs.String("processing.db_path", "", "Path to the processing queue database (processing.db)")
	fs.StringP("config", "c", vlconfig.DefaultConfigFilePath(), "config file path")
	fs.StringP("daemon.streamers_file_path", "s", vlconfig.DefaultStreamersFilePath(), "streamers file path")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}

	return fs, nil
}
