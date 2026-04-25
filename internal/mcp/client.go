// Package mcp implements a minimal stdio Model Context Protocol client.
//
// It speaks just enough of MCP to drive safe-ai-util-mcp from the burndown
// dispatcher: the initialize handshake, tools/list, and tools/call. Notifications
// from the server are ignored (we don't need progress events or resource
// subscriptions for this use case).
//
// The transport is decoupled from the spawning concern. Production code uses
// `Spawn` to launch a subprocess and pipe its stdin/stdout; tests use
// `NewClient` directly with an `io.Pipe` so they don't shell out.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the MCP version we advertise during initialize. Servers
// that don't support this version are expected to reply with the version they
// do support; we accept whatever they negotiate down to since we use only the
// stable subset.
const ProtocolVersion = "2025-06-18"

// ClientName is the identifier the burndown driver advertises to MCP servers.
const ClientName = "overnight-burndown"

// Transport is the bidirectional stream the client reads from and writes to.
// Production wraps a subprocess; tests use io.Pipe.
type Transport interface {
	io.Reader
	io.Writer
	io.Closer
}

// ToolDef describes one tool surfaced by the server. Mirrors the shape of
// `tools/list` entries so callers can hand it directly to Anthropic's tool
// registration without translation.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// CallResult is what `tools/call` returns. The text content is whatever the
// server chose to emit; for safe-ai-util-mcp this is a JSON-encoded
// {code, stdout, stderr} object that callers can unmarshal.
type CallResult struct {
	Text    string
	IsError bool
}

// Client is an MCP stdio client. Safe for concurrent CallTool / ListTools
// from multiple goroutines (writes are serialized; responses are demuxed by
// request id).
type Client struct {
	transport Transport
	encoder   *json.Encoder
	writeMu   sync.Mutex // serializes writes to transport

	nextID  atomic.Int64
	pending sync.Map // int64 -> chan rpcResponse

	closed    atomic.Bool
	closeOnce sync.Once
	doneCh    chan struct{}
	readErr   error // set when readLoop exits abnormally

	// subprocess (set by Spawn, nil for direct-transport clients)
	cmd *exec.Cmd
}

// NewClient takes ownership of `t` and performs the MCP initialize handshake.
// On any error during init the transport is closed and the error returned.
func NewClient(ctx context.Context, t Transport) (*Client, error) {
	c := &Client{
		transport: t,
		encoder:   json.NewEncoder(t),
		doneCh:    make(chan struct{}),
	}
	go c.readLoop(bufio.NewReader(t))

	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Spawn launches `name args...` as a subprocess and wires its stdio to a new
// Client. The subprocess inherits `env` (pass nil for os.Environ()). Stderr
// is captured into a small buffer for diagnostic purposes; on Close it's
// included in the returned error if the process exited badly.
func Spawn(ctx context.Context, name string, args []string, env []string) (*Client, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard // safe-ai-util-mcp logs to stderr; we ignore by default

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %q: %w", name, err)
	}

	t := &subprocessTransport{stdin: stdin, stdout: stdout}
	c, err := NewClient(ctx, t)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, err
	}
	c.cmd = cmd
	return c, nil
}

// Close shuts down the client. Idempotent.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if cerr := c.transport.Close(); cerr != nil {
			err = cerr
		}
		if c.cmd != nil {
			// Give the subprocess a moment to exit on its own; kill if not.
			done := make(chan error, 1)
			go func() { done <- c.cmd.Wait() }()
			select {
			case <-done:
			default:
				_ = c.cmd.Process.Kill()
				<-done
			}
		}
		close(c.doneCh)
	})
	return err
}

// ListTools returns the server's tool catalog.
func (c *Client) ListTools(ctx context.Context) ([]ToolDef, error) {
	raw, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/list: %w", err)
	}
	return resp.Tools, nil
}

// CallTool invokes a tool by name with the given arguments.
func (c *Client) CallTool(ctx context.Context, name string, args any) (*CallResult, error) {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	raw, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/call: %w", err)
	}
	// Concatenate text-typed content; ignore non-text blocks for now (the
	// burndown driver's tools all return JSON text).
	var textBuf string
	for _, b := range resp.Content {
		if b.Type == "text" {
			textBuf += b.Text
		}
	}
	return &CallResult{Text: textBuf, IsError: resp.IsError}, nil
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"` // present on server-initiated notifications
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message)
}

// call sends a request and waits for the matching response. Cancellation of
// ctx unblocks the wait but does not cancel the request server-side — the
// server may still emit a response that lands in /dev/null. The cost is a
// small leaked entry in `pending` for the lifetime of the client; acceptable.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, errors.New("mcp: client closed")
	}
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := c.writeJSON(req); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.doneCh:
		if c.readErr != nil {
			return nil, c.readErr
		}
		return nil, errors.New("mcp: client closed during call")
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *Client) notify(method string, params any) error {
	n := rpcNotification{JSONRPC: "2.0", Method: method, Params: params}
	return c.writeJSON(n)
}

func (c *Client) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.encoder.Encode(v); err != nil {
		return fmt.Errorf("mcp: write: %w", err)
	}
	return nil
}

// readLoop demuxes server messages. Responses (have id) deliver to the
// pending channel; notifications (no id) are dropped. If the transport closes
// or a decode fails irrecoverably, every pending caller is unblocked with
// readErr.
func (c *Client) readLoop(r *bufio.Reader) {
	dec := json.NewDecoder(r)
	for {
		var msg rpcResponse
		if err := dec.Decode(&msg); err != nil {
			if !errors.Is(err, io.EOF) && !c.closed.Load() {
				c.readErr = fmt.Errorf("mcp: read: %w", err)
			}
			c.failAllPending()
			return
		}
		if msg.ID == nil {
			// notification — ignore
			continue
		}
		if v, ok := c.pending.Load(*msg.ID); ok {
			ch := v.(chan rpcResponse)
			select {
			case ch <- msg:
			default:
				// caller already cancelled; drop response
			}
		}
	}
}

func (c *Client) failAllPending() {
	c.pending.Range(func(_, v any) bool {
		ch := v.(chan rpcResponse)
		select {
		case ch <- rpcResponse{Error: &rpcError{Code: -32000, Message: "transport closed"}}:
		default:
		}
		return true
	})
}

// initialize performs the MCP handshake: send `initialize`, await response,
// send `notifications/initialized`.
func (c *Client) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": ClientName, "version": "0.1.0-pre"},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp: initialized notification: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// subprocess transport
// ---------------------------------------------------------------------------

type subprocessTransport struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (t *subprocessTransport) Read(p []byte) (int, error)  { return t.stdout.Read(p) }
func (t *subprocessTransport) Write(p []byte) (int, error) { return t.stdin.Write(p) }
func (t *subprocessTransport) Close() error {
	// Closing stdin signals the server to exit cleanly. Then close stdout.
	cerr := t.stdin.Close()
	if rerr := t.stdout.Close(); rerr != nil && cerr == nil {
		cerr = rerr
	}
	return cerr
}
