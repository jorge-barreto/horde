package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	smithy "github.com/aws/smithy-go"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/jorge-barreto/horde/internal/provider"
	"github.com/jorge-barreto/horde/internal/store"
)

type stubStore struct {
	run    *store.Run
	runErr error
}

func (s *stubStore) CreateRun(_ context.Context, _ *store.Run) error { return nil }
func (s *stubStore) GetRun(_ context.Context, _ string) (*store.Run, error) {
	return s.run, s.runErr
}
func (s *stubStore) UpdateRun(_ context.Context, _ string, _ *store.RunUpdate) error { return nil }
func (s *stubStore) ListByRepo(_ context.Context, _ string, _ bool) ([]*store.Run, error) {
	return nil, nil
}
func (s *stubStore) FindActiveByTicket(_ context.Context, _ string, _ string) ([]*store.Run, error) {
	return nil, nil
}
func (s *stubStore) CountActive(_ context.Context) (int, error)         { return 0, nil }
func (s *stubStore) ListActive(_ context.Context) ([]*store.Run, error) { return nil, nil }
func (s *stubStore) Close() error                                       { return nil }

type fakeSSMClient struct {
	output *ssm.GetParameterOutput
	err    error
}

func (f *fakeSSMClient) GetParameter(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return f.output, f.err
}

func validSSMJSON() string {
	return `{"cluster_arn":"arn:aws:ecs:us-east-1:123456789012:cluster/horde","task_definition_arn":"arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1","subnets":["subnet-abc","subnet-def"],"security_group":"sg-123","log_group":"/ecs/horde-worker","log_stream_prefix":"ecs","artifacts_bucket":"my-horde-artifacts","runs_table":"horde-runs","ecr_repo_uri":"123456789012.dkr.ecr.us-east-1.amazonaws.com/horde-myproj","max_concurrent":5,"default_timeout_minutes":1440}`
}

func TestInitProviderAndStore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		provName    string
		deps        factoryDeps
		wantErr     bool
		errContains []string
	}{
		{
			name:     "explicit docker",
			provName: "docker",
			deps: factoryDeps{
				openStore: func(_ string) (store.Store, func(), error) {
					return &stubStore{}, func() {}, nil
				},
			},
			wantErr: false,
		},
		{
			name:     "explicit docker openStore error",
			provName: "docker",
			deps: factoryDeps{
				openStore: func(_ string) (store.Store, func(), error) {
					return nil, nil, fmt.Errorf("cannot open SQLite at /bad/path")
				},
			},
			wantErr:     true,
			errContains: []string{"cannot open SQLite"},
		},
		{
			name:     "explicit aws-ecs no-creds",
			provName: "aws-ecs",
			deps: factoryDeps{
				loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
					return aws.Config{}, fmt.Errorf("no valid credential sources")
				},
			},
			wantErr:     true,
			errContains: []string{"initializing aws-ecs provider"},
		},
		{
			name:     "explicit aws-ecs SSM ok",
			provName: "aws-ecs",
			deps: factoryDeps{
				loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
					return aws.Config{}, nil
				},
				newSSMClient: func(_ aws.Config) config.SSMClient {
					return &fakeSSMClient{
						output: &ssm.GetParameterOutput{
							Parameter: &ssmtypes.Parameter{Value: aws.String(validSSMJSON())},
						},
					}
				},
			},
			wantErr: false,
		},
		{
			name:     "default SSM ok",
			provName: "",
			deps: factoryDeps{
				loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
					return aws.Config{}, nil
				},
				newSSMClient: func(_ aws.Config) config.SSMClient {
					return &fakeSSMClient{
						output: &ssm.GetParameterOutput{
							Parameter: &ssmtypes.Parameter{Value: aws.String(validSSMJSON())},
						},
					}
				},
			},
			wantErr: false,
		},
		{
			name:     "default SSM missing",
			provName: "",
			deps: factoryDeps{
				loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
					return aws.Config{}, nil
				},
				newSSMClient: func(_ aws.Config) config.SSMClient {
					return &fakeSSMClient{
						err: &ssmtypes.ParameterNotFound{Message: aws.String("not found")},
					}
				},
			},
			wantErr:     true,
			errContains: []string{"auto-detecting provider", "deploy the @horde/cdk construct"},
		},
		{
			name:     "default SSM access denied",
			provName: "",
			deps: factoryDeps{
				loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
					return aws.Config{}, nil
				},
				newSSMClient: func(_ aws.Config) config.SSMClient {
					return &fakeSSMClient{
						err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized"},
					}
				},
			},
			wantErr:     true,
			errContains: []string{"auto-detecting provider", "attach the horde CLI user managed policy"},
		},
		{
			name:        "unsupported provider",
			provName:    "gcp",
			deps:        factoryDeps{},
			wantErr:     true,
			errContains: []string{`unsupported provider "gcp"`},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prov, st, maxConcurrent, gotProvName, _, cleanup, err := initProviderAndStoreWith(context.Background(), tc.provName, "", tc.deps)

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				for _, sub := range tc.errContains {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q does not contain %q", err.Error(), sub)
					}
				}
				if prov != nil {
					t.Errorf("prov on error = %v, want nil", prov)
				}
				if st != nil {
					t.Errorf("store on error = %v, want nil", st)
				}
				if cleanup != nil {
					t.Error("cleanup on error != nil, want nil")
				}
				if maxConcurrent != 0 {
					t.Errorf("maxConcurrent on error = %d, want 0", maxConcurrent)
				}
				if gotProvName != "" {
					t.Errorf("provName on error = %q, want \"\"", gotProvName)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.provName == "docker" {
				if _, ok := prov.(*provider.DockerProvider); !ok {
					t.Errorf("expected *provider.DockerProvider, got %T", prov)
				}
				if st == nil {
					t.Error("expected non-nil store")
				}
				if cleanup == nil {
					t.Error("expected non-nil cleanup")
				}
				if maxConcurrent != 100 {
					t.Errorf("maxConcurrent: got %d, want 100", maxConcurrent)
				}
				if gotProvName != "docker" {
					t.Errorf("provName: got %q, want %q", gotProvName, "docker")
				}
				cleanup()
			} else {
				if _, ok := prov.(*provider.ECSProvider); !ok {
					t.Errorf("expected *provider.ECSProvider, got %T", prov)
				}
				if st == nil {
					t.Error("expected non-nil store")
				}
				if cleanup == nil {
					t.Error("expected non-nil cleanup")
				}
				if gotProvName != "aws-ecs" {
					t.Errorf("provName: got %q, want %q", gotProvName, "aws-ecs")
				}
				cleanup()
			}
		})
	}
}

func TestNewProviderWith(t *testing.T) {
	t.Parallel()

	t.Run("docker", func(t *testing.T) {
		t.Parallel()
		prov, err := newProviderWith(context.Background(), "docker", "", defaultFactoryDeps())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := prov.(*provider.DockerProvider); !ok {
			t.Errorf("expected *provider.DockerProvider, got %T", prov)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		t.Parallel()
		_, err := newProviderWith(context.Background(), "gcp", "", defaultFactoryDeps())
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), `unsupported provider "gcp"`) {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("aws-ecs", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
				return aws.Config{}, nil
			},
			newSSMClient: func(_ aws.Config) config.SSMClient {
				return &fakeSSMClient{
					output: &ssm.GetParameterOutput{
						Parameter: &ssmtypes.Parameter{Value: aws.String(validSSMJSON())},
					},
				}
			},
		}
		prov, err := newProviderWith(context.Background(), "aws-ecs", "", deps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := prov.(*provider.ECSProvider); !ok {
			t.Errorf("expected *provider.ECSProvider, got %T", prov)
		}
	})
}

func TestAutoDetect_HintBlocksSeparatedByBlankLine(t *testing.T) {
	t.Parallel()

	// When the SSM Diagnostic emits its own hint block, the factory's
	// trailing "--provider docker" hint must be separated from it by a
	// blank line so the two hint: prefixes don't collide visually.
	t.Run("ssm NotFound diagnostic", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
				return aws.Config{}, nil
			},
			newSSMClient: func(_ aws.Config) config.SSMClient {
				return &fakeSSMClient{
					err: &ssmtypes.ParameterNotFound{Message: aws.String("not found")},
				}
			},
		}
		_, _, _, _, _, _, err := initProviderAndStoreWith(context.Background(), "", "", deps)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, "\n\nhint: use --provider docker for local mode") {
			t.Errorf("expected blank line before fallback hint, got:\n%s", msg)
		}
	})

	// When loadAWSConfig itself fails (no Diagnostic block), the fallback
	// hint still renders with the blank-line separator — harmless when
	// there is no preceding hint block.
	t.Run("loadAWSConfig failure", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
				return aws.Config{}, fmt.Errorf("no AWS credentials")
			},
		}
		_, _, _, _, _, _, err := initProviderAndStoreWith(context.Background(), "", "", deps)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, "\n\nhint: use --provider docker for local mode") {
			t.Errorf("expected blank line before fallback hint, got:\n%s", msg)
		}
	})
}

func TestInitFromRunID(t *testing.T) {
	t.Parallel()

	t.Run("no flag SQLite miss falls through to auto-detect", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			openStore: func(_ string) (store.Store, func(), error) {
				return &stubStore{runErr: store.ErrRunNotFound}, func() {}, nil
			},
			loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
				return aws.Config{}, fmt.Errorf("no AWS credentials")
			},
		}
		_, _, _, _, err := initFromRunIDWith(context.Background(), "", "", "abc123", deps)
		if err == nil {
			t.Fatal("expected error from auto-detect, got nil")
		}
		if !strings.Contains(err.Error(), "auto-detecting provider") {
			t.Errorf("expected auto-detect error, got: %v", err)
		}
	})

	t.Run("no flag finds run in SQLite uses stored docker provider", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			openStore: func(_ string) (store.Store, func(), error) {
				return &stubStore{
					run: &store.Run{ID: "abc123", Provider: "docker"},
				}, func() {}, nil
			},
			loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
				t.Fatal("loadAWSConfig should not be called when run is found in SQLite")
				return aws.Config{}, nil
			},
		}
		prov, st, run, cleanup, err := initFromRunIDWith(context.Background(), "", "", "abc123", deps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer cleanup()
		if _, ok := prov.(*provider.DockerProvider); !ok {
			t.Errorf("expected *provider.DockerProvider, got %T", prov)
		}
		if st == nil {
			t.Error("expected non-nil store")
		}
		if run.ID != "abc123" {
			t.Errorf("run.ID: got %q, want %q", run.ID, "abc123")
		}
	})

	t.Run("no flag SQLite open error falls through to auto-detect", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			openStore: func(name string) (store.Store, func(), error) {
				if name == "docker" {
					return nil, nil, fmt.Errorf("cannot open SQLite")
				}
				return &stubStore{}, func() {}, nil
			},
			loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
				return aws.Config{}, fmt.Errorf("no AWS credentials")
			},
		}
		_, _, _, _, err := initFromRunIDWith(context.Background(), "", "", "abc123", deps)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "auto-detecting provider") {
			t.Errorf("expected auto-detect fallback error, got: %v", err)
		}
	})

	t.Run("no flag GetRun I/O error is returned not swallowed", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			openStore: func(_ string) (store.Store, func(), error) {
				return &stubStore{runErr: fmt.Errorf("disk I/O error")}, func() {}, nil
			},
			loadAWSConfig: func(_ context.Context, _ string) (aws.Config, error) {
				t.Fatal("loadAWSConfig should not be called when GetRun returns a real error")
				return aws.Config{}, nil
			},
		}
		_, _, _, _, err := initFromRunIDWith(context.Background(), "", "", "abc123", deps)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "reading local store") {
			t.Errorf("expected 'reading local store' wrapper, got: %v", err)
		}
		if !strings.Contains(err.Error(), "disk I/O error") {
			t.Errorf("expected original error to be preserved, got: %v", err)
		}
	})

	t.Run("explicit flag overrides stored provider", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			openStore: func(_ string) (store.Store, func(), error) {
				return &stubStore{
					run: &store.Run{ID: "abc123", Provider: "aws-ecs"},
				}, func() {}, nil
			},
		}
		prov, _, _, cleanup, err := initFromRunIDWith(context.Background(), "docker", "", "abc123", deps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer cleanup()
		if _, ok := prov.(*provider.DockerProvider); !ok {
			t.Errorf("expected *provider.DockerProvider, got %T", prov)
		}
	})

	t.Run("run not found", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			openStore: func(_ string) (store.Store, func(), error) {
				return &stubStore{
					runErr: store.ErrRunNotFound,
				}, func() {}, nil
			},
		}
		_, _, _, _, err := initFromRunIDWith(context.Background(), "docker", "", "missing", deps)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "run not found") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("docker flag with empty stored provider", func(t *testing.T) {
		t.Parallel()
		deps := factoryDeps{
			openStore: func(_ string) (store.Store, func(), error) {
				return &stubStore{
					run: &store.Run{ID: "abc123", Provider: ""},
				}, func() {}, nil
			},
		}
		prov, _, run, cleanup, err := initFromRunIDWith(context.Background(), "docker", "", "abc123", deps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer cleanup()
		if _, ok := prov.(*provider.DockerProvider); !ok {
			t.Errorf("expected *provider.DockerProvider, got %T", prov)
		}
		if run.Provider != "" {
			t.Errorf("expected empty stored provider, got %q", run.Provider)
		}
	})

}
