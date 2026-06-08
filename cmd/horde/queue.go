package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jorge-barreto/horde/internal/store"
	"github.com/urfave/cli/v3"
)

func queueCmd() *cli.Command {
	return &cli.Command{
		Name:  "queue",
		Usage: "Inspect and steer the server-side launch queue (aws-ecs)",
		Description: `The queue holds launches submitted with 'horde launch --enqueue' that are
waiting for a concurrency slot (and spend-rate headroom). The drain starts
them mechanically: highest priority first, then oldest first. Reprioritize or
cancel waiting runs to curate what runs next. See 'horde docs queue'.`,
		Commands: []*cli.Command{
			queueListCmd(),
			queuePrioritizeCmd(),
			queueCancelCmd(),
		},
	}
}

func queueListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List queued runs in drain order (priority desc, oldest first)",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			_, st, _, _, _, hordeCfg, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			repo, err := resolveCanonicalRepo(hordeCfg, newResolver(cmd))
			if err != nil {
				return err
			}
			runs, err := st.ListRuns(ctx, store.RunFilter{Repo: repo, Statuses: []store.Status{store.StatusQueued}})
			if err != nil {
				return fmt.Errorf("listing queued runs: %w", err)
			}
			sortByDrainOrder(runs)
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, queueListV1(runs))
			}
			if len(runs) == 0 {
				fmt.Println("Queue is empty.")
				return nil
			}
			for _, r := range runs {
				fmt.Printf("%s  %-8s  %s  %s\n", r.ID, r.Priority, r.Ticket, r.Workflow)
			}
			return nil
		},
	}
}

func queuePrioritizeCmd() *cli.Command {
	return &cli.Command{
		Name:      "prioritize",
		Usage:     "Change the priority of a queued run",
		ArgsUsage: "<run-id> --priority <level>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "priority", Usage: "lowest|low|med|high|highest"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
			if cmd.String("priority") == "" {
				return fmt.Errorf("--priority is required (lowest|low|med|high|highest)")
			}
			p, err := store.ParsePriority(cmd.String("priority"))
			if err != nil {
				return err
			}
			_, st, _, _, _, _, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			run, err := st.GetRun(ctx, id)
			if err != nil {
				return err
			}
			if run.Status != store.StatusQueued {
				return fmt.Errorf("run %s is %s, not queued — only queued runs can be reprioritized", id, run.Status)
			}
			if err := st.UpdateRun(ctx, id, &store.RunUpdate{Priority: &p}); err != nil {
				return fmt.Errorf("updating priority: %w", err)
			}
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, queuePrioritizeV1(id, string(p)))
			}
			fmt.Fprintf(os.Stdout, "%s priority set to %s\n", id, p)
			return nil
		},
	}
}

func queueCancelCmd() *cli.Command {
	return &cli.Command{
		Name:      "cancel",
		Usage:     "Cancel a queued run before it ever runs",
		ArgsUsage: "<run-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
			_, st, _, _, _, _, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			run, err := st.GetRun(ctx, id)
			if err != nil {
				return err
			}
			if run.Status != store.StatusQueued {
				return fmt.Errorf("run %s is %s, not queued — only queued runs can be cancelled (use 'horde kill' for a running task)", id, run.Status)
			}
			cancelled := store.StatusCancelled
			if err := st.UpdateRun(ctx, id, &store.RunUpdate{Status: &cancelled}); err != nil {
				return fmt.Errorf("cancelling run: %w", err)
			}
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, queueCancelV1(id))
			}
			fmt.Fprintf(os.Stdout, "%s cancelled\n", id)
			return nil
		},
	}
}
