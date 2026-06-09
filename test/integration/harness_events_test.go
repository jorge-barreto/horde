package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/jorge-barreto/horde/internal/event"
)

// eventCapture is a transient EventBridge→SQS tap for the #36 event-backbone
// e2e test. It creates a temp SQS queue and an EventBridge rule on the horde
// bus matching source=["horde"], routing all horde events to the queue. The
// test then launches a run and polls the queue for the run's events. All
// resources are torn down via t.Cleanup (LIFO: targets → rule → queue).
type eventCapture struct {
	t        *testing.T
	ctx      context.Context
	sqs      *sqs.Client
	eb       *eventbridge.Client
	queueURL string
	queueArn string
	ruleName string
	busName  string
}

// envelope is the EventBridge → SQS message body shape: the event metadata
// plus the nested horde Detail.
type envelope struct {
	DetailType string       `json:"detail-type"`
	Source     string       `json:"source"`
	Detail     event.Detail `json:"detail"`
}

// newEventCapture wires the tap. busName is the custom bus (from
// hc.EventBusName). nonce makes resource names unique per test invocation.
func newEventCapture(t *testing.T, awsCfg aws.Config, busName, nonce string) *eventCapture {
	t.Helper()
	if busName == "" {
		t.Fatal("newEventCapture: empty bus name (SSM config has no event_bus_name?)")
	}
	ctx := context.Background()
	c := &eventCapture{
		t:        t,
		ctx:      ctx,
		sqs:      sqs.NewFromConfig(awsCfg),
		eb:       eventbridge.NewFromConfig(awsCfg),
		ruleName: "horde-e2e-evt-" + nonce,
		busName:  busName,
	}

	// 1. Create the SQS queue.
	queueName := "horde-e2e-evt-" + nonce
	cq, err := c.sqs.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(queueName)})
	if err != nil {
		t.Fatalf("eventCapture: CreateQueue(%s): %v", queueName, err)
	}
	c.queueURL = aws.ToString(cq.QueueUrl)

	// 2. Read the queue ARN.
	ga, err := c.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(c.queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatalf("eventCapture: GetQueueAttributes: %v", err)
	}
	c.queueArn = ga.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]

	// 3. Create the EventBridge rule on the horde bus.
	pr, err := c.eb.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:         aws.String(c.ruleName),
		EventBusName: aws.String(busName),
		EventPattern: aws.String(`{"source":["` + event.Source + `"]}`),
		State:        ebtypes.RuleStateEnabled,
	})
	if err != nil {
		t.Fatalf("eventCapture: PutRule: %v", err)
	}
	ruleArn := aws.ToString(pr.RuleArn)

	// 4. Grant events.amazonaws.com permission to send to the queue. WITHOUT
	// this policy, PutTargets succeeds but EventBridge silently fails to
	// deliver — the classic event→SQS drop. Scope to the rule ARN.
	policy := fmt.Sprintf(`{
      "Version": "2012-10-17",
      "Statement": [{
        "Effect": "Allow",
        "Principal": {"Service": "events.amazonaws.com"},
        "Action": "sqs:SendMessage",
        "Resource": "%s",
        "Condition": {"ArnEquals": {"aws:SourceArn": "%s"}}
      }]
    }`, c.queueArn, ruleArn)
	if _, err := c.sqs.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   aws.String(c.queueURL),
		Attributes: map[string]string{string(sqstypes.QueueAttributeNamePolicy): policy},
	}); err != nil {
		t.Fatalf("eventCapture: SetQueueAttributes(policy): %v", err)
	}

	// 5. Point the rule at the queue.
	pt, err := c.eb.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule:         aws.String(c.ruleName),
		EventBusName: aws.String(busName),
		Targets:      []ebtypes.Target{{Id: aws.String("sqs"), Arn: aws.String(c.queueArn)}},
	})
	if err != nil {
		t.Fatalf("eventCapture: PutTargets: %v", err)
	}
	if pt.FailedEntryCount > 0 {
		t.Fatalf("eventCapture: PutTargets reported %d failed entries: %+v", pt.FailedEntryCount, pt.FailedEntries)
	}

	// 6. Teardown (LIFO): RemoveTargets → DeleteRule → DeleteQueue. Best-effort.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := c.eb.RemoveTargets(cctx, &eventbridge.RemoveTargetsInput{
			Rule: aws.String(c.ruleName), EventBusName: aws.String(busName), Ids: []string{"sqs"},
		}); err != nil {
			t.Logf("eventCapture cleanup: RemoveTargets: %v", err)
		}
		if _, err := c.eb.DeleteRule(cctx, &eventbridge.DeleteRuleInput{
			Name: aws.String(c.ruleName), EventBusName: aws.String(busName),
		}); err != nil {
			t.Logf("eventCapture cleanup: DeleteRule: %v", err)
		}
		if _, err := c.sqs.DeleteQueue(cctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(c.queueURL)}); err != nil {
			t.Logf("eventCapture cleanup: DeleteQueue: %v", err)
		}
	})

	return c
}

// DetailsFor long-polls the queue up to timeout and returns every captured
// Detail whose RunID matches runID. Messages are deleted as they are read.
// Non-matching events (from other parallel tests sharing the bus) are also
// drained + deleted so they don't accumulate.
func (c *eventCapture) DetailsFor(runID string, timeout time.Duration) []event.Detail {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var matched []event.Detail
	for time.Now().Before(deadline) {
		out, err := c.sqs.ReceiveMessage(c.ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			c.t.Logf("eventCapture.DetailsFor: ReceiveMessage (will retry): %v", err)
			continue
		}
		for _, m := range out.Messages {
			var env envelope
			if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &env); err != nil {
				c.t.Logf("eventCapture.DetailsFor: bad envelope (skipping): %v\nbody: %s", err, aws.ToString(m.Body))
			} else if env.Detail.RunID == runID {
				c.t.Logf("eventCapture: captured %q for run %s (status=%q)", env.DetailType, runID, env.Detail.Status)
				matched = append(matched, env.Detail)
			}
			// Delete every message we received (matching or not) so the queue
			// drains and parallel-test noise doesn't pile up.
			if _, derr := c.sqs.DeleteMessage(c.ctx, &sqs.DeleteMessageInput{
				QueueUrl: aws.String(c.queueURL), ReceiptHandle: m.ReceiptHandle,
			}); derr != nil {
				c.t.Logf("eventCapture.DetailsFor: DeleteMessage: %v", derr)
			}
		}
		// Once we have both a started + terminal, we can stop early.
		if hasEventStatuses(matched) {
			break
		}
	}
	return matched
}

// hasEventStatuses reports whether the captured details include both a
// run.started-like (status running/pending) and a terminal status.
func hasEventStatuses(details []event.Detail) bool {
	started, terminal := false, false
	for _, d := range details {
		switch d.Status {
		case "running", "pending":
			started = true
		case "success", "failed", "killed", "timed_out", "cancelled":
			terminal = true
		}
	}
	return started && terminal
}
