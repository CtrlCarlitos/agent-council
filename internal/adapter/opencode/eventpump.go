package opencode

// Adapter-owned per-session SSE event pump (spec §3.4): one continuously
// drained native stream per active session, owned by the adapter — never
// by an Observe caller. Events route to per-TurnRef bounded buffers via
// the (native session, parentID) → turn registry. A caller detaching from
// a tap never closes the native stream; a slow consumer overflows its own
// tap only. Permission requests are denied by default and mirrored into
// the owning turn's stream as explicit tool-requested / tool-denied
// events.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// sessionPump drains one session's native SSE stream.
type sessionPump struct {
	nativeID string
	client   *NativeClient

	mu     sync.Mutex
	cursor int64               // last seen native event id
	turns  map[string]*turnTap // user message ID → tap

	loopOnce sync.Once

	stopCh   chan struct{}
	stopOnce sync.Once
}

// turnTap couples one turn's buffered stream with its deterministic user
// message ID.
type turnTap struct {
	ref    adapter.TurnRef
	stream *adapter.BufferedStream
	owner  *OpenCodeAdapter
	done   <-chan struct{} // closed when the Observe caller cancels; may be nil
}

func newSessionPump(nativeID string, client *NativeClient) *sessionPump {
	return &sessionPump{
		nativeID: nativeID,
		client:   client,
		turns:    make(map[string]*turnTap),
		stopCh:   make(chan struct{}),
	}
}

// register attaches a turn tap for the given deterministic user message
// ID and ensures the pump loop is running.
func (p *sessionPump) register(userMessageID string, tap *turnTap) {
	p.mu.Lock()
	p.turns[userMessageID] = tap
	p.mu.Unlock()
	p.loopOnce.Do(func() { go p.loop() })
}

// detach removes a turn tap: the caller ended its subscription. The
// native stream keeps draining.
func (p *sessionPump) detach(userMessageID string) {
	p.mu.Lock()
	tap, ok := p.turns[userMessageID]
	delete(p.turns, userMessageID)
	p.mu.Unlock()
	if ok {
		_ = tap.stream.Close()
	}
}

// stop ends the pump. The native connection is closed by the adapter when
// the session's server stops; stopping the pump only ends the drain loop.
func (p *sessionPump) stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
}

// route delivers an event to the tap registered for parentID. A slow or
// detached consumer overflows only its own tap; the native stream is
// unaffected.
func (p *sessionPump) route(parentID string, ev adapter.Event) bool {
	p.mu.Lock()
	tap, ok := p.turns[parentID]
	p.mu.Unlock()
	if !ok {
		return false
	}
	ev.Ref = tap.ref
	if err := tap.stream.SendOrOverflow(ev); err != nil {
		p.mu.Lock()
		delete(p.turns, parentID)
		p.mu.Unlock()
		_ = tap.stream.CloseWithErr(err)
		return false
	}
	return true
}

// loop drains the native stream until the pump is stopped. On transport
// loss it resyncs from message history (the deterministic message ID makes
// resync unambiguous) and reconnects with the recorded cursor.
func (p *sessionPump) loop() {
	for {
		select {
		case <-p.stopCh:
			return
		default:
		}
		if !p.drainOnce() {
			return
		}
		select {
		case <-p.stopCh:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// drainOnce opens one native SSE connection and consumes it. Returns
// false when the pump is stopped.
func (p *sessionPump) drainOnce() bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-p.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	p.mu.Lock()
	cursor := p.cursor
	p.mu.Unlock()
	sc, resp, err := p.client.Events(ctx, p.nativeID, cursor)
	if err != nil {
		if IsPostWriteError(err) || ctx.Err() != nil {
			// Ambiguous loss or pump stopped: resync from history on the
			// next pass rather than treating the stream as authoritative.
			p.resyncFromHistory()
			return ctx.Err() == nil
		}
		p.resyncFromHistory()
		return ctx.Err() == nil
	}
	defer resp.Body.Close()

	for {
		select {
		case <-p.stopCh:
			return false
		default:
		}
		line, ok := readSSELine(sc)
		if !ok {
			break // stream ended; reconnect
		}
		kind, value := sseFields(line)
		switch kind {
		case "id":
			if id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				p.mu.Lock()
				if id > p.cursor {
					p.cursor = id
				}
				p.mu.Unlock()
			}
		case "data":
			p.handleEvent(value)
		}
	}
	return true
}

// handleEvent routes one native event payload.
func (p *sessionPump) handleEvent(payload string) {
	var ev struct {
		Type       string `json:"type"`
		MessageID  string `json:"messageID"`
		ParentID   string `json:"parentID"`
		Permission *struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"permission"`
		Part *struct {
			Text string `json:"text"`
		} `json:"part"`
		Error *NativeMessageError `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return // unparseable native events are dropped, never guessed
	}

	parentID := ev.ParentID
	if parentID == "" {
		parentID = ev.MessageID
	}

	switch ev.Type {
	case "permission.requested":
		p.handlePermission(ev.Permission, parentID)
	case "message.updated":
		text := ""
		if ev.Part != nil {
			text = ev.Part.Text
		}
		p.route(parentID, adapter.Event{
			Type:    adapter.EventProgress,
			Status:  council.TurnRunning,
			Payload: text,
		})
	case "message.error":
		p.route(parentID, adapter.Event{
			Type:    adapter.EventTerminal,
			Status:  council.TurnFailed,
			Payload: errorMessage(ev.Error),
		})
	case "message.completed":
		p.route(parentID, adapter.Event{
			Type:   adapter.EventTerminal,
			Status: council.TurnCompleted,
		})
	}
}

// handlePermission answers a permission request with an explicit denial
// and mirrors the decision into the owning turn's stream. The adapter
// never approves: profiles are not a tool-approval authority.
func (p *sessionPump) handlePermission(perm *struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}, parentID string) {
	if perm == nil || perm.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	replyErr := p.client.PermissionReply(ctx, p.nativeID, perm.ID, NativePermissionReply{Response: "denied"})

	p.route(parentID, adapter.Event{
		Type:       adapter.EventToolRequested,
		Status:     council.TurnRunning,
		ApprovalID: perm.ID,
		Payload:    perm.Type,
	})
	denied := adapter.Event{
		Type:       adapter.EventToolDenied,
		Status:     council.TurnRunning,
		ApprovalID: perm.ID,
		Payload:    "denied by council: no approval authority is configured",
	}
	if replyErr != nil {
		denied.Payload = fmt.Sprintf("deny reply failed: %v", replyErr)
	}
	p.route(parentID, denied)
}

// resyncFromHistory reconstructs terminal state from message history after
// a transport loss. The deterministic user-message ID makes the assistant
// reply unambiguous.
func (p *sessionPump) resyncFromHistory() {
	p.mu.Lock()
	pending := make(map[string]*turnTap, len(p.turns))
	for id, tap := range p.turns {
		pending[id] = tap
	}
	p.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := p.client.ListMessages(ctx, p.nativeID)
	if err != nil {
		return // history unavailable; retry on the next reconnect
	}
	byParent := map[string]*NativeMessage{}
	for i := range msgs {
		if msgs[i].Role == "assistant" && msgs[i].ParentID != "" {
			byParent[msgs[i].ParentID] = &msgs[i]
		}
	}
	for userMsgID, tap := range pending {
		asst, ok := byParent[userMsgID]
		if !ok {
			continue // turn still unanswered
		}
		text := ""
		for _, part := range asst.Parts {
			if part.Type == "text" {
				text = part.Text
				break
			}
		}
		ev := adapter.Event{Type: adapter.EventTerminal, Status: council.TurnCompleted, Payload: text}
		if asst.Error != nil {
			ev.Status = council.TurnFailed
			ev.Payload = asst.Error.Message
		}
		if p.route(userMsgID, ev) {
			tap.owner.mu.Lock()
			if d, ok := tap.owner.dispatches[tap.ref]; ok {
				d.terminal = true
			}
			tap.owner.mu.Unlock()
		}
	}
}

func errorMessage(e *NativeMessageError) string {
	if e == nil {
		return ""
	}
	return e.Message
}

// readSSELine reads the next non-empty SSE line. ok is false when the
// stream ended.
func readSSELine(sc interface {
	Scan() bool
	Text() string
}) (string, bool) {
	for sc != nil && sc.Scan() {
		line := sc.Text()
		if line != "" {
			return line, true
		}
	}
	return "", false
}

func sseFields(line string) (kind, value string) {
	if idx := strings.Index(line, ":"); idx != -1 {
		return line[:idx], strings.TrimPrefix(line[idx+1:], " ")
	}
	return strings.TrimSpace(line), ""
}
