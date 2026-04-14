package awscfg

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestDiagnoseError_ProfileNotFound(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", config.SharedConfigProfileNotExistError{Profile: "staging"})
	result := DiagnoseError(err, "staging")
	if !strings.Contains(result, `profile "staging" not found`) {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, `profile "staging" not found`)
	}
	if !strings.Contains(result, "aws configure --profile staging") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "aws configure --profile staging")
	}
}

func TestDiagnoseError_ExpiredToken(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &ststypes.ExpiredTokenException{Message: aws.String("token expired")})
	result := DiagnoseError(err, "prod")
	if !strings.Contains(result, "security token expired") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "security token expired")
	}
	if !strings.Contains(result, "aws sso login --profile prod") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "aws sso login --profile prod")
	}
}

func TestDiagnoseError_ExpiredToken_NoProfile(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &ststypes.ExpiredTokenException{Message: aws.String("token expired")})
	result := DiagnoseError(err, "")
	if !strings.Contains(result, "aws sso login") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "aws sso login")
	}
	if strings.Contains(result, "--profile") {
		t.Errorf("DiagnoseError() = %q, want it NOT to contain %q", result, "--profile")
	}
}

func TestDiagnoseError_NoCredentials(t *testing.T) {
	t.Parallel()
	err := errors.New("failed to refresh cached credentials, no credential providers")
	result := DiagnoseError(err, "")
	if !strings.Contains(result, "no AWS credentials found") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "no AWS credentials found")
	}
	if !strings.Contains(result, "aws configure") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "aws configure")
	}
}

func TestDiagnoseError_NoCredentials_WithProfile(t *testing.T) {
	t.Parallel()
	err := errors.New("failed to refresh cached credentials, no credential providers")
	result := DiagnoseError(err, "staging")
	if !strings.Contains(result, "no AWS credentials found") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "no AWS credentials found")
	}
	if !strings.Contains(result, "aws configure --profile staging") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "aws configure --profile staging")
	}
}

func TestDiagnoseError_AccessDenied(t *testing.T) {
	t.Parallel()
	err := errors.New("operation error STS: GetCallerIdentity, AccessDeniedException: not authorized")
	result := DiagnoseError(err, "dev")
	if !strings.Contains(result, "credentials lack required permissions") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "credentials lack required permissions")
	}
	if !strings.Contains(result, "managed policy") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "managed policy")
	}
}

func TestDiagnoseError_NetworkError(t *testing.T) {
	t.Parallel()
	err := errors.New("dial tcp: lookup sts.us-east-1.amazonaws.com: no such host")
	result := DiagnoseError(err, "")
	if !strings.Contains(result, "cannot reach AWS") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "cannot reach AWS")
	}
}

func TestDiagnoseError_UnknownError_WithProfile(t *testing.T) {
	t.Parallel()
	err := errors.New("some unknown failure")
	result := DiagnoseError(err, "myprofile")
	if !strings.Contains(result, "check AWS credentials") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "check AWS credentials")
	}
	if !strings.Contains(result, "aws configure") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "aws configure")
	}
	if strings.Contains(result, "set --profile") {
		t.Errorf("DiagnoseError() = %q, want it NOT to contain %q", result, "set --profile")
	}
	if !strings.Contains(result, "aws configure --profile myprofile") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "aws configure --profile myprofile")
	}
}

func TestDiagnoseError_UnknownError_NoProfile(t *testing.T) {
	t.Parallel()
	err := errors.New("some unknown failure")
	result := DiagnoseError(err, "")
	if !strings.Contains(result, "check AWS credentials") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "check AWS credentials")
	}
	if !strings.Contains(result, "aws configure") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "aws configure")
	}
	if !strings.Contains(result, "set --profile") {
		t.Errorf("DiagnoseError() = %q, want it to contain %q", result, "set --profile")
	}
}
