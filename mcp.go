package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// A minimal MCP server over stdio (newline-delimited JSON-RPC 2.0). It only
// exposes tools, which is all Claude Code needs to manage shortcuts.

type rpcRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string                                     `json:"name"`
	Description string                                     `json:"description"`
	InputSchema map[string]any                             `json:"inputSchema"`
	run         func(client, json.RawMessage) (any, error) `json:"-"`
}

func objectSchema(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var mcpTools = []mcpTool{
	{
		Name: "list_services",
		Description: "List every hop shortcut: short names like yt/ that redirect to websites, and local apps " +
			"with their port and whether they are running. Use this to find which port a local service is on.",
		InputSchema: objectSchema([]string{}, map[string]any{}),
		run: func(c client, _ json.RawMessage) (any, error) {
			return c.list()
		},
	},
	{
		Name: "register_service",
		Description: "Give a website or a local app a short name the user can open in the browser as http://<name>/. " +
			"target is a URL (youtube.com, https://mail.google.com, or one with %s for a search term) or a local port number. " +
			"If the name already goes somewhere else this fails and says where; ask the user before retrying with replace=true.",
		InputSchema: objectSchema([]string{"name", "target"}, map[string]any{
			"name":    map[string]any{"type": "string", "description": "Lowercase letters, digits and dashes, e.g. talk-to-db"},
			"target":  map[string]any{"type": "string", "description": "A URL or a port number like 3000"},
			"replace": map[string]any{"type": "boolean", "description": "Replace an existing name. Only after the user agreed."},
		}),
		run: func(c client, raw json.RawMessage) (any, error) {
			var args struct {
				Name, Target string
				Replace      bool
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("bad arguments: %w", err)
			}
			svc := parseTarget(args.Target)
			svc.Name = args.Name
			return c.put(svc, args.Replace)
		},
	},
	{
		Name:        "remove_service",
		Description: "Remove a hop shortcut by name.",
		InputSchema: objectSchema([]string{"name"}, map[string]any{
			"name": map[string]any{"type": "string"},
		}),
		run: func(c client, raw json.RawMessage) (any, error) {
			var args struct{ Name string }
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("bad arguments: %w", err)
			}
			if err := c.remove(args.Name); err != nil {
				return nil, err
			}
			return "removed " + args.Name, nil
		},
	},
	{
		Name:        "free_port",
		Description: "Return an unused local TCP port, for configuring a new app before registering it.",
		InputSchema: objectSchema([]string{}, map[string]any{}),
		run: func(client, json.RawMessage) (any, error) {
			port, err := freePort()
			return strconv.Itoa(port), err
		},
	},
}

func serveMCP(in io.Reader, out io.Writer, c client) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if err := enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}}); err != nil {
				return fmt.Errorf("write response: %w", err)
			}
			continue
		}
		if len(req.ID) == 0 {
			continue // notifications such as notifications/initialized need no reply
		}
		result, rerr := handleMCP(c, req)
		if err := enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return scanner.Err()
}

func handleMCP(c client, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "hop", "version": "0.1.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		return callTool(c, req.Params)
	default:
		return nil, &rpcError{-32601, "method not found: " + req.Method}
	}
}

// Tool failures are returned as isError results, not protocol errors, so the
// model sees the message and can correct itself.
func callTool(c client, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{-32602, "invalid params"}
	}
	for _, t := range mcpTools {
		if t.Name != p.Name {
			continue
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		v, err := t.run(c, p.Arguments)
		if err != nil {
			return toolText(err.Error(), true), nil
		}
		if s, ok := v.(string); ok {
			return toolText(s, false), nil
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		return toolText(string(b), false), nil
	}
	return nil, &rpcError{-32602, "unknown tool: " + p.Name}
}

func toolText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}
