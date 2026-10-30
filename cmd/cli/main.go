package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/app/vlcli"
	"github.com/vodlabs-io/recorder/internal/errreport"
	"github.com/vodlabs-io/recorder/internal/version"
)

func main() {
	log.SetFlags(0) // Disable timestamps in log messages
	// Reports a panic if the user turned error reports on; LoadConfig reads
	// their answer.
	defer errreport.Recover()

	app := &vlcli.App{}

	cmd := &cli.Command{
		Name:           vlcli.RootCommandName,
		Usage:          "VodLabs Recorder CLI",
		Version:        version.Version,
		Before:         app.LoadConfig,
		After:          app.Cleanup,
		Flags:    app.Flags(),
		Commands: app.Commands(),
		// No DefaultCommand: it swallows a mistyped command name and silently
		// opens the blocking status view instead of failing. Bare `vod` still
		// means `vod status`, via the root action below.
		Action: app.RootAction,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cmd.Run(ctx, os.Args); err != nil {
		log.Fatal("Error: ", err)
	}
}
