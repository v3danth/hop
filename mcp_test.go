package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// mcpReply is the part of a JSON-RPC response the tests look at.
type mcpReply struct {
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Tools           []struct {
			Name string `json:"name"`
		} `json:"tools"`
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

func TestMCPSession(t *testing.T) {
	s, _ := newTestServer(t) // yt -> https://youtube.com
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"register_service","arguments":{"name":"web","target":"3000"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"register_service","arguments":{"name":"yt","target":"github.com"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"no/such"}`,
		`not json`,
	}, "\n")
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out, newTestClient(t, s)); err != nil {
		t.Fatal(err)
	}
	var replies []mcpReply
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r mcpReply
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, r)
	}

	// The notification gets no reply.
	if len(replies) != 6 {
		t.Fatalf("got %d replies, want 6:\n%s", len(replies), out.String())
	}
	if v := replies[0].Result.ProtocolVersion; v != "2025-06-18" {
		t.Errorf("initialize agreed to %q", v)
	}
	if n := len(replies[1].Result.Tools); n != len(mcpTools) {
		t.Errorf("tools/list returned %d tools, want %d", n, len(mcpTools))
	}
	if r := replies[2].Result; r.IsError {
		t.Errorf("registering web failed: %+v", r.Content)
	}
	if r := replies[3].Result; !r.IsError || len(r.Content) == 0 || !strings.Contains(r.Content[0].Text, "already goes to") {
		t.Errorf("registering over yt without replace = %+v, want a name-taken error", r)
	}
	if e := replies[4].Error; e == nil || e.Code != -32601 {
		t.Errorf("unknown method error = %+v, want -32601", e)
	}
	if e := replies[5].Error; e == nil || e.Code != -32700 {
		t.Errorf("bad JSON error = %+v, want -32700", e)
	}
	if svc, ok := s.reg.Lookup("web"); !ok || svc.Port != 3000 {
		t.Errorf("web = %+v, %v; want port 3000", svc, ok)
	}
}
