package hooksdk

import (
	"context"
	"testing"
	"time"
)

type dummyHandler struct {
	appendStr string
}

func (d *dummyHandler) Execute(ctx context.Context, hctx *HookContext) (*HookResult, error) {
	return &HookResult{
		ModifiedPayload: hctx.Payload + d.appendStr,
	}, nil
}

func TestHookDispatch(t *testing.T) {
	reg := NewRegistry()
	reg.Register(StagePreTurn, &dummyHandler{appendStr: " [step 1]"})
	reg.Register(StagePreTurn, &dummyHandler{appendStr: " [step 2]"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	hctx := &HookContext{
		SessionID: "sess-1",
		Stage:     StagePreTurn,
		Payload:   "input",
	}

	res, err := reg.Dispatch(ctx, hctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "input [step 1] [step 2]"
	if res.ModifiedPayload != expected {
		t.Fatalf("expected '%s', got '%s'", expected, res.ModifiedPayload)
	}
}
