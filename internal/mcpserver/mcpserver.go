// Package mcpserver exposes a minimal MCP (Model Context Protocol) server
// over the streamable-HTTP transport, so MCP-capable coding clients
// (Claude Code, Codex) can push progress updates and artifacts directly
// into the running turn. Output capture (Steps 3-6) remains the baseline
// transport; this channel is an enhancement, never a requirement.
//
// Scope is deliberately tiny and stdlib-only:
//   - one loopback HTTP listener shared by all turns,
//   - one turn-scoped bearer token per channel (revoked when the turn
//     ends), mapping the request to that turn's Sink,
//   - JSON-RPC 2.0 over POST with plain application/json responses (the
//     spec allows a server to answer without SSE; neither tool needs
//     server-initiated messages),
//   - two tools: "progress" and "add_artifact".
package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// protocolVersion is the newest MCP revision this server implements.
// Known older revisions requested by a client are echoed back unchanged
// (the handshake is backward compatible for this server's feature set).
const protocolVersion = "2025-06-18"

var knownVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// Sink receives what a client pushes through one turn's callback channel.
type Sink struct {
	// Emit forwards a normalized event into the turn's stream (persisted
	// and shown live like any adapter event). Required.
	Emit adapter.EmitFunc
	// AddArtifact stores a workspace file in the artifact library and
	// returns the artifact ID. Optional: nil makes the add_artifact tool
	// report that artifact storage is unavailable.
	AddArtifact func(path, note string) (string, error)
}

// Server is the shared loopback MCP endpoint. Create with Start, hand
// per-turn channels out with Register, shut down with Close.
type Server struct {
	ln  net.Listener
	srv *http.Server

	mu       sync.Mutex
	channels map[string]Sink
}

// Start listens on an ephemeral loopback port and serves the MCP endpoint
// at /mcp until Close.
func Start() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcpserver: listen: %w", err)
	}
	s := &Server{ln: ln, channels: make(map[string]Sink)}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handle)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// URL returns the streamable-HTTP endpoint clients connect to.
func (s *Server) URL() string {
	return "http://" + s.ln.Addr().String() + "/mcp"
}

// Close stops the listener; outstanding channels become invalid.
func (s *Server) Close() error { return s.srv.Close() }

// Channel is one turn's registration; Close revokes its token.
type Channel struct {
	Token string
	s     *Server
}

// Register creates a turn-scoped channel whose bearer token routes tool
// calls to sink.
func (s *Server) Register(sink Sink) *Channel {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic("mcpserver: crypto/rand failed: " + err.Error()) // never in practice
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	s.channels[token] = sink
	s.mu.Unlock()
	return &Channel{Token: token, s: s}
}

// Close revokes the channel's token; later requests with it get 401.
func (c *Channel) Close() {
	c.s.mu.Lock()
	delete(c.s.channels, c.Token)
	c.s.mu.Unlock()
}

// --- JSON-RPC plumbing ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// GET would open an SSE stream for server-initiated messages; we
		// have none. The spec allows responding 405 here.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	sink, ok := s.channels[token]
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: nil,
			Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}

	// Notifications (and client-side responses, which also carry no
	// method we serve) get 202 with no body, per streamable HTTP.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = s.initialize(req.Params)
	case "ping":
		resp.Result = struct{}{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList()}
	case "tools/call":
		result, err := callTool(sink, req.Params)
		if err != nil {
			resp.Error = &rpcError{Code: -32602, Message: err.Error()}
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) initialize(params json.RawMessage) any {
	version := protocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && knownVersions[p.ProtocolVersion] {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": struct{}{}},
		"serverInfo":      map[string]any{"name": "agentchat", "version": "0.1.0"},
	}
}

// --- tools ---

func toolList() []map[string]any {
	return []map[string]any{
		{
			"name": "progress",
			"description": "Report a short progress update to the AgentChat UI. " +
				"Use it to narrate long-running work; the message appears live " +
				"in the conversation transcript.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{
						"type":        "string",
						"description": "The progress update to show.",
					},
				},
				"required": []string{"message"},
			},
		},
		{
			"name": "add_artifact",
			"description": "Save a file from the workspace into the AgentChat " +
				"artifact library, so the user can find and download it after " +
				"the turn. Use workspace-relative paths.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Workspace-relative path of the file to store.",
					},
					"note": map[string]any{
						"type":        "string",
						"description": "Optional note on what the file is.",
					},
				},
				"required": []string{"path"},
			},
		},
	}
}

// callTool dispatches tools/call. A tool-level failure is reported inside
// the result (isError: true) per MCP; the returned error is reserved for
// malformed requests.
func callTool(sink Sink, params json.RawMessage) (any, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid tools/call params: %w", err)
	}

	switch p.Name {
	case "progress":
		var args struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(p.Arguments, &args); err != nil || strings.TrimSpace(args.Message) == "" {
			return toolError("progress requires a non-empty \"message\" string"), nil
		}
		sink.Emit(adapter.Event{
			Kind: adapter.EventThinking,
			Time: time.Now(),
			Text: args.Message,
			Raw:  json.RawMessage(`{"mcp_tool":"progress"}`),
		})
		return toolText("progress recorded"), nil

	case "add_artifact":
		var args struct {
			Path string `json:"path"`
			Note string `json:"note"`
		}
		if err := json.Unmarshal(p.Arguments, &args); err != nil || strings.TrimSpace(args.Path) == "" {
			return toolError("add_artifact requires a \"path\" string"), nil
		}
		if sink.AddArtifact == nil {
			return toolError("artifact storage is not available in this session"), nil
		}
		id, err := sink.AddArtifact(args.Path, args.Note)
		if err != nil {
			return toolError("storing artifact: " + err.Error()), nil
		}
		return toolText("stored artifact " + id), nil

	default:
		return toolError("unknown tool: " + p.Name), nil
	}
}

func toolText(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
	}
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}
