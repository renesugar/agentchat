package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/renesugar/agentchat/internal/adapter"
)

// post sends one JSON-RPC message with the given bearer token and returns
// the HTTP response with its decoded body (nil for empty bodies).
func post(t *testing.T, url, token, body string) (*http.Response, *rpcResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return resp, nil
	}
	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return resp, &rr
}

func call(t *testing.T, url, token, method, params string) *rpcResponse {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)
	resp, rr := post(t, url, token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d, want 200", method, resp.StatusCode)
	}
	if rr == nil {
		t.Fatalf("%s: empty response body", method)
	}
	return rr
}

// resultMap re-decodes a response result into a generic map.
func resultMap(t *testing.T, rr *rpcResponse) map[string]any {
	t.Helper()
	raw, err := json.Marshal(rr.Result)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestServerLifecycle(t *testing.T) {
	srv, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	var events []adapter.Event
	var artifacts []string
	ch := srv.Register(Sink{
		Emit: func(ev adapter.Event) { events = append(events, ev) },
		AddArtifact: func(path, note string) (string, error) {
			artifacts = append(artifacts, path+"|"+note)
			return "art-1", nil
		},
	})

	// initialize negotiates a known protocol version.
	rr := call(t, srv.URL(), ch.Token, "initialize",
		`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}`)
	if rr.Error != nil {
		t.Fatalf("initialize error: %+v", rr.Error)
	}
	if got := resultMap(t, rr)["protocolVersion"]; got != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want echo of client's 2025-03-26", got)
	}

	// An unknown requested version falls back to ours.
	rr = call(t, srv.URL(), ch.Token, "initialize", `{"protocolVersion":"1999-01-01"}`)
	if got := resultMap(t, rr)["protocolVersion"]; got != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", got, protocolVersion)
	}

	// notifications/initialized has no id → 202, empty body.
	resp, rrN := post(t, srv.URL(), ch.Token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != http.StatusAccepted || rrN != nil {
		t.Errorf("notification: status %d body %v, want 202 empty", resp.StatusCode, rrN)
	}

	// tools/list exposes exactly progress, get_turns, and add_artifact.
	rr = call(t, srv.URL(), ch.Token, "tools/list", `{}`)
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	raw, _ := json.Marshal(rr.Result)
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	want := []string{"progress", "get_turns", "add_artifact"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}

	// progress lands in the sink as a thinking event.
	rr = call(t, srv.URL(), ch.Token, "tools/call",
		`{"name":"progress","arguments":{"message":"halfway there"}}`)
	if rr.Error != nil {
		t.Fatalf("progress error: %+v", rr.Error)
	}
	if len(events) != 1 || events[0].Kind != adapter.EventThinking || events[0].Text != "halfway there" {
		t.Fatalf("events = %+v, want one thinking event", events)
	}

	// add_artifact reaches the sink callback and reports the ID back.
	rr = call(t, srv.URL(), ch.Token, "tools/call",
		`{"name":"add_artifact","arguments":{"path":"out/report.pdf","note":"final report"}}`)
	if rr.Error != nil {
		t.Fatalf("add_artifact error: %+v", rr.Error)
	}
	if len(artifacts) != 1 || artifacts[0] != "out/report.pdf|final report" {
		t.Errorf("artifacts = %v", artifacts)
	}
	if m := resultMap(t, rr); m["isError"] == true {
		t.Errorf("add_artifact result flagged as error: %v", m)
	}

	// Tool-level failures: missing args and unknown tools are isError
	// results, not protocol errors.
	for _, params := range []string{
		`{"name":"progress","arguments":{}}`,
		`{"name":"no_such_tool","arguments":{}}`,
	} {
		rr = call(t, srv.URL(), ch.Token, "tools/call", params)
		if rr.Error != nil {
			t.Errorf("params %s: got protocol error %+v, want isError result", params, rr.Error)
		} else if m := resultMap(t, rr); m["isError"] != true {
			t.Errorf("params %s: result %v, want isError", params, m)
		}
	}

	// ping works; unknown methods are -32601.
	if rr = call(t, srv.URL(), ch.Token, "ping", `{}`); rr.Error != nil {
		t.Errorf("ping: %+v", rr.Error)
	}
	if rr = call(t, srv.URL(), ch.Token, "resources/list", `{}`); rr.Error == nil || rr.Error.Code != -32601 {
		t.Errorf("unknown method: %+v, want -32601", rr.Error)
	}

	// GET (SSE listen stream) is declined.
	getResp, err := http.Get(srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", getResp.StatusCode)
	}

	// Bad or revoked tokens are rejected.
	resp, _ = post(t, srv.URL(), "wrong-token", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token: status %d, want 401", resp.StatusCode)
	}
	ch.Close()
	resp, _ = post(t, srv.URL(), ch.Token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("closed channel: status %d, want 401", resp.StatusCode)
	}
}

func TestContextToolAndREST(t *testing.T) {
	srv, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	var asked []int
	ch := srv.Register(Sink{
		Emit: func(adapter.Event) {},
		Context: func(lastN int) (string, error) {
			asked = append(asked, lastN)
			return fmt.Sprintf("TRANSCRIPT lastN=%d", lastN), nil
		},
	})
	defer ch.Close()

	// MCP tool: no args = full transcript; last_n trims.
	rr := call(t, srv.URL(), ch.Token, "tools/call", `{"name":"get_turns","arguments":{}}`)
	if rr.Error != nil {
		t.Fatalf("get_turns: %+v", rr.Error)
	}
	rr = call(t, srv.URL(), ch.Token, "tools/call", `{"name":"get_turns","arguments":{"last_n":2}}`)
	if rr.Error != nil {
		t.Fatalf("get_turns last_n: %+v", rr.Error)
	}
	// Negative last_n is a tool-level error and never reaches the sink.
	rr = call(t, srv.URL(), ch.Token, "tools/call", `{"name":"get_turns","arguments":{"last_n":-1}}`)
	if m := resultMap(t, rr); m["isError"] != true {
		t.Errorf("negative last_n accepted: %v", m)
	}

	// REST twin: GET /context with the same bearer token.
	restURL := strings.Replace(srv.URL(), "/mcp", "/context", 1)
	get := func(url, token string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(restURL, ch.Token); code != 200 || body != "TRANSCRIPT lastN=0" {
		t.Errorf("GET /context = %d %q", code, body)
	}
	if code, body := get(restURL+"?last_n=3", ch.Token); code != 200 || body != "TRANSCRIPT lastN=3" {
		t.Errorf("GET /context?last_n=3 = %d %q", code, body)
	}
	if code, _ := get(restURL+"?last_n=nope", ch.Token); code != 400 {
		t.Errorf("bad last_n: status %d, want 400", code)
	}
	if code, _ := get(restURL, "wrong"); code != 401 {
		t.Errorf("bad token: status %d, want 401", code)
	}
	if wantAsked := []int{0, 2, 0, 3}; !reflect.DeepEqual(asked, wantAsked) {
		t.Errorf("sink saw lastN=%v, want %v", asked, wantAsked)
	}

	// A sink without Context: tool-level error and REST 404.
	ch2 := srv.Register(Sink{Emit: func(adapter.Event) {}})
	defer ch2.Close()
	rr = call(t, srv.URL(), ch2.Token, "tools/call", `{"name":"get_turns","arguments":{}}`)
	if m := resultMap(t, rr); m["isError"] != true {
		t.Errorf("get_turns without Context: %v", m)
	}
	if code, _ := get(restURL, ch2.Token); code != 404 {
		t.Errorf("REST without Context: status %d, want 404", code)
	}
}

func TestAddArtifactWithoutSink(t *testing.T) {
	srv, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ch := srv.Register(Sink{Emit: func(adapter.Event) {}})
	defer ch.Close()

	rr := call(t, srv.URL(), ch.Token, "tools/call",
		`{"name":"add_artifact","arguments":{"path":"x.txt"}}`)
	if rr.Error != nil {
		t.Fatalf("protocol error: %+v", rr.Error)
	}
	if m := resultMap(t, rr); m["isError"] != true {
		t.Errorf("result = %v, want isError (no artifact sink)", m)
	}
}
