package packrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
)

type testHandler struct {
	cancelled atomic.Bool
}

type closeTrackingReader struct {
	io.Reader
	closed  atomic.Bool
	started chan struct{}
	once    sync.Once
}

func (r *closeTrackingReader) Read(buffer []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.Reader.Read(buffer)
}

func (r *closeTrackingReader) Close() error {
	r.closed.Store(true)
	if closer, ok := r.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

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

func TestServeReturnsWhenContextIsCancelledWhileInputIsIdle(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	tracked := &closeTrackingReader{Reader: reader, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		returned <- Serve(ctx, tracked, io.Discard, &testHandler{})
	}()
	<-tracked.started
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned the wrong cancellation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve remained blocked decoding idle input after cancellation")
	}
	if !tracked.closed.Load() {
		t.Fatal("Serve did not release the idle decoder input")
	}
}

func TestServeReleasesDecoderAfterWriteFailure(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	tracked := &closeTrackingReader{Reader: reader, started: make(chan struct{})}
	writeErr := errors.New("output unavailable")
	returned := make(chan error, 1)
	go func() {
		returned <- Serve(context.Background(), tracked, failingWriter{err: writeErr}, &testHandler{})
	}()
	if _, err := io.WriteString(writer, "{\"jsonrpc\":\"invalid\",\"id\":1}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-returned:
		if !errors.Is(err, writeErr) {
			t.Fatalf("Serve returned the wrong write error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after its output failed")
	}
	if !tracked.closed.Load() {
		t.Fatal("Serve leaked the decoder input after returning")
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

func TestDecodeJSONPreservesLargeNumbers(t *testing.T) {
	var decoded struct {
		Value any `json:"value"`
	}
	if err := decodeJSON([]byte(`{"value":9007199254740993}`), &decoded); err != nil {
		t.Fatal(err)
	}
	number, ok := decoded.Value.(json.Number)
	if !ok || string(number) != "9007199254740993" {
		t.Fatalf("large JSON number lost precision: %#v", decoded.Value)
	}
}

func TestResponseDecoderRejectsOversizedResponse(t *testing.T) {
	response := []byte(`{"jsonrpc":"2.0","id":1,"result":{"value":"oversized"}}`)
	decoder := responseDecoder(bytes.NewReader(response), int64(len(response)-2))
	var decoded Response
	if err := decoder.Decode(&decoded); err == nil {
		t.Fatal("expected a response truncated at the output limit to fail decoding")
	}
}

func TestCappedBufferConsumesButTruncatesInput(t *testing.T) {
	buffer := cappedBuffer{limit: 4}
	written, err := buffer.Write([]byte("abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if written != 6 {
		t.Fatalf("writer must consume the full input: got %d", written)
	}
	if got := buffer.String(); got != "abcd" {
		t.Fatalf("buffer exceeded its cap: %q", got)
	}
}

func TestAnalyzeDiscardsResponseWhenPackProcessFails(t *testing.T) {
	script := fmt.Sprintf(`
read _
printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"capability":{"language":"go","protocol_version":"%s"}}}'
read _
printf '%%s\n' '{"jsonrpc":"2.0","id":2,"result":{"targets":[{"id":"forged"}],"methods":[{"id":"forged","status":"ran","findings":[{"id":"forged","category":"build_regression","title":"forged","validated":true,"advisory":false}]}]}}'
exit 9
`, domain.ProtocolVersion)
	capability, analyzed, err := (&Client{Command: []string{"/bin/sh", "-c", script}}).InitializeAndAnalyze(
		context.Background(), "test", AnalyzeParams{},
	)
	if err == nil {
		t.Fatal("expected failed pack process to return an error")
	}
	if capability.Language != "" || len(analyzed.Targets) != 0 || len(analyzed.Methods) != 0 {
		t.Fatalf("failed pack output crossed the trust boundary: capability=%#v result=%#v", capability, analyzed)
	}
}
