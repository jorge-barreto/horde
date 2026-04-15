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

type stubStore struct{}

func (s *stubStore) CreateRun(_ context.Context, _ *store.Run) error                 { return nil }
func (s *stubStore) GetRun(_ context.Context, _ string) (*store.Run, error)          { return nil, nil }
func (s *stubStore) UpdateRun(_ context.Context, _ string, _ *store.RunUpdate) error { return nil }
func (s *stubStore) ListByRepo(_ context.Context, _ string, _ bool) ([]*store.Run, error) {
	return nil, nil
}
func (s *stubStore) FindActiveByTicket(_ context.Context, _ string, _ string) ([]*store.Run, error) {
	return nil, nil
}
func (s *stubStore) CountActive(_ context.Context) (int, error) { return 0, nil }

type fakeSSMClient struct {
	output *ssm.GetParameterOutput
	err    error
}

func (f *fakeSSMClient) GetParameter(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return f.output, f.err
}

func validSSMJSON() string {
	return `{"cluster_arn":"arn:aws:ecs:us-east-1:123456789012:cluster/horde","task_definition_arn":"arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1","subnets":["subnet-abc","subnet-def"],"security_group":"sg-123","log_group":"/ecs/horde-worker","artifacts_bucket":"my-horde-artifacts","runs_table":"horde-runs","max_concurrent":5,"default_timeout_minutes":1440}`
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
			wantErr:     true,
			errContains: []string{"aws-ecs provider is not yet implemented"},
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

			prov, st, cleanup, err := initProviderAndStoreWith(context.Background(), tc.provName, "", tc.deps)

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				for _, sub := range tc.errContains {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q does not contain %q", err.Error(), sub)
					}
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
				cleanup()
			}
		})
	}
}
