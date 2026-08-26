package packrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
)

type testHandler struct {
	cancelled atomic.Bool
}

func (*testHandler) Capability() domain.PackCapability {
	return domain.PackCapability{Language: "test", ProtocolVersion: domain.ProtocolVersion}
}

func (h *testHandler) Analyze(ctx context.Context, _ AnalyzeParams) (AnalyzeResult, error) {
	<-ctx.Done()
	h.cancelled.Store(true)
	return AnalyzeResult{}, ctx.Err()
}

func (*testHandler) Probe(context.Context, ProbeParams) (ProbeResult, error) {
	return ProbeResult{}, nil
}

func TestInitializeRejectsVersionMismatch(t *testing.T) {
	handler := &testHandler{}
	params, _ := json.Marshal(InitializeParams{ProtocolVersion: "old"})
	_, rpcErr := dispatch(context.Background(), handler, RawRequest{Method: "initialize", Params: params})
	if rpcErr == nil || rpcErr.Code != -32001 {
		t.Fatalf("expected version mismatch, got %#v", rpcErr)
	}
}

func TestCancelCancelsInflightRequest(t *testing.T) {
	analyze, _ := json.Marshal(Request{JSONRPC: "2.0", ID: 11, Method: "analyze", Params: AnalyzeParams{BudgetMS: 10_000}})
	cancel, _ := json.Marshal(Request{JSONRPC: "2.0", ID: 12, Method: "cancel", Params: CancelParams{RequestID: 11}})
	input := append(append(analyze, '\n'), append(cancel, '\n')...)
	var output bytes.Buffer
	handler := &testHandler{}
	if err := Serve(context.Background(), bytes.NewReader(input), &output, handler); err != nil {
		t.Fatal(err)
	}
	if !handler.cancelled.Load() {
		t.Fatal("analyze context was not cancelled")
	}
	decoder := json.NewDecoder(&output)
	responses := map[int64]Response{}
	for {
		var response Response
		if err := decoder.Decode(&response); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		responses[response.ID] = response
	}
	if responses[12].Error != nil || !bytes.Contains(responses[12].Result, []byte(`"cancelled":true`)) {
		t.Fatalf("unexpected cancel response: %#v", responses[12])
	}
	if responses[11].Error == nil {
		t.Fatal("cancelled analyze request must return an error status")
	}
}

func TestBudgetContextExpires(t *testing.T) {
	ctx, cancel := budgetContext(context.Background(), 1)
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("unexpected context error: %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("budget context did not expire")
	}
}
