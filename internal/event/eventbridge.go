package event

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

// ebClient is the subset of the EventBridge API the emitter uses (for testing).
type ebClient interface {
	PutEvents(ctx context.Context, in *eventbridge.PutEventsInput, optFns ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error)
}

// EventBridgeEmitter publishes events to a named custom EventBridge bus.
type EventBridgeEmitter struct {
	client  ebClient
	busName string
}

// NewEventBridgeEmitter constructs an emitter for the given bus.
func NewEventBridgeEmitter(cfg aws.Config, busName string) *EventBridgeEmitter {
	return &EventBridgeEmitter{client: eventbridge.NewFromConfig(cfg), busName: busName}
}

// Emit marshals the detail and PutEvents it to the bus. Returns an error if the
// marshal or the API call fails, or if EventBridge rejects the entry — callers
// treat this as best-effort and log rather than fail.
func (e *EventBridgeEmitter) Emit(ctx context.Context, detailType string, detail Detail) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshalling event detail: %w", err)
	}
	out, err := e.client.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []ebtypes.PutEventsRequestEntry{{
			EventBusName: aws.String(e.busName),
			Source:       aws.String(Source),
			DetailType:   aws.String(detailType),
			Detail:       aws.String(string(b)),
		}},
	})
	if err != nil {
		return fmt.Errorf("putting event: %w", err)
	}
	if out.FailedEntryCount > 0 {
		return fmt.Errorf("eventbridge rejected %d of 1 entries", out.FailedEntryCount)
	}
	return nil
}
