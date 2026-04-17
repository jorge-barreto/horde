package main

import (
	"context"
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

func openStore(providerName string) (store.Store, func(), error) {
	switch providerName {
	case "docker":
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
	case "aws-ecs":
		return nil, nil, fmt.Errorf("aws-ecs store is not yet implemented")
	default:
		return nil, nil, fmt.Errorf("openStore: unsupported provider %q", providerName)
	}
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
		hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, config.DefaultSSMPath)
		if err != nil {
			return nil, fmt.Errorf("initializing aws-ecs provider: %s", config.Diagnostic(err))
		}
		prov := provider.NewECSProvider(ecs.NewFromConfig(awsCfg), cloudwatchlogs.NewFromConfig(awsCfg), s3.NewFromConfig(awsCfg), hordeCfg)
		return prov, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q: valid values are \"docker\" and \"aws-ecs\"", name)
	}
}

// initFromRunIDWith opens the store, looks up the run, and creates the provider
// from the stored run.Provider field (unless provFlag overrides it).
func initFromRunIDWith(ctx context.Context, provFlag, profile, runID string, deps factoryDeps) (provider.Provider, store.Store, *store.Run, func(), error) {
	if provFlag != "" {
		prov, st, _, cleanup, err := initProviderAndStoreWith(ctx, provFlag, profile, deps)
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

	// No explicit provider — open local store, look up run, use stored provider.
	st, cleanup, err := deps.openStore("docker")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, fmt.Errorf("reading run: %w", err)
	}
	provName := run.Provider
	if provName == "" {
		provName = "docker"
	}
	prov, err := newProviderWith(ctx, provName, profile, deps)
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}
	return prov, st, run, cleanup, nil
}

func initProviderAndStoreWith(ctx context.Context, name, profile string, deps factoryDeps) (provider.Provider, store.Store, int, func(), error) {
	switch name {
	case "docker":
		prov := provider.NewDockerProvider()
		st, cleanup, err := deps.openStore("docker")
		if err != nil {
			return nil, nil, 0, nil, err
		}
		return prov, st, 100, cleanup, nil
	case "aws-ecs":
		awsCfg, err := deps.loadAWSConfig(ctx, profile)
		if err != nil {
			return nil, nil, 0, nil, fmt.Errorf("initializing aws-ecs provider: %w", err)
		}
		ssmClient := deps.newSSMClient(awsCfg)
		hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, config.DefaultSSMPath)
		if err != nil {
			return nil, nil, 0, nil, fmt.Errorf("initializing aws-ecs provider: %s", config.Diagnostic(err))
		}
		st, err := store.NewDynamoStore(ctx, awsCfg, hordeCfg.RunsTable)
		if err != nil {
			return nil, nil, 0, nil, fmt.Errorf("initializing aws-ecs store: %w", err)
		}
		prov := provider.NewECSProvider(ecs.NewFromConfig(awsCfg), cloudwatchlogs.NewFromConfig(awsCfg), s3.NewFromConfig(awsCfg), hordeCfg)
		return prov, st, hordeCfg.MaxConcurrent, func() {}, nil
	case "":
		awsCfg, err := deps.loadAWSConfig(ctx, profile)
		if err != nil {
			return nil, nil, 0, nil, fmt.Errorf("auto-detecting provider: %w\nhint: use --provider docker for local mode", err)
		}
		ssmClient := deps.newSSMClient(awsCfg)
		hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, config.DefaultSSMPath)
		if err != nil {
			return nil, nil, 0, nil, fmt.Errorf("auto-detecting provider: %s\nhint: use --provider docker for local mode", config.Diagnostic(err))
		}
		st, err := store.NewDynamoStore(ctx, awsCfg, hordeCfg.RunsTable)
		if err != nil {
			return nil, nil, 0, nil, fmt.Errorf("initializing aws-ecs store: %w", err)
		}
		prov := provider.NewECSProvider(ecs.NewFromConfig(awsCfg), cloudwatchlogs.NewFromConfig(awsCfg), s3.NewFromConfig(awsCfg), hordeCfg)
		return prov, st, hordeCfg.MaxConcurrent, func() {}, nil
	default:
		return nil, nil, 0, nil, fmt.Errorf("unsupported provider %q: valid values are \"docker\" and \"aws-ecs\"", name)
	}
}
