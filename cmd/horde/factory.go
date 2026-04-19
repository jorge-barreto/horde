package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/jorge-barreto/horde/internal/awscfg"
	"github.com/jorge-barreto/horde/internal/bootstrap"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/jorge-barreto/horde/internal/provider"
	"github.com/jorge-barreto/horde/internal/store"
)

// resolveSSMPath returns the SSM parameter path holding the runtime config
// for the current project. Order of precedence:
//
//  1. If HORDE_SSM_PATH is set, use it verbatim (escape hatch for custom
//     deployments or projects sharing config across repos).
//  2. If the current directory has a git remote, derive the project slug
//     the same way `horde bootstrap init` and `horde push` do and return
//     /horde/<slug>/config. This matches the path the bootstrap stack
//     actually writes to.
//  3. Fall back to the legacy global config.DefaultSSMPath (/horde/config)
//     so pre-slug deployments keep working.
func resolveSSMPath() string {
	if p := os.Getenv("HORDE_SSM_PATH"); p != "" {
		return p
	}
	cwd, err := os.Getwd()
	if err != nil {
		return config.DefaultSSMPath
	}
	repo, err := config.RepoURL(cwd)
	if err != nil {
		return config.DefaultSSMPath
	}
	slug, err := bootstrap.Slug(repo)
	if err != nil {
		return config.DefaultSSMPath
	}
	return "/horde/" + slug + "/config"
}

type factoryDeps struct {
	loadAWSConfig func(ctx context.Context, profile string) (aws.Config, error)
	newSSMClient  func(cfg aws.Config) config.SSMClient
	openStore     func(providerName string) (store.Store, func(), error)
}

func defaultFactoryDeps() factoryDeps {
	return factoryDeps{
		loadAWSConfig: awscfg.Load,
		newSSMClient:  func(cfg aws.Config) config.SSMClient { return ssm.NewFromConfig(cfg) },
		openStore:     openStore,
	}
}

// openStore opens the local SQLite store used by the docker provider.
// The aws-ecs provider gets its store directly via NewDynamoStore in
// initProviderAndStoreWith — this helper is the SQLite-only path.
func openStore(_ string) (store.Store, func(), error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("getting home directory: %w", err)
	}
	dbPath := filepath.Join(homeDir, ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening store: %w", err)
	}
	return st, func() { st.Close() }, nil
}

// newProviderWith creates just the provider, without opening a store.
func newProviderWith(ctx context.Context, name, profile string, deps factoryDeps) (provider.Provider, error) {
	switch name {
	case "docker":
		return provider.NewDockerProvider(), nil
	case "aws-ecs":
		awsCfg, err := deps.loadAWSConfig(ctx, profile)
		if err != nil {
			return nil, fmt.Errorf("initializing aws-ecs provider: %w", err)
		}
		ssmClient := deps.newSSMClient(awsCfg)
		hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, resolveSSMPath())
		if err != nil {
			return nil, fmt.Errorf("initializing aws-ecs provider: %s", config.Diagnostic(err))
		}
		prov := provider.NewECSProvider(ecs.NewFromConfig(awsCfg), cloudwatchlogs.NewFromConfig(awsCfg), s3.NewFromConfig(awsCfg), hordeCfg)
		return prov, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q: valid values are \"docker\" and \"aws-ecs\"", name)
	}
}

// initFromRunIDWith looks up the run and creates the matching provider.
// When provFlag is empty, it tries the local SQLite store first — this
// covers docker-only users who have no AWS credentials. If the run is
// not found locally, it falls through to AWS auto-detection.
func initFromRunIDWith(ctx context.Context, provFlag, profile, runID string, deps factoryDeps) (provider.Provider, store.Store, *store.Run, func(), error) {
	// When no explicit provider flag is given, check SQLite first.
	// This avoids AWS auto-detection errors for docker-only users.
	if provFlag == "" {
		if st, cleanup, err := deps.openStore("docker"); err == nil {
			run, err := st.GetRun(ctx, runID)
			if err == nil {
				prov, err := newProviderWith(ctx, run.Provider, profile, deps)
				if err != nil {
					cleanup()
					return nil, nil, nil, nil, err
				}
				return prov, st, run, cleanup, nil
			}
			cleanup()
			// Only fall through to AWS auto-detect when the run simply
			// doesn't exist locally.  Real store errors (disk full,
			// corruption, I/O) must be surfaced immediately.
			if !errors.Is(err, store.ErrRunNotFound) {
				return nil, nil, nil, nil, fmt.Errorf("reading local store: %w", err)
			}
		}
	}

	// Explicit provider flag, or run not found in SQLite.
	prov, st, _, _, _, cleanup, err := initProviderAndStoreWith(ctx, provFlag, profile, deps)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, fmt.Errorf("reading run: %w", err)
	}
	return prov, st, run, cleanup, nil
}

// defaultMaxConcurrentDocker is the local Docker provider's launch ceiling.
// `horde launch` rejects new runs once this many runs are pending or running
// at the same time, with the error "max concurrent runs reached (N/N)".
//
// 100 is intentionally generous: a developer machine will exhaust memory or
// disk long before hitting it under any realistic workflow. The cap exists to
// catch runaway scripts that fork-launch unbounded tickets, not to throttle
// real use. ECS uses its own per-stack `max_concurrent` from SSM config and
// does not consult this constant.
const defaultMaxConcurrentDocker = 100

// initProviderAndStoreWith creates the provider and store. The returned
// *aws.Config is non-nil for aws-ecs (and auto-detect that lands on aws-ecs)
// so callers can reuse the loaded config for downstream STS / SDK calls
// (e.g. resolveLaunchedBy) instead of triggering a fresh awscfg.Load round
// trip. nil for docker.
func initProviderAndStoreWith(ctx context.Context, name, profile string, deps factoryDeps) (provider.Provider, store.Store, int, string, *aws.Config, func(), error) {
	switch name {
	case "docker":
		prov := provider.NewDockerProvider()
		st, cleanup, err := deps.openStore("docker")
		if err != nil {
			return nil, nil, 0, "", nil, nil, err
		}
		return prov, st, defaultMaxConcurrentDocker, "docker", nil, cleanup, nil
	case "aws-ecs":
		awsCfg, err := deps.loadAWSConfig(ctx, profile)
		if err != nil {
			return nil, nil, 0, "", nil, nil, fmt.Errorf("initializing aws-ecs provider: %w", err)
		}
		ssmClient := deps.newSSMClient(awsCfg)
		hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, resolveSSMPath())
		if err != nil {
			return nil, nil, 0, "", nil, nil, fmt.Errorf("initializing aws-ecs provider: %s", config.Diagnostic(err))
		}
		st, err := store.NewDynamoStore(ctx, awsCfg, hordeCfg.RunsTable)
		if err != nil {
			return nil, nil, 0, "", nil, nil, fmt.Errorf("initializing aws-ecs store: %w", err)
		}
		prov := provider.NewECSProvider(ecs.NewFromConfig(awsCfg), cloudwatchlogs.NewFromConfig(awsCfg), s3.NewFromConfig(awsCfg), hordeCfg)
		return prov, st, hordeCfg.MaxConcurrent, "aws-ecs", &awsCfg, func() {}, nil
	case "":
		awsCfg, err := deps.loadAWSConfig(ctx, profile)
		if err != nil {
			return nil, nil, 0, "", nil, nil, fmt.Errorf("auto-detecting provider: %w\n\nhint: use --provider docker for local mode", err)
		}
		ssmClient := deps.newSSMClient(awsCfg)
		hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, resolveSSMPath())
		if err != nil {
			return nil, nil, 0, "", nil, nil, fmt.Errorf("auto-detecting provider: %s\n\nhint: use --provider docker for local mode", config.Diagnostic(err))
		}
		st, err := store.NewDynamoStore(ctx, awsCfg, hordeCfg.RunsTable)
		if err != nil {
			return nil, nil, 0, "", nil, nil, fmt.Errorf("auto-detecting provider: %w\n\nhint: use --provider docker for local mode", err)
		}
		prov := provider.NewECSProvider(ecs.NewFromConfig(awsCfg), cloudwatchlogs.NewFromConfig(awsCfg), s3.NewFromConfig(awsCfg), hordeCfg)
		return prov, st, hordeCfg.MaxConcurrent, "aws-ecs", &awsCfg, func() {}, nil
	default:
		return nil, nil, 0, "", nil, nil, fmt.Errorf("unsupported provider %q: valid values are \"docker\" and \"aws-ecs\"", name)
	}
}
