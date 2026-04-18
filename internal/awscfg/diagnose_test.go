package awscfg

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestDiagnosticError_ImplementsError(t *testing.T) {
	t.Parallel()
	var _ error = (*DiagnosticError)(nil)
}

func TestDiagnose_ProfileNotFound_Fields(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", config.SharedConfigProfileNotExistError{Profile: "staging"})
	got := Diagnose(err, "staging")
	wantSummary := `profile "staging" not found in ~/.aws/config`
	wantHints := []string{"aws configure --profile staging"}
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if !reflect.DeepEqual(got.Hints, wantHints) {
		t.Errorf("Hints = %#v, want %#v", got.Hints, wantHints)
	}
}

func TestDiagnose_ProfileNotFound_Error(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", config.SharedConfigProfileNotExistError{Profile: "staging"})
	got := Diagnose(err, "staging").Error()
	want := `profile "staging" not found in ~/.aws/config; run: aws configure --profile staging`
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestDiagnose_ExpiredToken_Fields(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &ststypes.ExpiredTokenException{Message: aws.String("token expired")})
	got := Diagnose(err, "prod")
	wantSummary := "security token expired"
	wantHints := []string{"aws sso login --profile prod"}
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if !reflect.DeepEqual(got.Hints, wantHints) {
		t.Errorf("Hints = %#v, want %#v", got.Hints, wantHints)
	}
}

func TestDiagnose_ExpiredToken_Error(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &ststypes.ExpiredTokenException{Message: aws.String("token expired")})
	got := Diagnose(err, "prod").Error()
	want := "security token expired; run: aws sso login --profile prod"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestDiagnose_ExpiredTokenNoProfile_Fields(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &ststypes.ExpiredTokenException{Message: aws.String("token expired")})
	got := Diagnose(err, "")
	wantSummary := "security token expired"
	wantHints := []string{"aws sso login"}
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if !reflect.DeepEqual(got.Hints, wantHints) {
		t.Errorf("Hints = %#v, want %#v", got.Hints, wantHints)
	}
}

func TestDiagnose_ExpiredTokenNoProfile_Error(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &ststypes.ExpiredTokenException{Message: aws.String("token expired")})
	got := Diagnose(err, "").Error()
	want := "security token expired; run: aws sso login"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestDiagnose_NoCredentialsWithProfile_Fields(t *testing.T) {
	t.Parallel()
	err := errors.New("failed to refresh cached credentials, no credential providers")
	got := Diagnose(err, "staging")
	wantSummary := "no AWS credentials found"
	wantHints := []string{"aws configure --profile staging"}
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if !reflect.DeepEqual(got.Hints, wantHints) {
		t.Errorf("Hints = %#v, want %#v", got.Hints, wantHints)
	}
}

func TestDiagnose_NoCredentialsWithProfile_Error(t *testing.T) {
	t.Parallel()
	err := errors.New("failed to refresh cached credentials, no credential providers")
	got := Diagnose(err, "staging").Error()
	want := "no AWS credentials found; run: aws configure --profile staging"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestDiagnose_NoCredentialsNoProfile_Fields(t *testing.T) {
	t.Parallel()
	err := errors.New("failed to refresh cached credentials, no credential providers")
	got := Diagnose(err, "")
	wantSummary := "no AWS credentials found"
	wantHints := []string{"aws configure, or set --profile"}
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if !reflect.DeepEqual(got.Hints, wantHints) {
		t.Errorf("Hints = %#v, want %#v", got.Hints, wantHints)
	}
}

func TestDiagnose_NoCredentialsNoProfile_Error(t *testing.T) {
	t.Parallel()
	err := errors.New("failed to refresh cached credentials, no credential providers")
	got := Diagnose(err, "").Error()
	want := "no AWS credentials found; run: aws configure, or set --profile"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestDiagnose_AccessDenied_Fields(t *testing.T) {
	t.Parallel()
	err := errors.New("operation error STS: GetCallerIdentity, AccessDeniedException: not authorized")
	got := Diagnose(err, "dev")
	wantSummary := "credentials lack required permissions; ensure the horde CLI managed policy is attached to your IAM user or role"
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if got.Hints != nil {
		t.Errorf("Hints = %#v, want nil", got.Hints)
	}
}

func TestDiagnose_AccessDenied_Error(t *testing.T) {
	t.Parallel()
	err := errors.New("operation error STS: GetCallerIdentity, AccessDeniedException: not authorized")
	got := Diagnose(err, "dev").Error()
	want := "credentials lack required permissions; ensure the horde CLI managed policy is attached to your IAM user or role"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestDiagnose_Network_Fields(t *testing.T) {
	t.Parallel()
	err := errors.New("dial tcp: lookup sts.us-east-1.amazonaws.com: no such host")
	got := Diagnose(err, "")
	wantSummary := "cannot reach AWS; check network connectivity and region configuration"
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if got.Hints != nil {
		t.Errorf("Hints = %#v, want nil", got.Hints)
	}
}

func TestDiagnose_Network_Error(t *testing.T) {
	t.Parallel()
	err := errors.New("dial tcp: lookup sts.us-east-1.amazonaws.com: no such host")
	got := Diagnose(err, "").Error()
	want := "cannot reach AWS; check network connectivity and region configuration"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestDiagnose_FallbackWithProfile_Fields(t *testing.T) {
	t.Parallel()
	err := errors.New("some unknown failure")
	got := Diagnose(err, "myprofile")
	wantSummary := "check AWS credentials and configuration"
	wantHints := []string{"aws configure --profile myprofile"}
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if !reflect.DeepEqual(got.Hints, wantHints) {
		t.Errorf("Hints = %#v, want %#v", got.Hints, wantHints)
	}
}

func TestDiagnose_FallbackWithProfile_Error(t *testing.T) {
	t.Parallel()
	err := errors.New("some unknown failure")
	got := Diagnose(err, "myprofile").Error()
	want := "check AWS credentials and configuration; run: aws configure --profile myprofile"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestDiagnose_FallbackNoProfile_Fields(t *testing.T) {
	t.Parallel()
	err := errors.New("some unknown failure")
	got := Diagnose(err, "")
	wantSummary := "check AWS credentials and configuration"
	wantHints := []string{"aws configure, or set --profile"}
	if got.Summary != wantSummary {
		t.Errorf("Summary = %q, want %q", got.Summary, wantSummary)
	}
	if !reflect.DeepEqual(got.Hints, wantHints) {
		t.Errorf("Hints = %#v, want %#v", got.Hints, wantHints)
	}
}

func TestDiagnose_FallbackNoProfile_Error(t *testing.T) {
	t.Parallel()
	err := errors.New("some unknown failure")
	got := Diagnose(err, "").Error()
	want := "check AWS credentials and configuration; run: aws configure, or set --profile"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
