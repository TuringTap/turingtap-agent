// Package mcp runs a local SSE MCP server on 127.0.0.1:7847. It exposes two
// agent-local tools (agent_status, agent_lan_test) and forwards everything
// else to the cloud mcp-gateway so an AI client can point at a single
// endpoint and get both local and cloud capabilities.
//
// Transport is MCP-over-SSE: client GET /sse receives an `endpoint` event with
// the POST path, then JSON-RPC 2.0 messages as `data:` events; client POSTs
// requests to that path.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/turingtap/agent/internal/config"
)

// Tunnel is the subset of tunnel.Tunnel this package needs.
type Tunnel interface {
	Online() bool
	LANTest(host string, port int) error
}

// Upstream forwards non-local JSON-RPC requests to the cloud mcp-gateway.
// The default implementation is CloudUpstream; tests use a fake.
type Upstream interface {
	// Call sends a JSON-RPC request upstream and returns the raw result
	// (or a JSON-RPC error).
	Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *rpcError)
	// Tools returns the cloud tool definitions to merge into tools/list.
	Tools(ctx context.Context) []Tool
}

// Server is the local MCP server.
type Server struct {
	cfg  *config.Config
	tun  Tunnel
	up   Upstream
	http *http.Server

	mu    sync.Mutex
	conns map[string]chan []byte
}

// New builds the server. If up is nil a CloudUpstream is created from cfg.
func New(cfg *config.Config, tun Tunnel, up Upstream) *Server {
	if up == nil {
		up = NewCloudUpstream(cfg.MCPGatewayURL, cfg.APIKey)
	}
	s := &Server{cfg: cfg, tun: tun, up: up, conns: map[string]chan []byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sse", s.handleSSE)
	mux.HandleFunc("POST /messages", s.handlePost)
	s.http = &http.Server{Addr: cfg.LocalMCPAddr, Handler: mux}
	return s
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.http.Shutdown(sctx)
	}()
	slog.Info("mcp: local SSE server listening", "addr", s.cfg.LocalMCPAddr)
	err := s.http.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// --- SSE transport ---------------------------------------------------------

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sid := fmt.Sprintf("%d", time.Now().UnixNano())
	ch := make(chan []byte, 32)
	s.mu.Lock()
	s.conns[sid] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, sid)
		s.mu.Unlock()
	}()

	fmt.Fprintf(w, "event: endpoint\ndata: /messages?session=%s\n\n", sid)
	fl.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			fl.Flush()
		}
	}
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	s.mu.Lock()
	ch, ok := s.conns[sid]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	go func() {
		resp := s.dispatch(context.Background(), &req)
		if resp == nil {
			return // notification
		}
		b, _ := json.Marshal(resp)
		ch <- b
	}()
	w.WriteHeader(http.StatusAccepted)
}

// --- JSON-RPC dispatch -----------------------------------------------------

func (s *Server) dispatch(ctx context.Context, req *rpcRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		return ok(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "turingtap-agent", "version": "0.1.0"},
		})
	case "notifications/initialized":
		return nil
	case "tools/list":
		tools := append(localTools(), s.up.Tools(ctx)...)
		return ok(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		return s.callTool(ctx, req)
	default:
		res, e := s.up.Call(ctx, req.Method, req.Params)
		if e != nil {
			return fail(req.ID, e)
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res}
	}
}

func (s *Server) callTool(ctx context.Context, req *rpcRequest) *rpcResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return fail(req.ID, &rpcError{Code: -32602, Message: err.Error()})
	}

	switch p.Name {
	case "agent_status":
		out := map[string]any{
			"online":          s.tun.Online(),
			"relay_url":       s.cfg.RelayURL,
			"proxy_url":       s.cfg.ProxyURL,
			"lan_allow_cidrs": s.cfg.LANAllowCIDRs,
			"local_mcp_addr":  s.cfg.LocalMCPAddr,
		}
		return ok(req.ID, toolResult(out, false))

	case "agent_lan_test":
		var a struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}
		if err := json.Unmarshal(p.Arguments, &a); err != nil {
			return ok(req.ID, toolResult(map[string]any{"error": err.Error()}, true))
		}
		err := s.tun.LANTest(a.Host, a.Port)
		out := map[string]any{"host": a.Host, "port": a.Port, "ok": err == nil}
		if err != nil {
			out["error"] = err.Error()
		}
		return ok(req.ID, toolResult(out, err != nil))

	default:
		res, e := s.up.Call(ctx, "tools/call", req.Params)
		if e != nil {
			return fail(req.ID, e)
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res}
	}
}

func toolResult(v any, isErr bool) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
		"isError": isErr,
	}
}

// --- Tool schemas ----------------------------------------------------------

// Tool is an MCP tool definition.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func localTools() []Tool {
	return []Tool{
		{
			Name:        "agent_status",
			Description: "Report turingtap-agent connectivity: relay online, proxy URL, LAN allow-list.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "agent_lan_test",
			Description: "Attempt a TCP dial to host:port through the reverse-SOCKS allow-list gate.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host": map[string]any{"type": "string"},
					"port": map[string]any{"type": "integer"},
				},
				"required": []string{"host", "port"},
			},
		},
	}
}

// --- JSON-RPC types --------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func ok(id any, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}
func fail(id any, e *rpcError) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: e}
}
