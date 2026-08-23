package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// CloudUpstream forwards JSON-RPC requests to the cloud mcp-gateway. The
// gateway exposes a POST /rpc bridge alongside its SSE endpoint for
// agent-side forwarding; this keeps the local proxy stateless.
type CloudUpstream struct {
	base   string
	apiKey string
	hc     *http.Client
}

// NewCloudUpstream constructs a forwarder.
func NewCloudUpstream(base, apiKey string) *CloudUpstream {
	return &CloudUpstream{
		base:   base,
		apiKey: apiKey,
		hc:     &http.Client{Timeout: 60 * time.Second},
	}
}

// Call POSTs a JSON-RPC request to <base>/rpc and returns the raw result.
func (c *CloudUpstream) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *rpcError) {
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: fmt.Sprintf("upstream unreachable: %v", err)}
	}
	defer resp.Body.Close()

	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return out.Result, nil
}

// Tools fetches the cloud tool list. On failure it returns nil so the local
// server still works offline.
func (c *CloudUpstream) Tools(ctx context.Context) []Tool {
	res, e := c.Call(ctx, "tools/list", nil)
	if e != nil {
		return nil
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil
	}
	return out.Tools
}

// NoopUpstream is used when no cloud gateway is configured (tests, offline).
type NoopUpstream struct{}

func (NoopUpstream) Call(context.Context, string, json.RawMessage) (json.RawMessage, *rpcError) {
	return nil, &rpcError{Code: -32001, Message: "cloud mcp-gateway not configured"}
}
func (NoopUpstream) Tools(context.Context) []Tool { return nil }
