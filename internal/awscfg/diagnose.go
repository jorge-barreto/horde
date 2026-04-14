package awscfg

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

// DiagnoseError inspects an AWS-related error and returns a human-readable
// hint string naming the likely fix. If the error cannot be classified,
// returns a generic hint.
func DiagnoseError(err error, profile string) string {
	// 1. Profile not found
	var profileErr config.SharedConfigProfileNotExistError
	if errors.As(err, &profileErr) {
		return fmt.Sprintf("profile %q not found in ~/.aws/config; run: aws configure --profile %s",
			profileErr.Profile, profileErr.Profile)
	}

	// 2. Expired token
	var expiredErr *ststypes.ExpiredTokenException
	if errors.As(err, &expiredErr) {
		if profile != "" {
			return fmt.Sprintf("security token expired; run: aws sso login --profile %s", profile)
		}
		return "security token expired; refresh credentials with: aws sso login"
	}

	msg := err.Error()

	// 3. No credentials
	if strings.Contains(msg, "no EC2 IMDS") ||
		strings.Contains(msg, "failed to refresh cached credentials") ||
		strings.Contains(msg, "AnonymousCredentials") {
		hint := "no AWS credentials found; run: aws configure"
		if profile == "" {
			hint += ", or set --profile"
		}
		return hint
	}

	// 4. Access denied / not authorized (case-insensitive)
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "accessdenied") || strings.Contains(lower, "not authorized") {
		return "credentials lack required permissions; ensure the horde CLI managed policy is attached to your IAM user or role"
	}

	// 5. Network / connectivity
	if strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "i/o timeout") {
		return "cannot reach AWS; check network connectivity and region configuration"
	}

	// 6. Default fallback
	hint := "check AWS credentials and configuration; run: aws configure"
	if profile == "" {
		hint += ", or set --profile"
	}
	return hint
}
