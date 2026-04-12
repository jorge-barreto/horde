package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/urfave/cli/v3"
)

func main() {
	cmd := &cli.Command{
		Name:  "horde",
		Usage: "Cloud launcher for orc workflows",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "provider",
				Value: "docker",
				Usage: "Override provider selection (docker or aws-ecs)",
			},
		},
		Commands: []*cli.Command{
			launchCmd(),
			statusCmd(),
			logsCmd(),
			killCmd(),
			resultsCmd(),
			listCmd(),
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func launchCmd() *cli.Command {
	return &cli.Command{
		Name:      "launch",
		Usage:     "Launch an orc workflow",
		ArgsUsage: "<ticket>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "branch",
				Usage: "Git branch to use",
			},
			&cli.StringFlag{
				Name:  "workflow",
				Usage: "Workflow to run",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Timeout for the run",
				Value: 60 * time.Minute,
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "Force launch even if already running",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func statusCmd() *cli.Command {
	return &cli.Command{
		Name:      "status",
		Usage:     "Show status of a run",
		ArgsUsage: "<run-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func logsCmd() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "Show logs for a run",
		ArgsUsage: "<run-id>",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "follow",
				Usage: "Follow log output",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func killCmd() *cli.Command {
	return &cli.Command{
		Name:      "kill",
		Usage:     "Kill a running run",
		ArgsUsage: "<run-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func resultsCmd() *cli.Command {
	return &cli.Command{
		Name:      "results",
		Usage:     "Show results of a run",
		ArgsUsage: "<run-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func listCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List runs",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "all",
				Usage: "List all runs",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}
