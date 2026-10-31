// POST /mcp: auth, batch refusal, per-agent MCP server, tool dispatch
// through the policy pipeline. Port of the second half of src/gateway.ts.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/tools"
)

// fail is the TS fail(): every pipeline denial and handler exception reaches
// the model as an isError tool result with a "Failed: " prefix.
func fail(why string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Failed: " + why}},
		IsError: true,
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// mcpPost reproduces the two express layers: auth+parse first, dispatch
// second. One request = one decision.
func (d *Deps) mcpPost(w http.ResponseWriter, r *http.Request) {
	defer func() {
		// The TS express error handler: whatever a handler did not catch.
		if rec := recover(); rec != nil {
			fmt.Printf("[mcp] unhandled error: %v\n", rec)
			writeJSON(w, 500, `{"jsonrpc":"2.0","error":{"code":-32603,"message":"Internal error"},"id":null}`)
		}
	}()

	// Before boot finishes there is no legacy bearer and no tool list; an
	// answer now would be a 401 or an empty tools/list that Open WebUI caches.
	if !d.Ready() {
		writeJSON(w, 503, `{"error":"starting"}`)
		return
	}
	provided := bearerOf(r)
	agent, err := d.Authenticate()(provided)
	if err != nil {
		// The policy DB could not answer. Neither "unauthorized" nor a stack.
		fmt.Printf("[auth] policy lookup failed: %v\n", err)
		writeJSON(w, 503, `{"error":"policy unavailable"}`)
		return
	}
	if agent == nil {
		source := r.RemoteAddr
		fmt.Printf("[auth] rejected request from %s\n", orUnknown(source))
		if a := d.Audit(); a != nil {
			a.AuthFailure(source, "")
		}
		writeJSON(w, 401, `{"error":"unauthorized"}`)
		return
	}

	// express.json({limit:"4mb"}), mounted after auth: anything else on the
	// shared Docker network cannot make this service parse megabytes of JSON
	// per request with no credential.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		// body-parser 413 → the TS catch-all maps 4xx to -32600.
		writeJSON(w, 413, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Invalid request"},"id":null}`)
		return
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// A body the JSON parser refused is the client's error, in JSON-RPC terms.
		writeJSON(w, 400, `{"jsonrpc":"2.0","error":{"code":-32700,"message":"Parse error"},"id":null}`)
		return
	}

	// Batching was removed from the protocol in 2025-06-18 and Open WebUI
	// never sends it; refusing it keeps one request = one decision.
	if _, isArray := parsed.([]any); isArray {
		writeJSON(w, 400, `{"error":"batch requests are not supported"}`)
		return
	}

	// The TS streamable transport frames EVERY /mcp JSON-RPC answer as SSE
	// ("event: message\ndata: <json>\n\n", content-type text/event-stream),
	// including protocol errors. The answers this layer owns are framed the
	// same way. (Oracle-pinned.)
	sseJSON := func(payload string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
	}
	idBytes := func(obj map[string]any) []byte {
		id, _ := json.Marshal(obj["id"])
		if len(id) == 0 {
			id = []byte("null")
		}
		return id
	}

	if obj, ok := parsed.(map[string]any); ok {
		switch obj["method"] {
		case "tools/list":
			// The go-sdk's own answer adds ttlMs/cacheScope fields and sorts
			// the tools; the TS answer is {"result":{"tools":[...]}} in the
			// pinned exposure order with schemas passed through byte-exactly.
			var b strings.Builder
			b.WriteString(`{"result":{"tools":[`)
			first := true
			enabled := map[string]bool{}
			for _, n := range d.ToolsFor(agent) {
				enabled[n] = true
			}
			for i := range d.Schemas {
				t := d.Schemas[i]
				if !enabled[t.Name] {
					continue
				}
				if !first {
					b.WriteByte(',')
				}
				first = false
				name, _ := json.Marshal(t.Name)
				desc, _ := json.Marshal(t.Description)
				var compact bytes.Buffer
				if err := json.Compact(&compact, t.InputSchema); err != nil {
					continue // boot-validated; unreachable
				}
				fmt.Fprintf(&b, `{"name":%s,"description":%s,"inputSchema":%s}`, name, desc, compact.Bytes())
			}
			fmt.Fprintf(&b, `]},"jsonrpc":"2.0","id":%s}`, idBytes(obj))
			sseJSON(b.String())
			return
		case "tools/call":
			params, _ := obj["params"].(map[string]any)
			// The TS SDK validates the request envelope first: params.arguments
			// must be a record when present. Null/array/scalar arguments get
			// this zod-issue envelope before any tool lookup. (Oracle-pinned.)
			if rawArgs, present := params["arguments"]; present {
				if _, isObj := rawArgs.(map[string]any); !isObj {
					received := "null"
					switch rawArgs.(type) {
					case []any:
						received = "array"
					case string:
						received = "string"
					case float64:
						received = "number"
					case bool:
						received = "boolean"
					}
					type zodIssue struct {
						Expected string   `json:"expected"`
						Code     string   `json:"code"`
						Path     []string `json:"path"`
						Message  string   `json:"message"`
					}
					pretty, _ := json.MarshalIndent([]zodIssue{{
						Expected: "record",
						Code:     "invalid_type",
						Path:     []string{"params", "arguments"},
						Message:  "Invalid input: expected record, received " + received,
					}}, "", "  ")
					msg := mustJSON("Invalid tools/call request: " + string(pretty))
					sseJSON(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":%s}}`, idBytes(obj), msg))
					return
				}
			}
			if name, ok := params["name"].(string); ok {
				if !contains(d.ToolsFor(agent), name) {
					// The TS gateway audits a tools/call for a tool this agent
					// cannot use and lets the Node SDK answer -32602 "Tool
					// <name> not found"; the go-sdk's own refusal says
					// `unknown tool "<name>"`. (Oracle-pinned.)
					if a := d.Audit(); a != nil {
						a.Tool(agent.Slug, truncate(name, 100), "deny", "not_enabled", nil)
					}
					msg := mustJSON(fmt.Sprintf("Tool %s not found", name))
					sseJSON(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":%s}}`, idBytes(obj), msg))
					return
				}
				// Boundary validation: the TS SDK validates arguments against
				// the served inputSchema (AJV, allErrors) BEFORE the handler.
				args, _ := params["arguments"].(map[string]any)
				if args == nil {
					args = map[string]any{} // TS: args ?? {}
				}
				if schema := d.schemaFor(name); schema != nil {
					if verrs := validateBoundary(schema, args); len(verrs) > 0 {
						text := fmt.Sprintf("Input validation error: Invalid arguments for tool %s: %s", name, strings.Join(verrs, ", "))
						result := fmt.Sprintf(`{"content":[{"type":"text","text":%s}],"isError":true}`, mustJSON(text))
						sseJSON(fmt.Sprintf(`{"result":%s,"jsonrpc":"2.0","id":%s}`, result, idBytes(obj)))
						return
					}
				}
			}
		}
	}

	r = r.WithContext(withAgent(r.Context(), agent))
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	d.streamable.ServeHTTP(w, r)
}

// mustJSON: JSON.stringify semantics — no HTML escaping (Go's Marshal
// escapes <>& which the TS server emits raw; oracle-pinned on "must be <=").
func mustJSON(v string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // strings always marshal
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// schemaFor: the pinned served inputSchema for a tool, for boundary validation.
func (d *Deps) schemaFor(name string) json.RawMessage {
	for i := range d.Schemas {
		if d.Schemas[i].Name == name {
			return d.Schemas[i].InputSchema
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] // audit label cap; TS name.slice(0, 100)
}

// buildServer: a server exposing this agent's tools. Schemas pass through
// from the pinned JSON rather than being re-declared. (TS createServer.)
func (d *Deps) buildServer(agent *policy.AgentContext) *mcp.Server {
	// Capabilities non-nil: the go-sdk's nil default adds {"logging":{}}, which
	// the TS server does not advertise. Tools listChanged is inferred.
	server := mcp.NewServer(&mcp.Implementation{Name: "gworkspace", Version: "0.2.0"},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}})
	enabled := map[string]bool{}
	for _, name := range d.ToolsFor(agent) {
		enabled[name] = true
	}
	for i := range d.Schemas {
		t := d.Schemas[i]
		if !enabled[t.Name] {
			continue
		}
		desc := t.Description
		if desc == "" {
			desc = "Google Workspace: " + t.Name
		}
		var schema any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			// Registration is validated at boot; reaching here means a pinned
			// schema no SDK would compile.
			fmt.Printf("[mcp] could not build the tool server: %v\n", err)
			continue
		}
		name := t.Name
		server.AddTool(&mcp.Tool{Name: name, Description: desc, InputSchema: schema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return d.callTool(ctx, agent, name, req)
			})
	}
	return server
}

// callTool: every call goes through the policy pipeline. Port of the TS
// registerTool callback, with upstream.callTool replaced by the registry.
func (d *Deps) callTool(ctx context.Context, agent *policy.AgentContext, name string, req *mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
	var raw map[string]any
	if len(req.Params.Arguments) > 0 {
		if jerr := json.Unmarshal(req.Params.Arguments, &raw); jerr != nil {
			// The SDK hands us validated JSON; a failure here is a client frame
			// the TS SDK would have rejected at its own layer.
			return nil, fmt.Errorf("invalid arguments: %w", jerr)
		}
	}
	if raw == nil {
		raw = map[string]any{}
	}

	// Argument KEYS only. The values are document text, spreadsheet cells,
	// search queries and calendar descriptions — user content, sometimes
	// personal, going into a log file with no retention policy of its own.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	fmt.Printf("[tool] %s %s (%s)\n", agent.Slug, name, strings.Join(keys, ", "))

	aud := d.Audit()
	resources := []string{}
	defer func() {
		// The TS catch: any panic in a handler is an exception error result.
		if rec := recover(); rec != nil {
			if aud != nil {
				aud.Tool(agent.Slug, name, "error", "exception", resources)
			}
			result = fail(fmt.Sprintf("%v", rec))
			err = nil
		}
	}()

	plan, err := enforce.PlanCall(ctx, agent, name, raw, d.Pipeline)
	if err != nil {
		if aud != nil {
			aud.Tool(agent.Slug, name, "error", "exception", resources)
		}
		return fail(err.Error()), nil
	}
	resources = plan.Resources

	switch plan.Kind {
	case "deny":
		if aud != nil {
			aud.Tool(agent.Slug, name, "deny", plan.Reason, plan.Resources)
		}
		// calendar_rejected is audited and returned, and feeds nothing:
		// Calendar says nothing about whether Drive works.
		if plan.Reason == enforce.ReasonDriveError || plan.Reason == enforce.ReasonCredentialRejected {
			detail := plan.Detail
			d.Google.LogDrive(name, plan.Reason, plan.Category, &detail)
			kind := "transient"
			if plan.Reason == enforce.ReasonCredentialRejected {
				kind = "rejected"
			}
			d.Google.Failure(kind, plan.Category, orReason(plan.Detail, plan.Reason))
		}
		return fail(plan.Message), nil

	case "serve":
		text, serr := plan.Serve(ctx)
		if serr == nil {
			if aud != nil {
				aud.Tool(agent.Slug, name, "allow", "served", plan.Resources)
			}
			return textResult(text), nil
		}
		var tooLarge *enforce.ScopeTooLarge
		if errors.As(serr, &tooLarge) {
			if aud != nil {
				aud.Tool(agent.Slug, name, "deny", "too_large", plan.Resources)
			}
			return fail(enforce.DenyMessage("too_large", "", "")), nil
		}
		var de *drive.Error
		if errors.As(serr, &de) && de.Rejection != "" && de.Source != drive.SourceCalendar {
			category := drive.CategoryOf(serr)
			detail := de.Error()
			if aud != nil {
				aud.Tool(agent.Slug, name, "deny", "credential_rejected", plan.Resources)
			}
			d.Google.LogDrive(name, "credential_rejected", category, &detail)
			d.Google.Failure("rejected", category, detail)
			return fail(enforce.DenyMessage("credential_rejected", "", drive.RejectionDetail(de))), nil
		}
		if (errors.As(serr, &de) && de.Retryable) || isAbort(serr) {
			category := drive.CategoryOf(serr)
			detail := de.Error()
			if aud != nil {
				aud.Tool(agent.Slug, name, "deny", "drive_error", plan.Resources)
			}
			d.Google.LogDrive(name, "drive_error", category, &detail)
			d.Google.Failure("transient", category, detail)
			return fail(enforce.DenyMessage("drive_error", "", "")), nil
		}
		// Anything else: the TS rethrows into the outer catch.
		if aud != nil {
			aud.Tool(agent.Slug, name, "error", "exception", plan.Resources)
		}
		return fail(serr.Error()), nil
	}

	// "relay": the TS passed the call to the upstream child; the Go port runs
	// the Go-native handler in-process. Result text and isError pass through
	// verbatim, as the relayed result did.
	handler, ok := d.Registry[name]
	if !ok {
		// Enabled but unimplemented: an internal gap, not a user error. The TS
		// could not reach this state (upstream owned every exposed tool).
		if aud != nil {
			aud.Tool(agent.Slug, name, "error", "exception", plan.Resources)
		}
		return fail("tool handler unavailable"), nil
	}
	argsJSON, _ := json.Marshal(plan.Args)
	text, herr := handler(ctx, d.GoogleTools, argsJSON)
	if herr != nil {
		var ue *tools.UserError
		if errors.As(herr, &ue) {
			// fastmcp resolves UserError as isError:true with this exact text;
			// the TS relay logged and audited it as an upstream error.
			fmt.Printf("[tool] %s %s FAILED: %s\n", agent.Slug, name, ue.Msg)
			if aud != nil {
				aud.Tool(agent.Slug, name, "error", "upstream", plan.Resources)
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: ue.Msg}},
				IsError: true,
			}, nil
		}
		if aud != nil {
			aud.Tool(agent.Slug, name, "error", "exception", plan.Resources)
		}
		return fail(herr.Error()), nil
	}
	if aud != nil {
		if plan.Mutated {
			aud.Tool(agent.Slug, name, "mutate", "", plan.Resources)
		} else {
			aud.Tool(agent.Slug, name, "allow", "", plan.Resources)
		}
	}
	if plan.After != nil {
		plan.After()
	}
	return textResult(text), nil
}

func orReason(detail, reason string) string {
	if detail != "" {
		return detail
	}
	return reason
}

// isAbort: the TS checks error.name AbortError/TimeoutError — in Go, a
// context deadline or a net timeout on the request.
func isAbort(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}
