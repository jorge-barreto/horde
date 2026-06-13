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
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/jorge-barreto/horde/internal/provider"
	"github.com/jorge-barreto/horde/internal/store"
)

// resolveSSMPath returns the SSM parameter path holding the runtime config
// for the current project. Order of precedence:
//
//  1. SSM override (--ssm-path / HORDE_SSM_PATH): used verbatim (escape hatch
//     for custom deployments or projects sharing config across repos).
//  2. Repo override (--repo / HORDE_REPO_URL): derive the slug from it the
//     same way push does and return /horde/<slug>/config. Deterministic and
//     filesystem-free — a malformed override falls back to DefaultSSMPath.
//  3. Discovery: if the working directory has a git remote, derive the slug
//     from it. Any failure here is swallowed to the legacy global
//     config.DefaultSSMPath (/horde/config) so docker-only users with no git
//     and pre-slug deployments keep working.
func resolveSSMPath(r *config.Resolver) string {
	if r != nil && r.SSMOverride != "" {
		return r.SSMOverride
	}
	if r != nil && r.RepoOverride != "" {
		return slugSSMPath(func() (string, error) { return config.CanonicalRepo(r.RepoOverride) })
	}
	dir := ""
	if r != nil {
		dir = r.Dir
	}
	if dir == "" {
		return config.DefaultSSMPath
	}
	return slugSSMPath(func() (string, error) { return config.RepoURL(dir) })
}

// slugSSMPath derives /horde/<slug>/config from a repo produced by getRepo,
// swallowing any failure to config.DefaultSSMPath.
func slugSSMPath(getRepo func() (string, error)) string {
	repo, err := getRepo()
	if err != nil {
		return config.DefaultSSMPath
	}
	slug, err := config.Slug(repo)
	if err != nil {
		return config.DefaultSSMPath
	}
	return "/horde/" + slug + "/config"
}

type factoryDeps struct {
	loadAWSConfig func(ctx context.Context, profile string) (aws.Config, error)
	newSSMClient  func(cfg aws.Config) config.SSMClient
	openStore     func(providerName string) (store.Store, func(), error)
	// resolver carries the identity/discovery overrides into the ECS bring-up
	// (resolveSSMPath). Defaults to a discovery-only resolver scoped to the
	// working directory; CLI entry points replace it via withResolver so flag
	// and env overrides take effect.
	resolver *config.Resolver
}

func defaultFactoryDeps() factoryDeps {
	cwd, _ := os.Getwd()
	return factoryDeps{
		loadAWSConfig: awscfg.Load,
		newSSMClient:  func(cfg aws.Config) config.SSMClient { return ssm.NewFromConfig(cfg) },
		openStore:     openStore,
		resolver:      &config.Resolver{Dir: cwd},
	}
}

// withResolver returns a copy of deps with the resolver replaced. CLI entry
// points use this to inject the flag/env-aware resolver built from the command.
func (d factoryDeps) withResolver(r *config.Resolver) factoryDeps {
	d.resolver = r
	return d
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
		hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, resolveSSMPath(deps.resolver))
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
	prov, st, _, _, _, _, cleanup, err := initProviderAndStoreWith(ctx, provFlag, profile, deps)
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
// trip. nil for docker. The returned *config.HordeConfig follows the same
// convention: populated for aws-ecs so callers can read deployment-scoped
// values like the canonical repo string from SSM, nil for docker (which has
// no SSM analog).
func initProviderAndStoreWith(ctx context.Context, name, profile string, deps factoryDeps) (provider.Provider, store.Store, int, string, *aws.Config, *config.HordeConfig, func(), error) {
	switch name {
	case "docker":
		prov := provider.NewDockerProvider()
		st, cleanup, err := deps.openStore("docker")
		if err != nil {
			return nil, nil, 0, "", nil, nil, nil, err
		}
		return prov, st, defaultMaxConcurrentDocker, "docker", nil, nil, cleanup, nil
	case "aws-ecs":
		// Explicit aws-ecs: no docker fallback hint — the user asked
		// for ECS, so a docker hint would be misleading.
		return initECSProviderAndStore(ctx, profile, deps, "initializing aws-ecs provider", false)
	case "":
		// Auto-detect: every error gets the "use --provider docker for
		// local mode" hint so docker-only users have an obvious recovery.
		return initECSProviderAndStore(ctx, profile, deps, "auto-detecting provider", true)
	default:
		return nil, nil, 0, "", nil, nil, nil, fmt.Errorf("unsupported provider %q: valid values are \"docker\" and \"aws-ecs\"", name)
	}
}

// initECSProviderAndStore performs the shared 4-step ECS bring-up:
// loadAWSConfig → LoadFromSSM → NewDynamoStore → NewECSProvider. errPrefix
// is the action verb at the front of every error ("initializing aws-ecs
// provider" or "auto-detecting provider"). withDockerHint appends the
// fallback hint to every error (used for the auto-detect path so docker-only
// users see the recovery; suppressed for explicit aws-ecs which would be
// misleading).
func initECSProviderAndStore(ctx context.Context, profile string, deps factoryDeps, errPrefix string, withDockerHint bool) (provider.Provider, store.Store, int, string, *aws.Config, *config.HordeConfig, func(), error) {
	hint := ""
	if withDockerHint {
		hint = "\n\nhint: use --provider docker for local mode"
	}

	awsCfg, err := deps.loadAWSConfig(ctx, profile)
	if err != nil {
		return nil, nil, 0, "", nil, nil, nil, fmt.Errorf("%s: %w%s", errPrefix, err, hint)
	}
	ssmClient := deps.newSSMClient(awsCfg)
	hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, resolveSSMPath(deps.resolver))
	if err != nil {
		// Diagnostic returns a formatted string (not an error), so use %s.
		return nil, nil, 0, "", nil, nil, nil, fmt.Errorf("%s: %s%s", errPrefix, config.Diagnostic(err), hint)
	}
	st, err := store.NewDynamoStore(ctx, awsCfg, hordeCfg.RunsTable)
	if err != nil {
		// Preserve the historical "initializing aws-ecs store:" prefix on
		// the explicit-aws-ecs path; the auto-detect path keeps its own
		// errPrefix and tacks on the hint.
		if !withDockerHint {
			return nil, nil, 0, "", nil, nil, nil, fmt.Errorf("initializing aws-ecs store: %w", err)
		}
		return nil, nil, 0, "", nil, nil, nil, fmt.Errorf("%s: %w%s", errPrefix, err, hint)
	}
	prov := provider.NewECSProvider(ecs.NewFromConfig(awsCfg), cloudwatchlogs.NewFromConfig(awsCfg), s3.NewFromConfig(awsCfg), hordeCfg)
	return prov, st, hordeCfg.MaxConcurrent, "aws-ecs", &awsCfg, hordeCfg, func() {}, nil
}
