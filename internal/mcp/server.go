package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zhoudm1743/kimi-ssh/internal/session"
	"github.com/zhoudm1743/kimi-ssh/internal/ssh"
)

// JSON-RPC error codes this server can emit.
const (
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
)

// Request is a JSON-RPC 2.0 request. A notification carries no id and must not
// be answered.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response carrying either a result or an error.
type Response struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *Error      `json:"error,omitempty"`
}

// Error is the JSON-RPC error object.
type Error struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Tool is one entry of the tools/list result.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema,omitempty"`
}

// toolSpec binds a tool's public schema to the code that runs it. Both live in
// one value so tools/list and tools/call cannot drift apart.
type toolSpec struct {
	name        string
	description string
	properties  map[string]interface{}
	required    []string
	run         func(*Server, json.RawMessage) ([]string, error)
}

// inputSchema renders the spec as the JSON Schema a client expects. Tools that
// take no arguments still advertise an empty object.
func (spec toolSpec) inputSchema() map[string]interface{} {
	properties := spec.properties
	if properties == nil {
		properties = map[string]interface{}{}
	}

	schema := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}
	if len(spec.required) > 0 {
		schema["required"] = spec.required
	}
	return schema
}

func (spec toolSpec) tool() Tool {
	return Tool{
		Name:        spec.name,
		Description: spec.description,
		InputSchema: spec.inputSchema(),
	}
}

// Server answers MCP requests using the SSH hosts in the user's config.
type Server struct {
	configParser   *ssh.ConfigParser
	sessionManager *session.SessionManager
	tools          []toolSpec
	methods        map[string]func(Request) *Response
}

// NewServer wires a server to a fresh config parser, connection manager and
// session manager, and installs the method and tool tables.
func NewServer() *Server {
	server := &Server{
		configParser:   ssh.NewConfigParser(),
		sessionManager: session.NewSessionManager(ssh.NewConnectionManager()),
		tools:          toolSpecs(),
	}

	ignore := func(Request) *Response { return nil }
	server.methods = map[string]func(Request) *Response{
		"initialize":                server.handleInitialize,
		"initialized":               ignore,
		"notifications/initialized": ignore,
		"shutdown":                  server.handleShutdown,
		"tools/list":                server.handleListTools,
		"list_tools":                server.handleListTools,
		"tools/call":                server.handleCallTool,
		"call_tool":                 server.handleCallTool,
	}

	return server
}

// HandleRequest dispatches one request and returns the reply, or nil when the
// frame must stay unanswered.
func (s *Server) HandleRequest(req Request) *Response {
	// Notifications are fire-and-forget: writing a reply would put an
	// unsolicited frame on the wire.
	if req.ID == nil {
		return nil
	}

	if method, known := s.methods[req.Method]; known {
		return method(req)
	}

	return failure(req.ID, codeMethodNotFound, "Method not found")
}

func (s *Server) handleInitialize(req Request) *Response {
	if err := s.configParser.ParseConfigFiles(); err != nil {
		// The handshake still succeeds: a broken config is a tool-level
		// problem, not a protocol-level one.
		fmt.Fprintf(os.Stderr, "kimi-ssh: failed to parse SSH config: %v\n", err)
	} else {
		hosts := s.configParser.GetAllConfigs()
		fmt.Fprintf(os.Stderr, "kimi-ssh: loaded %d SSH hosts from config\n", len(hosts))
		s.sessionManager.SetConfigs(hosts)
	}

	return success(req.ID, map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{
				"listChanged": false,
			},
		},
		"serverInfo": map[string]interface{}{
			"name":    "kimi-ssh",
			"version": "1.2.2",
		},
	})
}

func (s *Server) handleShutdown(req Request) *Response {
	return success(req.ID, true)
}

func (s *Server) handleListTools(req Request) *Response {
	tools := make([]Tool, 0, len(s.tools))
	for _, spec := range s.tools {
		tools = append(tools, spec.tool())
	}

	return success(req.ID, map[string]interface{}{"tools": tools})
}

// handleCallTool runs the named tool. A tool that fails still produces a
// successful call whose content carries isError, so the client can show the
// message to the model instead of treating it as a protocol fault.
func (s *Server) handleCallTool(req Request) *Response {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &call); err != nil {
		return failure(req.ID, codeInvalidParams, "Invalid tool call format")
	}

	for _, spec := range s.tools {
		if spec.name != call.Name {
			continue
		}

		texts, err := spec.run(s, call.Arguments)
		if err != nil {
			return success(req.ID, content([]string{err.Error()}, true))
		}
		return success(req.ID, content(texts, false))
	}

	return failure(req.ID, codeMethodNotFound, fmt.Sprintf("Unknown tool: %s", call.Name))
}

// toolSpecs is the tool table: one entry per tool the server publishes.
func toolSpecs() []toolSpec {
	return []toolSpec{
		{
			name:        "ssh_list",
			description: "List available SSH hosts from config",
			run:         (*Server).listHosts,
		},
		{
			name:        "ssh_connect",
			description: "Connect to a remote server via SSH",
			properties: map[string]interface{}{
				"host": field("string", "SSH config host alias or IP"),
			},
			required: []string{"host"},
			run:      (*Server).connectHost,
		},
		{
			name:        "ssh_exec",
			description: "Execute command on remote server",
			properties: map[string]interface{}{
				"command": field("string", "Command to execute"),
				"host":    field("string", "Target host (optional, uses active session)"),
			},
			required: []string{"command"},
			run:      (*Server).execCommand,
		},
		{
			name:        "ssh_status",
			description: "Show current SSH connection status",
			run:         (*Server).status,
		},
		{
			name:        "ssh_disconnect",
			description: "Disconnect from a remote server",
			properties: map[string]interface{}{
				"host": field("string", "Host alias to disconnect"),
			},
			run: (*Server).disconnectHost,
		},
	}
}

// listHosts reports every alias parsed from the SSH config.
func (s *Server) listHosts(json.RawMessage) ([]string, error) {
	configs := s.configParser.GetAllConfigs()

	hosts := make([]map[string]string, 0, len(configs))
	for alias, config := range configs {
		hosts = append(hosts, map[string]string{
			"host":     alias,
			"hostname": config.HostName,
			"port":     config.Port,
			"user":     config.User,
		})
	}

	return []string{
		fmt.Sprintf("Found %d SSH hosts in config", len(hosts)),
		formatHosts(hosts),
	}, nil
}

// connectHost opens a session and leaves it active for later ssh_exec calls.
func (s *Server) connectHost(arguments json.RawMessage) ([]string, error) {
	var args struct {
		Host string `json:"host"`
	}
	if err := decode(arguments, &args); err != nil {
		return nil, errors.New("Invalid parameters for ssh_connect")
	}
	if args.Host == "" {
		return nil, errors.New("Host parameter is required")
	}

	if err := s.sessionManager.Connect(args.Host); err != nil {
		return nil, err
	}

	return []string{fmt.Sprintf("Successfully connected to %s", args.Host)}, nil
}

// execCommand runs a command on the named host, or on the active one when the
// argument is empty. Output and a non-zero exit status are all reported as
// content; only a failure to run anything at all becomes an error.
func (s *Server) execCommand(arguments json.RawMessage) ([]string, error) {
	var args struct {
		Command string `json:"command"`
		Host    string `json:"host"`
	}
	if err := decode(arguments, &args); err != nil {
		return nil, errors.New("Invalid parameters for ssh_exec")
	}
	if args.Command == "" {
		return nil, errors.New("Command parameter is required")
	}

	stdout, stderr, runErr := s.sessionManager.Execute(args.Command, args.Host)

	texts := []string{fmt.Sprintf("Executed command: %s", args.Command)}
	if stderr != "" {
		texts = append(texts, "STDERR:\n"+stderr)
	}
	if stdout != "" {
		texts = append(texts, "STDOUT:\n"+stdout)
	}
	if runErr != nil {
		texts = append(texts, fmt.Sprintf("ERROR: %v", runErr))
	}

	return texts, nil
}

// status summarizes what is connected right now.
func (s *Server) status(json.RawMessage) ([]string, error) {
	active := s.sessionManager.ListActiveConnections()

	texts := []string{fmt.Sprintf("Active connections: %d", len(active))}
	if len(active) > 0 {
		texts = append(texts, fmt.Sprintf("Active hosts: %v", active))
	}
	if host := s.sessionManager.GetActiveHost(); host != "" {
		texts = append(texts, fmt.Sprintf("Current active host: %s", host))
	} else {
		texts = append(texts, "No active host selected")
	}

	return texts, nil
}

// disconnectHost closes a session, defaulting to the active host.
func (s *Server) disconnectHost(arguments json.RawMessage) ([]string, error) {
	var args struct {
		Host string `json:"host"`
	}
	if err := decode(arguments, &args); err != nil {
		return nil, errors.New("Invalid parameters for ssh_disconnect")
	}

	host := args.Host
	if host == "" {
		if host = s.sessionManager.GetActiveHost(); host == "" {
			return nil, errors.New("No active host to disconnect from")
		}
	}

	if err := s.sessionManager.Disconnect(host); err != nil {
		return nil, err
	}

	return []string{fmt.Sprintf("Disconnected from %s", host)}, nil
}

// content builds a tools/call result from text blocks, marking it as an error
// when the tool refused to do its job.
func content(texts []string, isError bool) map[string]interface{} {
	blocks := make([]interface{}, 0, len(texts))
	for _, text := range texts {
		blocks = append(blocks, map[string]interface{}{
			"type": "text",
			"text": text,
		})
	}

	result := map[string]interface{}{"content": blocks}
	if isError {
		result["isError"] = true
	}
	return result
}

// failure builds a JSON-RPC error response.
func failure(id interface{}, code int, message string) *Response {
	return &Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &Error{Code: code, Message: message},
	}
}

// success builds a JSON-RPC result response.
func success(id interface{}, result interface{}) *Response {
	return &Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

// decode reads tool arguments. A call that omits "arguments" arrives as an
// empty raw message and simply leaves the destination zero-valued.
func decode(arguments json.RawMessage, destination interface{}) error {
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return json.Unmarshal(trimmed, destination)
}

// field describes one property of a tool's input schema.
func field(kind, description string) map[string]interface{} {
	return map[string]interface{}{
		"type":        kind,
		"description": description,
	}
}

// formatHosts renders the ssh_list body, one line per alias.
func formatHosts(hosts []map[string]string) string {
	if len(hosts) == 0 {
		return "No SSH hosts found in config."
	}

	var out strings.Builder
	out.WriteString("Available SSH hosts:\n")
	for _, host := range hosts {
		fmt.Fprintf(&out, "  - %s (%s@%s:%s)\n", host["host"], host["user"], host["hostname"], host["port"])
	}
	return out.String()
}
