package vlcli

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/encoding"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

// mergeCommand enqueues a merge of a VOD's numbered VOD files.
func (a *App) mergeCommand() *cli.Command {
	return &cli.Command{
		Name:      "merge",
		Usage:     "Merge a VOD's files into a single vod.mp4 (queued re-encode)",
		ArgsUsage: "<session_dir>",
		Before:    a.connectDaemon,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "keep-parts",
				Usage: "Keep the original vodN.mp4 parts (renamed to .bak) instead of deleting them after a verified merge",
			},
		},
		Action: a.runMerge,
	}
}

// queueCommand manages the encode queue.
func (a *App) queueCommand() *cli.Command {
	return &cli.Command{
		Name:   "queue",
		Usage:  "Manage the encode (merge/re-encode) job queue",
		Before: a.connectDaemon,
		Commands: []*cli.Command{
			{
				Name:   "list",
				Usage:  "List encode jobs",
				Flags:  []cli.Flag{jsonFlag()},
				Action: a.runQueueList,
			},
			{
				Name:      "cancel",
				Usage:     "Cancel a queued or running job",
				ArgsUsage: "<job_id>",
				Action:    a.queueJobOp("encode.cancel", "cancelled"),
			},
			{
				Name:      "rm",
				Usage:     "Remove a finished/failed/cancelled job from the list",
				ArgsUsage: "<job_id>",
				Action:    a.queueJobOp("encode.remove", "removed from the queue"),
			},
			{
				Name:      "retry",
				Usage:     "Re-queue a failed or cancelled job",
				ArgsUsage: "<job_id>",
				Action:    a.queueJobOp("encode.retry", "re-queued"),
			},
			{
				Name:   "pause",
				Usage:  "Pause the queue (the running job finishes; no new jobs start)",
				Action: a.queuePauseOp("encode.pause"),
			},
			{
				Name:   "resume",
				Usage:  "Resume a paused queue",
				Action: a.queuePauseOp("encode.resume"),
			},
		},
	}
}

type encodeEnqueueReq struct {
	Dir         string `json:"dir"`
	DeleteParts bool   `json:"delete_parts"`
}

type encodeJobReq struct {
	ID int64 `json:"id"`
}

func (a *App) runMerge(ctx context.Context, cmd *cli.Command) error {
	dir := strings.TrimSpace(cmd.Args().First())
	if dir == "" {
		return fmt.Errorf("usage: vod merge <session_dir>")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	job, err := vlapi.Request[encodeEnqueueReq, encoding.Job](a.nc, "encode.enqueue", encodeEnqueueReq{
		Dir:         abs,
		DeleteParts: !cmd.Bool("keep-parts"),
	})
	if err != nil {
		return err
	}
	fmt.Printf("Queued merge job %d: %d parts in %s\n", job.ID, len(job.Inputs), job.Dir)
	fmt.Println("Watch it with: vod queue list")
	return nil
}

func (a *App) runQueueList(ctx context.Context, cmd *cli.Command) error {
	q, err := vlapi.Request[any, encoding.Queue](a.nc, "encode.list", nil)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return emitJSON(q)
	}
	if q.Paused {
		fmt.Println("Queue is PAUSED (resume with: vod queue resume)")
	}
	if len(q.Jobs) == 0 {
		fmt.Println("No encode jobs.")
		return nil
	}

	w := newTable()
	fmt.Fprintln(w, "ID\tSTATUS\tSTAGE\tSESSION\tPARTS\tCREATED\tNOTE")
	for _, j := range q.Jobs {
		session := j.Dir
		if j.Platform != "" && j.Username != "" {
			session = j.Platform + "/" + j.Username + " (" + filepath.Base(j.Dir) + ")"
		}
		note := j.Error
		if note == "" {
			note = j.Warning
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\t%s\n",
			j.ID, j.Status, j.Stage, session, len(j.Inputs),
			time.Unix(j.CreatedAt, 0).Format("2006-01-02 15:04"), note)
	}
	return w.Flush()
}

// queueJobOp returns an action that sends {id} to the given encode subject.
// done is the past-tense verb for the confirmation (e.g. "cancelled").
func (a *App) queueJobOp(subject, done string) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		idStr := strings.TrimSpace(cmd.Args().First())
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("usage: %s queue %s <job_id>", RootCommandName, cmd.Name)
		}
		if _, err := vlapi.Request[encodeJobReq, string](a.nc, subject, encodeJobReq{ID: id}); err != nil {
			return err
		}
		fmt.Printf("Job %d %s.\n", id, done)
		return nil
	}
}

// queuePauseOp returns an action that sends an empty request to the given
// pause/resume subject.
func (a *App) queuePauseOp(subject string) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if _, err := vlapi.Request[any, string](a.nc, subject, nil); err != nil {
			return err
		}
		fmt.Printf("Queue %sd.\n", cmd.Name)
		return nil
	}
}
