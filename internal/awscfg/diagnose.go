package awscfg

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

// DiagnosticError is a structured classification of an AWS-related error,
// carrying a human-readable summary plus zero or more suggested commands.
type DiagnosticError struct {
	Summary string
	Hints   []string
}

func (e *DiagnosticError) Error() string {
	if len(e.Hints) == 0 {
		return e.Summary
	}
	return e.Summary + "; run: " + strings.Join(e.Hints, "; ")
}

// Diagnose inspects an AWS-related error and returns a DiagnosticError naming
// the likely fix. If the error cannot be classified, returns a generic hint.
func Diagnose(err error, profile string) *DiagnosticError {
	var profileErr config.SharedConfigProfileNotExistError
	if errors.As(err, &profileErr) {
		return &DiagnosticError{
			Summary: fmt.Sprintf("profile %q not found in ~/.aws/config", profileErr.Profile),
			Hints:   []string{fmt.Sprintf("aws configure --profile %s", profileErr.Profile)},
		}
	}

	var expiredErr *ststypes.ExpiredTokenException
	if errors.As(err, &expiredErr) {
		if profile != "" {
			return &DiagnosticError{
				Summary: "security token expired",
				Hints:   []string{fmt.Sprintf("aws sso login --profile %s", profile)},
			}
		}
		return &DiagnosticError{
			Summary: "security token expired",
			Hints:   []string{"aws sso login"},
		}
	}

	msg := err.Error()

	if strings.Contains(msg, "no EC2 IMDS") ||
		strings.Contains(msg, "failed to refresh cached credentials") ||
		strings.Contains(msg, "AnonymousCredentials") {
		if profile != "" {
			return &DiagnosticError{
				Summary: "no AWS credentials found",
				Hints:   []string{fmt.Sprintf("aws configure --profile %s", profile)},
			}
		}
		return &DiagnosticError{
			Summary: "no AWS credentials found",
			Hints:   []string{"aws configure, or set --profile"},
		}
	}

	lower := strings.ToLower(msg)
	if strings.Contains(lower, "accessdenied") || strings.Contains(lower, "not authorized") {
		return &DiagnosticError{
			Summary: "credentials lack required permissions; ensure the horde CLI managed policy is attached to your IAM user or role",
		}
	}

	if strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "i/o timeout") {
		return &DiagnosticError{
			Summary: "cannot reach AWS; check network connectivity and region configuration",
		}
	}

	if profile != "" {
		return &DiagnosticError{
			Summary: "check AWS credentials and configuration",
			Hints:   []string{fmt.Sprintf("aws configure --profile %s", profile)},
		}
	}
	return &DiagnosticError{
		Summary: "check AWS credentials and configuration",
		Hints:   []string{"aws configure, or set --profile"},
	}
}
