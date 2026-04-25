package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// pipeTransport bridges two io.Pipes so the client and the fake server can
// each Read/Write/Close their own side. The client owns one end, the fake
// server reads from clientWrites and writes to clientReads.
type pipeTransport struct {
	r io.Reader
	w io.Writer
	c io.Closer
}

func (p *pipeTransport) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeTransport) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeTransport) Close() error                { return p.c.Close() }

// closeBoth wraps two closers so closing one closes the other (used so the
// client closing its side ends the server's read loop).
type closeBoth struct{ a, b io.Closer }

func (c *closeBoth) Close() error {
	err1 := c.a.Close()
	err2 := c.b.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// fakeServer reads JSON-RPC requests and dispatches them via the handler.
// One server per test; cleanup closes both pipes.
type fakeServer struct {
	t       *testing.T
	in      *io.PipeReader // server reads requests from here
	out     *io.PipeWriter // server writes responses here
	handler func(method string, params json.RawMessage, id *int64) (any, *rpcError)

	mu             sync.Mutex
	initializeSeen bool
	initializedSeen bool
	requestCount   int
	listCalls      int
	callToolCalls  int
	lastCallTool   struct {
		name string
		args json.RawMessage
	}
}

func newFakeServer(t *testing.T) (*Client, *fakeServer) {
	t.Helper()
	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()

	transport := &pipeTransport{
		r: clientReads,
		w: clientWrites,
		c: &closeBoth{a: clientWrites, b: clientReads},
	}

	srv := &fakeServer{
		t:   t,
		in:  serverReads,
		out: serverWrites,
	}
	srv.handler = srv.defaultHandler

	go srv.run()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewClient(ctx, transport)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = serverReads.Close()
		_ = serverWrites.Close()
	})
	return client, srv
}

// run is the server's read/respond loop.
func (s *fakeServer) run() {
	defer s.out.Close()
	dec := json.NewDecoder(bufio.NewReader(s.in))
	for {
		var raw struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&raw); err != nil {
			return
		}
		s.mu.Lock()
		s.requestCount++
		switch raw.Method {
		case "notifications/initialized":
			s.initializedSeen = true
			s.mu.Unlock()
			continue
		case "initialize":
			s.initializeSeen = true
		}
		handler := s.handler
		s.mu.Unlock()

		result, rpcErr := handler(raw.Method, raw.Params, raw.ID)
		if raw.ID == nil {
			continue // notification: no response
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": *raw.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		_ = json.NewEncoder(s.out).Encode(resp)
	}
}

// defaultHandler implements minimal MCP responses for the test suite.
func (s *fakeServer) defaultHandler(method string, params json.RawMessage, _ *int64) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fake-server", "version": "1.0"},
		}, nil
	case "tools/list":
		s.mu.Lock()
		s.listCalls++
		s.mu.Unlock()
		return map[string]any{
			"tools": []map[string]any{
				{
					"name":        "fs_read",
					"description": "Read a file",
					"inputSchema": map[string]any{
						"type":     "object",
						"required": []string{"path"},
						"properties": map[string]any{
							"path": map[string]any{"type": "string"},
						},
					},
				},
			},
		}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(params, &p)
		s.mu.Lock()
		s.callToolCalls++
		s.lastCallTool.name = p.Name
		s.lastCallTool.args = append([]byte(nil), p.Arguments...)
		s.mu.Unlock()

		if p.Name == "boom" {
			return nil, &rpcError{Code: -32601, Message: "tool not found"}
		}
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": `{"code":0,"stdout":"hello","stderr":""}`},
			},
			"isError": false,
		}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found"}
}

// ---------------------------------------------------------------------------
// initialize handshake
// ---------------------------------------------------------------------------

func TestNewClient_PerformsInitializeHandshake(t *testing.T) {
	_, srv := newFakeServer(t)

	// Allow time for the initialized notification to land before assertions.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		ok := srv.initializeSeen && srv.initializedSeen
		srv.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected initialize + notifications/initialized, got initialize=%v initialized=%v",
		srv.initializeSeen, srv.initializedSeen)
}

// ---------------------------------------------------------------------------
// ListTools
// ---------------------------------------------------------------------------

func TestListTools_DecodesCatalog(t *testing.T) {
	c, _ := newFakeServer(t)

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "fs_read" {
		t.Fatalf("expected one tool fs_read, got %+v", tools)
	}
	if tools[0].InputSchema["type"] != "object" {
		t.Errorf("schema lost in decode: %+v", tools[0].InputSchema)
	}
}

// ---------------------------------------------------------------------------
// CallTool
// ---------------------------------------------------------------------------

func TestCallTool_ConcatenatesTextContent(t *testing.T) {
	c, srv := newFakeServer(t)

	res, err := c.CallTool(context.Background(), "fs_read", map[string]any{"path": "README.md"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Errorf("expected isError=false, got true")
	}
	if res.Text != `{"code":0,"stdout":"hello","stderr":""}` {
		t.Errorf("unexpected text payload: %q", res.Text)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.lastCallTool.name != "fs_read" {
		t.Errorf("server didn't see expected name: %q", srv.lastCallTool.name)
	}
	if !contains(srv.lastCallTool.args, "README.md") {
		t.Errorf("arguments not forwarded: %s", string(srv.lastCallTool.args))
	}
}

func TestCallTool_PropagatesRPCError(t *testing.T) {
	c, _ := newFakeServer(t)

	_, err := c.CallTool(context.Background(), "boom", nil)
	if err == nil {
		t.Fatal("expected rpc error")
	}
	var rpc *rpcError
	if !errors.As(err, &rpc) {
		t.Fatalf("expected *rpcError, got %T: %v", err, err)
	}
	if rpc.Code != -32601 {
		t.Errorf("got code %d, want -32601", rpc.Code)
	}
}

// ---------------------------------------------------------------------------
// concurrent calls demux correctly
// ---------------------------------------------------------------------------

func TestCallTool_Concurrent(t *testing.T) {
	c, _ := newFakeServer(t)

	const N = 8
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, err := c.CallTool(context.Background(), "fs_read", map[string]any{"path": "x"})
			errs <- err
		}()
	}
	for i := 0; i < N; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("concurrent CallTool[%d]: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("concurrent CallTool[%d] timed out", i)
		}
	}
}

// ---------------------------------------------------------------------------
// context cancellation
// ---------------------------------------------------------------------------

func TestCallTool_RespectsContextCancellation(t *testing.T) {
	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()

	transport := &pipeTransport{
		r: clientReads,
		w: clientWrites,
		c: &closeBoth{a: clientWrites, b: clientReads},
	}

	// Server: respond ONLY to initialize, then go silent so tools/call hangs.
	go func() {
		dec := json.NewDecoder(bufio.NewReader(serverReads))
		enc := json.NewEncoder(serverWrites)
		for {
			var raw struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if err := dec.Decode(&raw); err != nil {
				return
			}
			if raw.Method == "initialize" && raw.ID != nil {
				_ = enc.Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      *raw.ID,
					"result":  map[string]any{"protocolVersion": ProtocolVersion},
				})
			}
			// other requests get no reply
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := NewClient(ctx, transport)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	callCtx, cancelCall := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelCall()

	start := time.Now()
	_, err = c.CallTool(callCtx, "fs_read", nil)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("call took %v, expected <500ms (cancellation should be prompt)", elapsed)
	}
}

// ---------------------------------------------------------------------------
// transport closure during pending call
// ---------------------------------------------------------------------------

func TestCallTool_FailsWhenTransportClosesMidCall(t *testing.T) {
	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()

	transport := &pipeTransport{
		r: clientReads,
		w: clientWrites,
		c: &closeBoth{a: clientWrites, b: clientReads},
	}

	go func() {
		dec := json.NewDecoder(bufio.NewReader(serverReads))
		enc := json.NewEncoder(serverWrites)
		for {
			var raw struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if err := dec.Decode(&raw); err != nil {
				return
			}
			if raw.Method == "initialize" && raw.ID != nil {
				_ = enc.Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      *raw.ID,
					"result":  map[string]any{"protocolVersion": ProtocolVersion},
				})
			}
		}
	}()

	ctx := context.Background()
	c, err := NewClient(ctx, transport)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Kick off a call that will hang, then close the transport.
	errCh := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "fs_read", nil)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = serverWrites.Close()
	_ = serverReads.Close()
	_ = c.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error after transport close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CallTool never returned after transport close")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func contains(haystack []byte, needle string) bool {
	return len(haystack) >= len(needle) &&
		string(haystack) != "" &&
		bytesContainsString(haystack, needle)
}

func bytesContainsString(b []byte, s string) bool {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}
