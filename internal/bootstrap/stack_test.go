package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/smithy-go"
)

// validationErr is a minimal implementation of smithy.APIError for tests that
// want to exercise the APIError-aware branches of isNotExistError / isNoUpdatesError.
type validationErr struct {
	code string
	msg  string
}

func (v *validationErr) Error() string                          { return fmt.Sprintf("%s: %s", v.code, v.msg) }
func (v *validationErr) ErrorCode() string                      { return v.code }
func (v *validationErr) ErrorMessage() string                   { return v.msg }
func (v *validationErr) ErrorFault() smithy.ErrorFault          { return smithy.FaultClient }
func (v *validationErr) Unwrap() error                          { return nil }

var _ smithy.APIError = (*validationErr)(nil)

// fakeCFClient is a scriptable CFClient for tests.
type fakeCFClient struct {
	describeResponses []describeResponse
	describeCalls     int

	createCalls int
	updateCalls int

	updateErr error

	events []cftypes.StackEvent

	describeTimestamps []time.Time // records time.Now() of each DescribeStacks call
}

type describeResponse struct {
	status cftypes.StackStatus
	reason string
	err    error
}

func (f *fakeCFClient) DescribeStacks(ctx context.Context, in *cloudformation.DescribeStacksInput, _ ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error) {
	f.describeTimestamps = append(f.describeTimestamps, time.Now())
	idx := f.describeCalls
	if idx >= len(f.describeResponses) {
		idx = len(f.describeResponses) - 1
	}
	r := f.describeResponses[idx]
	f.describeCalls++
	if r.err != nil {
		return nil, r.err
	}
	s := cftypes.Stack{
		StackName:   in.StackName,
		StackStatus: r.status,
	}
	if r.reason != "" {
		s.StackStatusReason = aws.String(r.reason)
	}
	return &cloudformation.DescribeStacksOutput{Stacks: []cftypes.Stack{s}}, nil
}

func (f *fakeCFClient) CreateStack(ctx context.Context, in *cloudformation.CreateStackInput, _ ...func(*cloudformation.Options)) (*cloudformation.CreateStackOutput, error) {
	f.createCalls++
	return &cloudformation.CreateStackOutput{StackId: aws.String("stack/fake")}, nil
}

func (f *fakeCFClient) UpdateStack(ctx context.Context, in *cloudformation.UpdateStackInput, _ ...func(*cloudformation.Options)) (*cloudformation.UpdateStackOutput, error) {
	f.updateCalls++
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &cloudformation.UpdateStackOutput{StackId: aws.String("stack/fake")}, nil
}

func (f *fakeCFClient) DescribeStackEvents(ctx context.Context, in *cloudformation.DescribeStackEventsInput, _ ...func(*cloudformation.Options)) (*cloudformation.DescribeStackEventsOutput, error) {
	return &cloudformation.DescribeStackEventsOutput{StackEvents: f.events}, nil
}

func baseReq() DeployRequest {
	return DeployRequest{
		StackName:       "horde-test",
		Slug:            "test",
		TemplateBody:    "AWSTemplateFormatVersion: '2010-09-09'\nResources: {}",
		AnthropicAPIKey: "sk-test",
		GitToken:        "ghp_test",
		PollInterval:    10 * time.Millisecond,
	}
}

func TestDeploy_CreatesWhenMissing(t *testing.T) {
	c := &fakeCFClient{
		describeResponses: []describeResponse{
			{err: &validationErr{code: "ValidationError", msg: "Stack with id horde-test does not exist"}},
			{status: cftypes.StackStatusCreateInProgress},
			{status: cftypes.StackStatusCreateComplete},
		},
	}
	var buf bytes.Buffer
	if err := Deploy(context.Background(), c, baseReq(), &buf); err != nil {
		t.Fatalf("Deploy: %v\noutput:\n%s", err, buf.String())
	}
	if c.createCalls != 1 {
		t.Errorf("CreateStack calls = %d, want 1", c.createCalls)
	}
	if c.updateCalls != 0 {
		t.Errorf("UpdateStack calls = %d, want 0", c.updateCalls)
	}
}

func TestDeploy_UpdatesWhenExisting(t *testing.T) {
	c := &fakeCFClient{
		describeResponses: []describeResponse{
			{status: cftypes.StackStatusCreateComplete},
			{status: cftypes.StackStatusUpdateInProgress},
			{status: cftypes.StackStatusUpdateComplete},
		},
	}
	var buf bytes.Buffer
	if err := Deploy(context.Background(), c, baseReq(), &buf); err != nil {
		t.Fatalf("Deploy: %v\noutput:\n%s", err, buf.String())
	}
	if c.updateCalls != 1 {
		t.Errorf("UpdateStack calls = %d, want 1", c.updateCalls)
	}
	if c.createCalls != 0 {
		t.Errorf("CreateStack calls = %d, want 0", c.createCalls)
	}
}

func TestDeploy_NoUpdatesIsSuccess(t *testing.T) {
	c := &fakeCFClient{
		describeResponses: []describeResponse{
			{status: cftypes.StackStatusCreateComplete},
		},
		updateErr: &validationErr{code: "ValidationError", msg: "No updates are to be performed."},
	}
	var buf bytes.Buffer
	if err := Deploy(context.Background(), c, baseReq(), &buf); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !strings.Contains(buf.String(), "No updates are to be performed") {
		t.Errorf("expected 'No updates are to be performed' in output; got:\n%s", buf.String())
	}
}

func TestDeploy_RollbackIsFailure(t *testing.T) {
	c := &fakeCFClient{
		describeResponses: []describeResponse{
			{err: &validationErr{code: "ValidationError", msg: "Stack does not exist"}},
			{status: cftypes.StackStatusCreateInProgress},
			{status: cftypes.StackStatusRollbackComplete, reason: "resource creation cancelled"},
		},
	}
	var buf bytes.Buffer
	err := Deploy(context.Background(), c, baseReq(), &buf)
	if err == nil {
		t.Fatal("expected error on rollback, got nil")
	}
	if !strings.Contains(err.Error(), "ROLLBACK_COMPLETE") {
		t.Errorf("error %q missing status reference", err.Error())
	}
}

func TestDeploy_PollInterval(t *testing.T) {
	c := &fakeCFClient{
		describeResponses: []describeResponse{
			{err: &validationErr{code: "ValidationError", msg: "does not exist"}},
			{status: cftypes.StackStatusCreateInProgress},
			{status: cftypes.StackStatusCreateInProgress},
			{status: cftypes.StackStatusCreateComplete},
		},
	}
	req := baseReq()
	req.PollInterval = 10 * time.Millisecond
	start := time.Now()
	var buf bytes.Buffer
	if err := Deploy(context.Background(), c, req, &buf); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	elapsed := time.Since(start)
	// 3 post-create Describe calls → 2 sleeps of 10ms between them → ≥ 20ms.
	// Use a slightly generous floor of 15ms to avoid flakes on fast clocks.
	if elapsed < 15*time.Millisecond {
		t.Errorf("expected elapsed ≥ 15ms (two 10ms waits), got %s", elapsed)
	}
}

func TestIsNotExistError_StringFallback(t *testing.T) {
	if !isNotExistError(errors.New("ValidationError: Stack with id foo does not exist")) {
		t.Error("expected string fallback to match does-not-exist")
	}
	if isNotExistError(errors.New("unrelated error")) {
		t.Error("expected unrelated error not to match")
	}
}

func TestIsFailureStatus(t *testing.T) {
	cases := []struct {
		s    cftypes.StackStatus
		want bool
	}{
		{cftypes.StackStatusCreateComplete, false},
		{cftypes.StackStatusUpdateComplete, false},
		{cftypes.StackStatusCreateInProgress, false},
		{cftypes.StackStatusUpdateCompleteCleanupInProgress, false},
		{cftypes.StackStatusCreateFailed, true},
		{cftypes.StackStatusRollbackComplete, true},
		{cftypes.StackStatusUpdateRollbackComplete, true},
		{cftypes.StackStatusRollbackInProgress, true},
	}
	for _, c := range cases {
		if got := isFailureStatus(c.s); got != c.want {
			t.Errorf("isFailureStatus(%s) = %v, want %v", c.s, got, c.want)
		}
	}
}
