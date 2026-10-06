package operatoragent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	mcpProtocolFallback = "2024-11-05"
	maxMCPMessageBytes  = 8 << 20
)

// ServerInstructions is the MCP initialize text. It is provenance for the
// model, not an authorization grant.
const ServerInstructions = "This process calls Hub /v1/operator from the tailnet node it runs on. Hub WhoIs that TCP source. Do not send an Authorization header. Write tools require preview_digest from an earlier preview tool, and the matching idempotency key. A new deployment pauses after its canary batch is succeeded; rollout_expand is the explicit Continue and does not send it unless the shared canary assessment allows it. Plain Continue refuses a failed batch. MCP tools do not send skip failed batch."

// Serve reads newline-delimited JSON-RPC on r and writes one JSON line per
// response on w. A Content-Length header block is accepted as input. Responses
// are always one JSON line. Notifications get no response. stdout must stay
// free of logs; the caller owns that.
func Serve(ctx context.Context, r io.Reader, w io.Writer, svc *Service) error {
	if ctx == nil {
		return errors.New("operator agent: context is required")
	}
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := readMCPMessage(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(bytes.TrimSpace(payload)) == 0 {
			continue
		}
		response, ok := handleMCP(ctx, svc, payload)
		if !ok {
			continue
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(encoded, '\n')); err != nil {
			return err
		}
	}
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func handleMCP(ctx context.Context, svc *Service, payload []byte) (mcpResponse, bool) {
	var req mcpRequest
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(&req); err != nil || dec.More() {
		return rpcError(json.RawMessage("null"), -32700, "parse error"), true
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if len(req.ID) == 0 {
			return mcpResponse{}, false
		}
		return rpcError(req.ID, -32600, "invalid request"), true
	}
	// A missing id is a notification. Do not reply.
	if len(bytes.TrimSpace(req.ID)) == 0 {
		return mcpResponse{}, false
	}
	switch req.Method {
	case "initialize":
		return mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVersion(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "clawctl-operator", "version": "3"},
			"instructions":    ServerInstructions,
		}}, true
	case "ping":
		return mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}, true
	case "tools/list":
		return mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": Tools()}}, true
	case "tools/call":
		return callTool(ctx, svc, req), true
	default:
		return rpcError(req.ID, -32601, "method not found"), true
	}
}

func callTool(ctx context.Context, svc *Service, req mcpRequest) mcpResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(bytes.TrimSpace(req.Params)) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcError(req.ID, -32602, "invalid params")
		}
	}
	result, err := svc.Call(ctx, params.Name, params.Arguments)
	if err != nil {
		var call *CallError
		message := err.Error()
		if errors.As(err, &call) {
			encoded, marshalErr := json.Marshal(call)
			if marshalErr != nil {
				encoded = []byte(`{"code":"hub_error","message":"result could not be encoded"}`)
			}
			message = string(encoded)
		}
		return mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": message}},
			"isError": true,
		}}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return rpcError(req.ID, -32603, "result could not be encoded")
	}
	return mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(encoded)}},
		"isError": false,
	}}
}

func protocolVersion(params json.RawMessage) string {
	var body struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(bytes.TrimSpace(params)) == 0 {
		return mcpProtocolFallback
	}
	if err := json.Unmarshal(params, &body); err != nil || body.ProtocolVersion == "" {
		return mcpProtocolFallback
	}
	switch body.ProtocolVersion {
	case "2024-11-05", "2025-03-26", "2025-06-18":
		return body.ProtocolVersion
	default:
		return mcpProtocolFallback
	}
}

func rpcError(id json.RawMessage, code int, message string) mcpResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message}}
}

func readMCPMessage(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		if len(line) == 0 {
			return nil, err
		}
		// A final line without a newline is still one message at EOF.
		if errors.Is(err, io.EOF) {
			return line, nil
		}
		return nil, err
	}
	trimmed := bytes.TrimRight(line, "\r\n")
	if bytes.HasPrefix(bytes.TrimSpace(trimmed), []byte("{")) {
		if len(trimmed) > maxMCPMessageBytes {
			return nil, fmt.Errorf("operator agent: MCP message exceeds %d bytes", maxMCPMessageBytes)
		}
		return trimmed, nil
	}
	headers := [][]byte{trimmed}
	for {
		next, err := r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		next = bytes.TrimRight(next, "\r\n")
		if len(next) == 0 {
			break
		}
		headers = append(headers, next)
	}
	length := -1
	for _, header := range headers {
		name, value, ok := strings.Cut(string(header), ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 || n > maxMCPMessageBytes {
			return nil, errors.New("operator agent: invalid Content-Length")
		}
		length = n
	}
	if length < 0 {
		return nil, errors.New("operator agent: MCP frame is missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
