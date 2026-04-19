package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/smithy-go"
)

// CFClient is the subset of cloudformation.Client methods used by Deploy.
// The full client from the AWS SDK satisfies this interface; tests substitute
// a fake.
type CFClient interface {
	DescribeStacks(ctx context.Context, in *cloudformation.DescribeStacksInput, opts ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error)
	CreateStack(ctx context.Context, in *cloudformation.CreateStackInput, opts ...func(*cloudformation.Options)) (*cloudformation.CreateStackOutput, error)
	UpdateStack(ctx context.Context, in *cloudformation.UpdateStackInput, opts ...func(*cloudformation.Options)) (*cloudformation.UpdateStackOutput, error)
	DeleteStack(ctx context.Context, in *cloudformation.DeleteStackInput, opts ...func(*cloudformation.Options)) (*cloudformation.DeleteStackOutput, error)
	DescribeStackEvents(ctx context.Context, in *cloudformation.DescribeStackEventsInput, opts ...func(*cloudformation.Options)) (*cloudformation.DescribeStackEventsOutput, error)
}

// DeployRequest holds the inputs required by Deploy.
type DeployRequest struct {
	StackName       string
	Slug            string
	TemplateBody    string
	AnthropicAPIKey string
	GitToken        string
	// PollInterval controls how often DescribeStacks is called while waiting
	// for a terminal status. If zero, defaults to 5s. Tests use a small value.
	PollInterval time.Duration
}

// Deploy creates or updates the CloudFormation stack described by req, then
// polls DescribeStacks until a terminal status is reached. Progress events
// are written to w as `<timestamp> <LogicalResourceId> <ResourceStatus> <ResourceStatusReason>`.
//
// Returns nil on CREATE_COMPLETE / UPDATE_COMPLETE. Returns nil if UpdateStack
// reports "No updates are to be performed" (the stack already matches the
// template + parameters). Returns an error for any *_FAILED or ROLLBACK_*
// terminal status.
func Deploy(ctx context.Context, client CFClient, req DeployRequest, w io.Writer) error {
	if req.PollInterval <= 0 {
		req.PollInterval = 5 * time.Second
	}
	if strings.TrimSpace(req.StackName) == "" {
		return fmt.Errorf("deploy: stack name is empty")
	}
	if strings.TrimSpace(req.TemplateBody) == "" {
		return fmt.Errorf("deploy: template body is empty")
	}
	if req.AnthropicAPIKey == "" {
		return fmt.Errorf("deploy: ANTHROPIC_API_KEY is empty")
	}
	if req.GitToken == "" {
		return fmt.Errorf("deploy: GIT_TOKEN is empty")
	}

	exists, err := stackExists(ctx, client, req.StackName)
	if err != nil {
		return err
	}

	params := []cftypes.Parameter{
		{
			ParameterKey:     aws.String("AnthropicApiKey"),
			ParameterValue:   aws.String(req.AnthropicAPIKey),
			UsePreviousValue: aws.Bool(false),
		},
		{
			ParameterKey:     aws.String("GitToken"),
			ParameterValue:   aws.String(req.GitToken),
			UsePreviousValue: aws.Bool(false),
		},
	}
	tags := []cftypes.Tag{
		{Key: aws.String("horde-slug"), Value: aws.String(req.Slug)},
	}
	caps := []cftypes.Capability{cftypes.CapabilityCapabilityNamedIam}

	if exists {
		fmt.Fprintf(w, "Updating stack %s...\n", req.StackName)
		_, err := client.UpdateStack(ctx, &cloudformation.UpdateStackInput{
			StackName:    aws.String(req.StackName),
			TemplateBody: aws.String(req.TemplateBody),
			Parameters:   params,
			Capabilities: caps,
			Tags:         tags,
		})
		if err != nil {
			if isNoUpdatesError(err) {
				fmt.Fprintln(w, "No updates are to be performed.")
				return nil
			}
			return fmt.Errorf("updating stack %s: %w", req.StackName, err)
		}
	} else {
		fmt.Fprintf(w, "Creating stack %s...\n", req.StackName)
		_, err := client.CreateStack(ctx, &cloudformation.CreateStackInput{
			StackName:    aws.String(req.StackName),
			TemplateBody: aws.String(req.TemplateBody),
			Parameters:   params,
			Capabilities: caps,
			Tags:         tags,
		})
		if err != nil {
			return fmt.Errorf("creating stack %s: %w", req.StackName, err)
		}
	}

	return waitForStack(ctx, client, req.StackName, req.PollInterval, w)
}

// Destroy deletes the named CloudFormation stack and polls until the stack is
// gone (DescribeStacks returns a "does not exist" ValidationError) or until
// the stack reaches a DELETE_FAILED terminal status. Events are streamed to w.
//
// PollInterval defaults to 5s if zero. Returns nil on successful deletion.
func Destroy(ctx context.Context, client CFClient, stackName string, pollInterval time.Duration, w io.Writer) error {
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	if strings.TrimSpace(stackName) == "" {
		return fmt.Errorf("destroy: stack name is empty")
	}

	exists, err := stackExists(ctx, client, stackName)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(w, "Stack %s does not exist; nothing to destroy.\n", stackName)
		return nil
	}

	fmt.Fprintf(w, "Deleting stack %s...\n", stackName)
	if _, err := client.DeleteStack(ctx, &cloudformation.DeleteStackInput{
		StackName: aws.String(stackName),
	}); err != nil {
		return fmt.Errorf("deleting stack %s: %w", stackName, err)
	}

	return waitForDelete(ctx, client, stackName, pollInterval, w)
}

// waitForDelete polls DescribeStacks until the stack disappears (treated as
// successful deletion) or reaches DELETE_FAILED.
func waitForDelete(ctx context.Context, client CFClient, name string, interval time.Duration, w io.Writer) error {
	var lastEventTime time.Time
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false

		lastEventTime = printNewEvents(ctx, client, name, lastEventTime, w)

		out, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: aws.String(name),
		})
		if err != nil {
			if isNotExistError(err) {
				// Stack has been fully deleted.
				return nil
			}
			return fmt.Errorf("describing stack %s: %w", name, err)
		}
		if len(out.Stacks) == 0 {
			return nil
		}
		status := out.Stacks[0].StackStatus
		if status == cftypes.StackStatusDeleteComplete {
			printNewEvents(ctx, client, name, lastEventTime, w)
			return nil
		}
		if status == cftypes.StackStatusDeleteFailed {
			printNewEvents(ctx, client, name, lastEventTime, w)
			reason := ""
			if out.Stacks[0].StackStatusReason != nil {
				reason = ": " + *out.Stacks[0].StackStatusReason
			}
			return fmt.Errorf("stack %s: DELETE_FAILED%s", name, reason)
		}
	}
}

// stackExists reports whether the named stack already exists. A
// ValidationError with "does not exist" is treated as absence. Other errors
// propagate.
func stackExists(ctx context.Context, client CFClient, name string) (bool, error) {
	_, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: aws.String(name),
	})
	if err == nil {
		return true, nil
	}
	if isNotExistError(err) {
		return false, nil
	}
	return false, fmt.Errorf("describing stack %s: %w", name, err)
}

// waitForStack polls DescribeStacks and DescribeStackEvents until the stack
// reaches a terminal status.
func waitForStack(ctx context.Context, client CFClient, name string, interval time.Duration, w io.Writer) error {
	var lastEventTime time.Time
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false

		lastEventTime = printNewEvents(ctx, client, name, lastEventTime, w)

		out, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: aws.String(name),
		})
		if err != nil {
			// If the stack vanished mid-deploy, treat as failure.
			if isNotExistError(err) {
				return fmt.Errorf("stack %s disappeared during wait", name)
			}
			return fmt.Errorf("describing stack %s: %w", name, err)
		}
		if len(out.Stacks) == 0 {
			return fmt.Errorf("describing stack %s: no stacks returned", name)
		}
		status := out.Stacks[0].StackStatus
		switch status {
		case cftypes.StackStatusCreateComplete, cftypes.StackStatusUpdateComplete:
			// Drain any remaining events before returning.
			printNewEvents(ctx, client, name, lastEventTime, w)
			return nil
		}
		if isFailureStatus(status) {
			printNewEvents(ctx, client, name, lastEventTime, w)
			reason := ""
			if out.Stacks[0].StackStatusReason != nil {
				reason = ": " + *out.Stacks[0].StackStatusReason
			}
			return fmt.Errorf("stack %s reached terminal failure status %s%s", name, status, reason)
		}
		// Otherwise still in progress (*_IN_PROGRESS, UPDATE_COMPLETE_CLEANUP_IN_PROGRESS, etc.).
	}
}

// printNewEvents writes any stack events newer than after to w and returns
// the timestamp of the most recent event seen (or after if there were none).
// Errors are swallowed — event printing is best-effort progress output.
func printNewEvents(ctx context.Context, client CFClient, name string, after time.Time, w io.Writer) time.Time {
	out, err := client.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{
		StackName: aws.String(name),
	})
	if err != nil {
		return after
	}
	// Events are returned most-recent-first. Print oldest-first for readability.
	newest := after
	fresh := make([]cftypes.StackEvent, 0, len(out.StackEvents))
	for _, e := range out.StackEvents {
		if e.Timestamp == nil {
			continue
		}
		if !e.Timestamp.After(after) {
			continue
		}
		fresh = append(fresh, e)
		if e.Timestamp.After(newest) {
			newest = *e.Timestamp
		}
	}
	// Reverse to chronological order.
	for i := len(fresh) - 1; i >= 0; i-- {
		e := fresh[i]
		ts := e.Timestamp.Format(time.RFC3339)
		logical := derefString(e.LogicalResourceId)
		status := string(e.ResourceStatus)
		reason := derefString(e.ResourceStatusReason)
		if reason != "" {
			fmt.Fprintf(w, "%s %s %s %s\n", ts, logical, status, reason)
		} else {
			fmt.Fprintf(w, "%s %s %s\n", ts, logical, status)
		}
	}
	return newest
}

// isFailureStatus reports whether the given CFN stack status is a terminal
// failure for Deploy's purposes. ROLLBACK_COMPLETE and UPDATE_ROLLBACK_COMPLETE
// are terminal failure states (the deploy did not apply).
func isFailureStatus(s cftypes.StackStatus) bool {
	str := string(s)
	if strings.HasSuffix(str, "_FAILED") {
		return true
	}
	switch s {
	case cftypes.StackStatusRollbackComplete,
		cftypes.StackStatusRollbackFailed,
		cftypes.StackStatusRollbackInProgress,
		cftypes.StackStatusUpdateRollbackComplete,
		cftypes.StackStatusUpdateRollbackFailed,
		cftypes.StackStatusUpdateRollbackInProgress,
		cftypes.StackStatusDeleteFailed:
		return true
	}
	return false
}

// isNotExistError reports whether err is the CloudFormation ValidationError
// indicating the named stack does not exist.
func isNotExistError(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if apiErr.ErrorCode() == "ValidationError" && strings.Contains(apiErr.ErrorMessage(), "does not exist") {
			return true
		}
	}
	// Fallback: string match for fakes.
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		return true
	}
	return false
}

// isNoUpdatesError reports whether err is the benign "No updates are to be
// performed" error returned by UpdateStack when the template + parameters
// match the live stack exactly.
func isNoUpdatesError(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if strings.Contains(apiErr.ErrorMessage(), "No updates are to be performed") {
			return true
		}
	}
	return err != nil && strings.Contains(err.Error(), "No updates are to be performed")
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
