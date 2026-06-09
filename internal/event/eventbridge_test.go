package event

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
)

type fakeEB struct {
	called  bool
	source  string
	detail  string
	busName string
	dtype   string
}

func (f *fakeEB) PutEvents(_ context.Context, in *eventbridge.PutEventsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error) {
	f.called = true
	e := in.Entries[0]
	f.source = *e.Source
	f.detail = *e.Detail
	f.busName = *e.EventBusName
	f.dtype = *e.DetailType
	return &eventbridge.PutEventsOutput{FailedEntryCount: 0}, nil
}

func TestEventBridgeEmitPutsEntry(t *testing.T) {
	f := &fakeEB{}
	e := &EventBridgeEmitter{client: f, busName: "horde-proj"}
	err := e.Emit(context.Background(), TypeRunTerminal, Detail{RunID: "r1", Status: "success"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.called {
		t.Fatal("PutEvents not called")
	}
	if f.source != Source {
		t.Errorf("source = %q, want %q", f.source, Source)
	}
	if f.busName != "horde-proj" {
		t.Errorf("busName = %q, want horde-proj", f.busName)
	}
	if f.dtype != TypeRunTerminal {
		t.Errorf("detailType = %q, want %q", f.dtype, TypeRunTerminal)
	}
	if f.detail == "" {
		t.Error("detail JSON empty")
	}
}
