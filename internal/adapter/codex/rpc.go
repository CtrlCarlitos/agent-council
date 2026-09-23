package codex

// JSON-RPC 2.0 framing and connection layer for the `codex app-server`
// child (AC-009 spec §3.2): one JSON object per line over the child's
// stdio, monotonically increasing request ids, response/error routing by
// id, and fail-closed poison rules. Framing corruption (malformed or
// oversized frames) poisons the connection with a typed error and no
// recovery; unknown method/error SHAPES stay graceful typed RPC errors.
//
// The connection's read loop is bound to the ADAPTER's lifetime, never to
// a caller context: a disconnected controller must not stop the pump or
// the child. Caller contexts bound only the request write and its ack
// wait inside Call.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

// MaxFrameBytes bounds one JSON-RPC frame (one JSON object per line,
// newline included). Generous against the largest frames observed in the
// 0.154.0 evidence captures by orders of magnitude; a frame exceeding it
// is framing corruption and poisons the connection (spec §3.9).
const MaxFrameBytes = 1 << 20 // 1 MiB

const rpcVersion = "2.0"

// ErrConnClosed reports calls on a closed (or terminated) connection.
var ErrConnClosed = errors.New("codexrpc connection closed")

// ErrCreationPending reports an attempt to open a second pending thread
// creation while one is still unresolved (one pending creation per child).
var ErrCreationPending = errors.New("a thread creation reservation is already pending for this child")

// ErrDuplicateRequestID reports a pending-slot registration for a request
// id that is already in flight.
type ErrDuplicateRequestID struct {
	ID int64
}

func (e *ErrDuplicateRequestID) Error() string {
	return fmt.Sprintf("codexrpc request id %d is already pending", e.ID)
}

// ErrPoisonedFrame reports framing corruption: the connection is poisoned,
// every pending call fails, and no recovery is attempted (spec §3.9:
// stream poisoned → child terminated).
type ErrPoisonedFrame struct {
	Reason string
	Err    error
}

func (e *ErrPoisonedFrame) Error() string {
	if e.Err != nil {
		return "codexrpc stream poisoned (" + e.Reason + "): " + e.Err.Error()
	}
	return "codexrpc stream poisoned (" + e.Reason + ")"
}

func (e *ErrPoisonedFrame) Unwrap() error { return e.Err }

// ErrRequestWrite reports a failed request write. BytesWritten > 0 means
// at least one stdin byte was transmitted before the failure — the native
// side may have received the request, so the outcome is ambiguous
// (post-write), never a clean rejection.
type ErrRequestWrite struct {
	BytesWritten int64
	Err          error
}

func (e *ErrRequestWrite) Error() string {
	return fmt.Sprintf("codexrpc request write failed after %d bytes: %v", e.BytesWritten, e.Err)
}

func (e *ErrRequestWrite) Unwrap() error { return e.Err }

// PostWrite reports whether transmission had begun when the write failed.
func (e *ErrRequestWrite) PostWrite() bool { return e.BytesWritten > 0 }

// RPCError is a typed JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("codexrpc error %d: %s", e.Code, e.Message)
}

// ServerRequest is a server→client JSON-RPC REQUEST (e.g. approval
// requests): it carries an id and must eventually be answered by the
// registered handler via Respond/RespondError. The transport never
// answers on its own.
type ServerRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// pendingCall is one in-flight client request awaiting its response.
type pendingCall struct {
	ch chan pendingResult
}

type pendingResult struct {
	result json.RawMessage
	rpcErr *RPCError
	err    error
}

// countingWriter records how many bytes reached the child's stdin so a
// failed write can be classified pre- vs post-transmission.
type countingWriter struct {
	mu   sync.Mutex
	w    io.Writer
	full bool
	n    int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	full := c.full
	c.mu.Unlock()
	if full {
		return 0, io.ErrClosedPipe
	}
	written, err := c.w.Write(p)
	c.mu.Lock()
	c.n += int64(written)
	c.mu.Unlock()
	return written, err
}

func (c *countingWriter) markFull() {
	c.mu.Lock()
	c.full = true
	c.mu.Unlock()
}

func (c *countingWriter) bytesWritten() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// frameHead is the discriminated shape of one inbound line.
type frameHead struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

// Conn is one JSON-RPC connection over the codex app-server child's stdio.
// Frames are one JSON object per line; ids are monotonically increasing;
// responses and errors route to callers by id. A caller context bounds
// only its own write+ack wait; the read loop never sees caller contexts.
type Conn struct {
	stdout io.Reader
	w      *countingWriter

	mu      sync.Mutex
	nextID  int64
	pending map[int64]*pendingCall
	closed  bool
	poison  error

	reqMu       sync.Mutex
	reqHandler  func(ServerRequest)
	reqQueue    chan ServerRequest
	reqQueueMu  sync.Mutex
	reqQueueOn  bool
	notifFn     func(method string, params json.RawMessage)
	notifMu     sync.Mutex
	fatalFn     func(error)
	fatalMu     sync.Mutex
	serverReqWG sync.WaitGroup

	startOnce sync.Once
	done      chan struct{}
	doneOnce  sync.Once
}

// NewConn wires a connection over the child's stdin/stdout pipes. Start
// arms the framing reader; it must run before any notification-emitting
// request is sent (pump-before-first-notification, spec §3.2).
func NewConn(stdin io.Writer, stdout io.Reader) *Conn {
	return &Conn{
		stdout:   stdout,
		w:        &countingWriter{w: stdin},
		nextID:   0,
		pending:  make(map[int64]*pendingCall),
		reqQueue: make(chan ServerRequest, maxQueuedServerRequests),
		done:     make(chan struct{}),
	}
}

// SetNotificationHandler routes server notifications (no id).
func (c *Conn) SetNotificationHandler(fn func(method string, params json.RawMessage)) {
	c.notifMu.Lock()
	c.notifFn = fn
	c.notifMu.Unlock()
}

// SetRequestHandler registers the server→client request handler (the
// approval seam). When unset, requests surface through the bounded
// NextServerRequest queue; a full queue is framing-level evidence loss and
// poisons the connection rather than dropping an approval silently.
func (c *Conn) SetRequestHandler(fn func(ServerRequest)) {
	c.reqMu.Lock()
	c.reqHandler = fn
	c.reqMu.Unlock()
}

// SetFatalHandler registers the callback fired once when framing
// corruption (or any terminal stream failure) poisons the connection. The
// server manager uses it to terminate the child.
func (c *Conn) SetFatalHandler(fn func(error)) {
	c.fatalMu.Lock()
	c.fatalFn = fn
	c.fatalMu.Unlock()
}

// Start launches the framing reader exactly once. It runs without any
// caller context and exits only on stream end, poison, or Close.
func (c *Conn) Start() {
	c.startOnce.Do(func() { go c.readLoop() })
}

// Done closes when the read loop has terminated.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Fatal returns the first terminal stream error, or nil while the stream
// is healthy.
func (c *Conn) Fatal() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.poison
}

// AllocateID returns the next monotonically increasing request id without
// sending anything. ThreadStart uses it to bind the creation reservation
// to the request id BEFORE the request is written.
func (c *Conn) AllocateID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// registerPending claims the pending slot for id. Duplicate registration
// is rejected (typed), never silently replaced.
func (c *Conn) registerPending(id int64) (*pendingCall, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrConnClosed
	}
	if c.poison != nil {
		return nil, c.poison
	}
	if _, ok := c.pending[id]; ok {
		return nil, &ErrDuplicateRequestID{ID: id}
	}
	p := &pendingCall{ch: make(chan pendingResult, 1)}
	c.pending[id] = p
	return p, nil
}

func (c *Conn) takePending(id int64) (*pendingCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	return p, ok
}

// Call sends one request and awaits its response. ctx bounds only the
// request write and the ack wait — cancelling it never stops the child,
// the read loop, or the pump.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	return c.CallWithID(ctx, c.AllocateID(), method, params, result)
}

// CallWithID sends a request with a pre-allocated id (creation-reservation
// discipline: reserve, then write) and awaits its response.
func (c *Conn) CallWithID(ctx context.Context, id int64, method string, params, result any) error {
	p, err := c.registerPending(id)
	if err != nil {
		return err
	}
	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{rpcVersion, id, method, params}
	frame, err := json.Marshal(req)
	if err != nil {
		c.takePending(id)
		return fmt.Errorf("marshal %s request: %w", method, err)
	}
	frame = append(frame, '\n')

	before := c.w.bytesWritten()
	if _, err := c.w.Write(frame); err != nil {
		c.takePending(id)
		return &ErrRequestWrite{BytesWritten: c.w.bytesWritten() - before, Err: err}
	}

	select {
	case pr := <-p.ch:
		if pr.err != nil {
			return pr.err
		}
		if pr.rpcErr != nil {
			return pr.rpcErr
		}
		if result == nil || len(pr.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(pr.result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		// Unregister; a late response for this id is dropped by the
		// reader without poisoning.
		c.takePending(id)
		return ctx.Err()
	case <-c.done:
		c.mu.Lock()
		poison := c.poison
		c.mu.Unlock()
		if poison != nil {
			return poison
		}
		return ErrConnClosed
	}
}

// Respond answers a server→client request with a result.
func (c *Conn) Respond(id json.RawMessage, result any) error {
	return c.writeServerReply(id, "result", result)
}

// RespondError answers a server→client request with a JSON-RPC error.
func (c *Conn) RespondError(id json.RawMessage, rpcErr *RPCError) error {
	return c.writeServerReply(id, "error", rpcErr)
}

func (c *Conn) writeServerReply(id json.RawMessage, field string, payload any) error {
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal server reply: %w", err)
	}
	idField := string(bytes.TrimSpace(id))
	if idField == "" || idField == "null" {
		return fmt.Errorf("server reply requires a non-null id")
	}
	frame := fmt.Sprintf(`{"jsonrpc":"%s","id":%s,"%s":%s}`+"\n", rpcVersion, idField, field, payloadRaw)
	if _, err := c.w.Write([]byte(frame)); err != nil {
		return &ErrRequestWrite{BytesWritten: 0, Err: err}
	}
	return nil
}

// NextServerRequest drains the bounded default queue of server→client
// requests. It errors once a handler has been registered (requests then go
// only to the handler).
func (c *Conn) NextServerRequest(timeout time.Duration) (ServerRequest, error) {
	c.reqMu.Lock()
	registered := c.reqHandler != nil
	c.reqMu.Unlock()
	if registered {
		return ServerRequest{}, errors.New("a server request handler is registered; the default queue is not in use")
	}
	select {
	case sr := <-c.reqQueue:
		return sr, nil
	case <-time.After(timeout):
		return ServerRequest{}, errors.New("no server request arrived")
	case <-c.done:
		return ServerRequest{}, ErrConnClosed
	}
}

// Close fails every pending call and stops the connection. It does not
// close the underlying pipes (the child's process owner does that).
func (c *Conn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[int64]*pendingCall)
	c.mu.Unlock()
	c.w.markFull()
	for id, p := range pending {
		p.ch <- pendingResult{err: fmt.Errorf("%w (request id %d)", ErrConnClosed, id)}
	}
	c.doneOnce.Do(func() { close(c.done) })
}

// finish terminates the read loop: pending calls fail, Done closes, and
// the fatal handler (if any) fires once with the terminal cause. A nil
// cause (clean stream end) still fails every pending call with
// ErrConnClosed — a call pending at unexpected child death must never
// return nil with a zero result (fail closed, spec §3.9).
func (c *Conn) finish(cause error) {
	c.mu.Lock()
	if c.poison == nil {
		c.poison = cause
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[int64]*pendingCall)
	c.mu.Unlock()
	c.w.markFull()
	failure := cause
	if failure == nil {
		failure = fmt.Errorf("%w (pending at child stream end)", ErrConnClosed)
	}
	for _, p := range pending {
		p.ch <- pendingResult{err: failure}
	}
	c.doneOnce.Do(func() { close(c.done) })
	if cause != nil {
		c.fatalMu.Lock()
		fn := c.fatalFn
		c.fatalMu.Unlock()
		if fn != nil {
			// Decoupled: the read loop never holds locks while the
			// handler (which may terminate the process) runs.
			go fn(cause)
		}
	}
}

// readFrame reads one newline-terminated frame enforcing MaxFrameBytes.
func readFrame(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			buf = append(buf, chunk...)
			if len(buf) > MaxFrameBytes {
				return nil, &ErrPoisonedFrame{Reason: "oversized frame exceeds the 1 MiB bound"}
			}
			continue
		}
		if err != nil {
			if err == io.EOF && len(buf)+len(chunk) > 0 {
				return nil, &ErrPoisonedFrame{Reason: "malformed frame: unterminated final line", Err: io.ErrUnexpectedEOF}
			}
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > MaxFrameBytes {
			return nil, &ErrPoisonedFrame{Reason: "oversized frame exceeds the 1 MiB bound"}
		}
		return buf, nil
	}
}

func (c *Conn) readLoop() {
	br := bufio.NewReaderSize(c.stdout, 64<<10)
	for {
		raw, err := readFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Clean stream end: the child closed stdout (exit).
				c.finish(nil)
				return
			}
			var poisoned *ErrPoisonedFrame
			if !errors.As(err, &poisoned) {
				poisoned = &ErrPoisonedFrame{Reason: "framing reader failure", Err: err}
			}
			c.finish(poisoned)
			return
		}
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			c.finish(&ErrPoisonedFrame{Reason: "malformed frame: empty line"})
			return
		}
		var head frameHead
		if err := json.Unmarshal(line, &head); err != nil {
			c.finish(&ErrPoisonedFrame{Reason: "malformed frame: not a JSON object", Err: err})
			return
		}
		switch {
		case len(head.ID) > 0 && head.Method == "":
			c.routeResponse(&head)
		case len(head.ID) > 0 && head.Method != "":
			if !c.routeServerRequest(ServerRequest{ID: head.ID, Method: head.Method, Params: head.Params}) {
				c.finish(&ErrPoisonedFrame{Reason: "malformed stream: server request queue overflow (approval evidence loss)"})
				return
			}
		case head.Method != "":
			c.notifMu.Lock()
			fn := c.notifFn
			c.notifMu.Unlock()
			if fn != nil {
				fn(head.Method, head.Params)
			}
		default:
			c.finish(&ErrPoisonedFrame{Reason: "malformed frame: neither request, response, nor notification"})
			return
		}
	}
}

func (c *Conn) routeResponse(head *frameHead) {
	id, ok := parseID(head.ID)
	if !ok {
		// A response whose id we cannot correlate is dropped; it can
		// never poison (ids are ours, always numeric, echoed verbatim).
		return
	}
	p, ok := c.takePending(id)
	if !ok {
		return // late reply for a timed-out caller: drop, not poison
	}
	if head.Error != nil {
		p.ch <- pendingResult{rpcErr: head.Error}
		return
	}
	p.ch <- pendingResult{result: head.Result}
}

// parseID decodes a JSON-RPC id that we sent as a JSON number.
func parseID(raw json.RawMessage) (int64, bool) {
	s := string(bytes.TrimSpace(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// routeServerRequest hands the request to the registered handler or the
// bounded default queue. Returns false only on queue overflow.
func (c *Conn) routeServerRequest(sr ServerRequest) bool {
	c.reqMu.Lock()
	fn := c.reqHandler
	c.reqMu.Unlock()
	if fn != nil {
		c.serverReqWG.Add(1)
		go func() {
			defer c.serverReqWG.Done()
			fn(sr)
		}()
		return true
	}
	select {
	case c.reqQueue <- sr:
		return true
	default:
		return false
	}
}
