package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/vodlabs-io/recorder/internal/app/daemon"
)

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
		syscall.SIGHUP)
	defer stop()

	d, err := daemon.New(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize daemon: %v", err)
	}

	if err := d.Run(ctx); err != nil {
		log.Fatalf("Daemon error: %v", err)
	}
}
