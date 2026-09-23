package vlcli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/flvfix"
)

func (a *App) flvFixCommand() *cli.Command {
	return &cli.Command{
		Name:  "flv-fix",
		Usage: "Repair raw FLV recordings (.ts) into clean, remux-ready files",
		Description: "Sanitizes raw FLV live captures: drops garbage and duplicate\n" +
			"(replayed) data, repairs timestamps, and splits at codec configuration\n" +
			"changes so every output converts to a clean, seekable MP4 with\n" +
			"ffmpeg -c copy. One output file is produced per distinct codec\n" +
			"configuration (<stem>.ts, <stem>_2.ts, ...).",
		ArgsUsage: "<recording.ts> [more.ts ...]",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "Output stem (single input only; default: input path + \"_fixed\")",
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "Log each repair event (restarts, replays, gaps, desyncs)",
			},
		},
		Action: a.flvFixAction,
	}
}

func (a *App) flvFixAction(ctx context.Context, cmd *cli.Command) error {
	inputs := cmd.Args().Slice()
	if len(inputs) == 0 {
		return fmt.Errorf("no input files given")
	}
	outStem := cmd.String("output")
	if outStem != "" && len(inputs) > 1 {
		return fmt.Errorf("--output is only valid with a single input file")
	}

	level := slog.LevelWarn
	if cmd.Bool("verbose") {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	for _, in := range inputs {
		stem := outStem
		if stem == "" {
			stem = strings.TrimSuffix(in, ".ts") + "_fixed"
		}
		outputs, stats, err := flvfix.RepairFile(in, stem, logger)
		if err != nil {
			return fmt.Errorf("repair %s: %w", in, err)
		}
		fmt.Printf("%s → %d output(s)\n", in, len(outputs))
		for _, out := range outputs {
			fmt.Printf("  %s\n", out)
		}
		fmt.Printf("  %s\n", summarizeStats(stats))
	}
	return nil
}

// summarizeStats renders the non-zero sanitizer counters on one line.
func summarizeStats(st flvfix.Stats) string {
	attrs := st.Attrs()
	var parts []string
	for i := 0; i+1 < len(attrs); i += 2 {
		if v, ok := attrs[i+1].(int64); ok && v != 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", attrs[i], v))
		}
	}
	if len(parts) == 0 {
		return "clean (no corrections needed)"
	}
	return strings.Join(parts, " ")
}
