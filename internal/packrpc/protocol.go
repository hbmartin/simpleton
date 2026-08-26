package packrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/gitx"
)

type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type RawRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("pack RPC %d: %s", e.Code, e.Message) }

type InitializeParams struct {
	ProtocolVersion string `json:"protocol_version"`
	CoreVersion     string `json:"core_version"`
}

type InitializeResult struct {
	Capability domain.PackCapability `json:"capability"`
}

type AnalyzeParams struct {
	ProtocolVersion   string             `json:"protocol_version"`
	Repository        string             `json:"repository"`
	BaseRevision      string             `json:"base_revision"`
	HeadRevision      string             `json:"head_revision"`
	ChangedFiles      []gitx.ChangedFile `json:"changed_files"`
	ContractDigest    string             `json:"contract_digest,omitempty"`
	EnvironmentDigest string             `json:"environment_digest"`
	Seed              string             `json:"seed"`
	BudgetMS          int64              `json:"budget_ms"`
}

type AnalyzeResult struct {
	Targets       []domain.VerificationTarget `json:"targets"`
	Methods       []domain.MethodResult       `json:"methods"`
	Opportunities []domain.Opportunity        `json:"opportunities"`
}

type ProbeParams struct {
	ProtocolVersion string                    `json:"protocol_version"`
	Target          domain.VerificationTarget `json:"target"`
	Observation     domain.ObservationSpec    `json:"observation"`
	BudgetMS        int64                     `json:"budget_ms"`
	Seed            string                    `json:"seed"`
}

type ProbeResult struct {
	Method      domain.MethodResult         `json:"method"`
	Divergences []domain.ObservedDivergence `json:"divergences"`
	Capsules    []domain.ReplayCapsule      `json:"replay_capsules"`
}

type CancelParams struct {
	RequestID int64 `json:"request_id"`
}

type CancelResult struct {
	Cancelled bool `json:"cancelled"`
}

type Client struct {
	Command []string
	nextID  atomic.Int64
}

const (
	maxPackOutputBytes = 32 << 20
	maxPackStderrBytes = 1 << 20
)

func responseDecoder(reader io.Reader, limit int64) *json.Decoder {
	return json.NewDecoder(bufio.NewReader(io.LimitReader(reader, limit)))
}

type cappedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.buffer.Write(data)
	}
	return written, nil
}

func (b *cappedBuffer) String() string { return b.buffer.String() }

func (c *Client) InitializeAndAnalyze(ctx context.Context, coreVersion string, params AnalyzeParams) (domain.PackCapability, AnalyzeResult, error) {
	var analyzed AnalyzeResult
	capability, err := c.initializeAndCall(ctx, coreVersion, "analyze", params, &analyzed)
	if err != nil {
		return domain.PackCapability{}, AnalyzeResult{}, err
	}
	return capability, analyzed, err
}

func (c *Client) InitializeAndProbe(ctx context.Context, coreVersion string, params ProbeParams) (domain.PackCapability, ProbeResult, error) {
	var probed ProbeResult
	capability, err := c.initializeAndCall(ctx, coreVersion, "probe", params, &probed)
	if err != nil {
		return domain.PackCapability{}, ProbeResult{}, err
	}
	return capability, probed, err
}

func (c *Client) initializeAndCall(ctx context.Context, coreVersion, method string, params, output any) (domain.PackCapability, error) {
	if len(c.Command) == 0 {
		return domain.PackCapability{}, errors.New("pack command is empty")
	}
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return domain.PackCapability{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return domain.PackCapability{}, err
	}
	stderr := cappedBuffer{limit: maxPackStderrBytes}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return domain.PackCapability{}, err
	}
	enc := json.NewEncoder(stdin)
	dec := responseDecoder(stdout, maxPackOutputBytes)
	var initialized InitializeResult
	if err := c.call(enc, dec, "initialize", InitializeParams{ProtocolVersion: domain.ProtocolVersion, CoreVersion: coreVersion}, &initialized); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return domain.PackCapability{}, withStderr(err, stderr.String())
	}
	if initialized.Capability.ProtocolVersion != domain.ProtocolVersion {
		_ = stdin.Close()
		_ = cmd.Wait()
		return domain.PackCapability{}, fmt.Errorf("pack protocol version %q does not match core %q", initialized.Capability.ProtocolVersion, domain.ProtocolVersion)
	}
	if err := c.call(enc, dec, method, params, output); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return initialized.Capability, withStderr(err, stderr.String())
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Wait()
		return initialized.Capability, err
	}
	if err := cmd.Wait(); err != nil {
		return initialized.Capability, withStderr(err, stderr.String())
	}
	return initialized.Capability, nil
}

func (c *Client) call(enc *json.Encoder, dec *json.Decoder, method string, params any, out any) error {
	id := c.nextID.Add(1)
	if err := enc.Encode(Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	var response Response
	if err := dec.Decode(&response); err != nil {
		return err
	}
	if response.JSONRPC != "2.0" || response.ID != id {
		return errors.New("malformed or mismatched JSON-RPC response")
	}
	if response.Error != nil {
		return response.Error
	}
	if len(response.Result) == 0 {
		return errors.New("JSON-RPC response has no result")
	}
	return decodeJSON(response.Result, out)
}

func decodeJSON(data []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(output)
}

func withStderr(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

type Handler interface {
	Capability() domain.PackCapability
	Analyze(context.Context, AnalyzeParams) (AnalyzeResult, error)
	Probe(context.Context, ProbeParams) (ProbeResult, error)
}

type decodedRequest struct {
	request RawRequest
	err     error
}

func Serve(ctx context.Context, in io.Reader, out io.Writer, handler Handler) error {
	dec := json.NewDecoder(bufio.NewReader(in))
	enc := json.NewEncoder(out)
	decoded := make(chan decodedRequest)
	go func() {
		defer close(decoded)
		for {
			var request RawRequest
			err := dec.Decode(&request)
			select {
			case decoded <- decodedRequest{request: request, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var writeMu sync.Mutex
	var inflightMu sync.Mutex
	inflight := map[int64]context.CancelFunc{}
	var workers sync.WaitGroup
	defer func() {
		inflightMu.Lock()
		for _, cancel := range inflight {
			cancel()
		}
		inflightMu.Unlock()
		workers.Wait()
	}()
	write := func(response Response) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return enc.Encode(response)
	}
	for {
		var request RawRequest
		select {
		case <-ctx.Done():
			if closer, ok := in.(io.Closer); ok {
				_ = closer.Close()
			}
			return ctx.Err()
		case item, ok := <-decoded:
			if !ok {
				return nil
			}
			if item.err != nil {
				if errors.Is(item.err, io.EOF) {
					return nil
				}
				return item.err
			}
			request = item.request
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if request.JSONRPC != "2.0" {
			if err := write(Response{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32600, Message: "invalid JSON-RPC request"}}); err != nil {
				return err
			}
			continue
		}
		if request.Method == "cancel" {
			var params CancelParams
			if err := decodeJSON(request.Params, &params); err != nil {
				if err := write(Response{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32602, Message: err.Error()}}); err != nil {
					return err
				}
				continue
			}
			inflightMu.Lock()
			cancel, found := inflight[params.RequestID]
			if found {
				cancel()
			}
			inflightMu.Unlock()
			encoded, _ := json.Marshal(CancelResult{Cancelled: found})
			if err := write(Response{JSONRPC: "2.0", ID: request.ID, Result: encoded}); err != nil {
				return err
			}
			continue
		}
		requestCtx, cancel := context.WithCancel(ctx)
		inflightMu.Lock()
		inflight[request.ID] = cancel
		inflightMu.Unlock()
		workers.Add(1)
		go func(request RawRequest) {
			defer workers.Done()
			defer cancel()
			response := Response{JSONRPC: "2.0", ID: request.ID}
			result, rpcErr := dispatch(requestCtx, handler, request)
			if rpcErr != nil {
				response.Error = rpcErr
			} else {
				encoded, err := json.Marshal(result)
				if err != nil {
					response.Error = &RPCError{Code: -32603, Message: err.Error()}
				} else {
					response.Result = encoded
				}
			}
			inflightMu.Lock()
			delete(inflight, request.ID)
			inflightMu.Unlock()
			_ = write(response)
		}(request)
	}
}

func dispatch(ctx context.Context, handler Handler, request RawRequest) (any, *RPCError) {
	switch request.Method {
	case "initialize":
		var params InitializeParams
		if err := decodeJSON(request.Params, &params); err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}
		capability := handler.Capability()
		if params.ProtocolVersion != capability.ProtocolVersion {
			return nil, &RPCError{Code: -32001, Message: "protocol version mismatch"}
		}
		return InitializeResult{Capability: capability}, nil
	case "analyze":
		var params AnalyzeParams
		if err := decodeJSON(request.Params, &params); err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}
		deadlineCtx, cancel := budgetContext(ctx, params.BudgetMS)
		defer cancel()
		result, err := handler.Analyze(deadlineCtx, params)
		if err != nil {
			return nil, &RPCError{Code: -32002, Message: err.Error()}
		}
		return result, nil
	case "probe":
		var params ProbeParams
		if err := decodeJSON(request.Params, &params); err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}
		deadlineCtx, cancel := budgetContext(ctx, params.BudgetMS)
		defer cancel()
		result, err := handler.Probe(deadlineCtx, params)
		if err != nil {
			return nil, &RPCError{Code: -32003, Message: err.Error()}
		}
		return result, nil
	default:
		return nil, &RPCError{Code: -32601, Message: "method not found"}
	}
}

func budgetContext(parent context.Context, budgetMS int64) (context.Context, context.CancelFunc) {
	if budgetMS <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(budgetMS)*time.Millisecond)
}
