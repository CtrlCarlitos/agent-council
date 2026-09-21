package opencode

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
)

// fakeOpenCodeServer implements the verified endpoint subset for CI
// (provider-free). It is NOT a production component.
type fakeOpenCodeServer struct {
	mu        sync.Mutex
	endpoint  string
	sessions  map[string]*fakeSession
	connected bool

	// expectedUser/expectedPass enable Basic-auth enforcement when
	// expectedUser is non-empty. Mismatched or missing credentials are
	// recorded in the ledger and rejected with 401.
	expectedUser string
	expectedPass string

	// flakyPromptAsync is a one-shot fault injected into the next
	// prompt_async call. "" disables it. "drop-before-record" hijacks the
	// connection after reading the request but before recording state
	// (client sees a post-write failure; nothing persisted).
	// "drop-after-record" records the message first, then hijacks the
	// connection (client sees a post-write failure; state persisted).
	flakyPromptAsync string

	ledger fakeRequestLedger
}

// fakeRequestLedger is a thread-safe record of the native HTTP requests the
// fake server actually received. Contract tests assert against it instead of
// adapter-internal state.
type fakeRequestLedger struct {
	mu                    sync.Mutex
	promptAsyncCalls      int
	promptAsyncSessions   []string
	promptAsyncMessageIDs []string
	abortCalls            int
	authFailures          int
}

func (l *fakeRequestLedger) recordPromptAsync(sessionID, messageID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.promptAsyncCalls++
	l.promptAsyncSessions = append(l.promptAsyncSessions, sessionID)
	l.promptAsyncMessageIDs = append(l.promptAsyncMessageIDs, messageID)
}

func (l *fakeRequestLedger) recordAbort() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.abortCalls++
}

func (l *fakeRequestLedger) recordAuthFailure() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.authFailures++
}

func (l *fakeRequestLedger) promptAsyncCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.promptAsyncCalls
}

func (l *fakeRequestLedger) promptAsyncMessageID(i int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.promptAsyncMessageIDs[i]
}

func (l *fakeRequestLedger) abortCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.abortCalls
}

func (l *fakeRequestLedger) authFailureCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.authFailures
}

type fakeSession struct {
	id             string
	title          string
	abortRequested bool
	messages       []fakeMessage
}

type fakeMessage struct {
	ID       string          `json:"id"`
	Role     string          `json:"role"`
	ParentID string          `json:"parentID,omitempty"`
	Parts    []fakePart      `json:"parts"`
	Error    *fakeProbeError `json:"error,omitempty"`
}

func (f *fakeOpenCodeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"healthy": true})
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
			Model struct {
				ProviderID string `json:"providerID"`
				ID         string `json:"id"`
			} `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		id := fmt.Sprintf("ses_fake_%d", len(f.sessions)+1)
		sess := &fakeSession{id: id, title: body.Title}
		f.sessions[id] = sess
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    id,
			"title": body.Title,
			"cost":  0,
		})
	})
	mux.HandleFunc("GET /session/{sessionID}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		sess, ok := f.sessions[r.PathValue("sessionID")]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": sess.id, "title": sess.title})
	})
	mux.HandleFunc("POST /session/{sessionID}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MessageID string `json:"messageID"`
			Parts     []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
			Model struct {
				ProviderID string `json:"providerID"`
				ModelID    string `json:"modelID"`
			} `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		sessionID := r.PathValue("sessionID")
		sess, ok := f.sessions[sessionID]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		f.ledger.recordPromptAsync(sessionID, body.MessageID)
		mode := f.flakyPromptAsync
		f.flakyPromptAsync = ""
		if mode == "drop-before-record" {
			// Simulate a post-write disconnect: the request was fully
			// read but the connection dies before any response.
			conn, _, _ := w.(http.Hijacker).Hijack()
			if conn != nil {
				conn.Close()
			}
			return
		}
		if sess.abortRequested {
			http.Error(w, "aborted", 409)
			return
		}
		userMsg := fakeMessage{
			ID:   body.MessageID,
			Role: "user",
		}
		for _, p := range body.Parts {
			userMsg.Parts = append(userMsg.Parts, fakePart{Type: p.Type, Text: p.Text})
		}
		sess.messages = append(sess.messages, userMsg)
		if mode == "drop-after-record" {
			// The server processed and persisted the turn, but the client
			// never sees the response.
			conn, _, _ := w.(http.Hijacker).Hijack()
			if conn != nil {
				conn.Close()
			}
			return
		}
		// Script an assistant reply with parentID linkage.
		asstID := fmt.Sprintf("msg_asst_%d", len(sess.messages)+1000)
		asstMsg := fakeMessage{
			ID:       asstID,
			Role:     "assistant",
			ParentID: body.MessageID,
			Parts:    []fakePart{{Type: "text", Text: "fake assistant response"}},
		}
		sess.messages = append(sess.messages, asstMsg)
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /session/{sessionID}/message", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		sess, ok := f.sessions[r.PathValue("sessionID")]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		var msgs []map[string]any
		for _, m := range sess.messages {
			parts := make([]map[string]string, len(m.Parts))
			for i, p := range m.Parts {
				parts[i] = map[string]string{"type": p.Type, "text": p.Text}
			}
			entry := map[string]any{
				"info": map[string]any{
					"id":       m.ID,
					"role":     m.Role,
					"parentID": m.ParentID,
				},
				"parts": parts,
			}
			msgs = append(msgs, entry)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(msgs)
	})
	mux.HandleFunc("GET /session/{sessionID}/message/{messageID}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, m := range f.sessions[r.PathValue("sessionID")].messages {
			if m.ID == r.PathValue("messageID") {
				parts := make([]map[string]string, len(m.Parts))
				for i, p := range m.Parts {
					parts[i] = map[string]string{"type": p.Type, "text": p.Text}
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"info":  map[string]any{"id": m.ID, "role": m.Role, "parentID": m.ParentID},
					"parts": parts,
				})
				return
			}
		}
		http.Error(w, "not found", 404)
	})
	mux.HandleFunc("POST /session/{sessionID}/abort", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ledger.recordAbort()
		if sess, ok := f.sessions[r.PathValue("sessionID")]; ok {
			sess.abortRequested = true
		}
		w.WriteHeader(200)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.expectedUser != "" {
			user, pass, ok := r.BasicAuth()
			if !ok || user != f.expectedUser || pass != f.expectedPass {
				f.ledger.recordAuthFailure()
				w.Header().Set("WWW-Authenticate", `Basic realm="opencode"`)
				http.Error(w, "unauthorized", 401)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// armFlakyPromptAsync injects a one-shot post-write disconnect into the next
// prompt_async call. mode is "drop-before-record" or "drop-after-record".
func (f *fakeOpenCodeServer) armFlakyPromptAsync(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flakyPromptAsync = mode
}

// messageExists reports whether the given user message ID was recorded for
// the session (post-record fault semantics).
func (f *fakeOpenCodeServer) messageExists(sessionID, messageID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.sessions[sessionID]
	if !ok {
		return false
	}
	for _, m := range sess.messages {
		if m.ID == messageID {
			return true
		}
	}
	return false
}

// startFakeServer launches the fake server (auth disabled) and returns it
// with its endpoint.
func startFakeServer(t *testing.T) (*fakeOpenCodeServer, string) {
	t.Helper()
	return startFakeServerWithAuth(t, "", "")
}

// startFakeServerWithAuth launches the fake server enforcing Basic auth with
// the given credentials. Returned credentials match what a launched
// serverProcess would retain from GeneratedServerEnv.
func startFakeServerWithAuth(t *testing.T, user, pass string) (*fakeOpenCodeServer, string) {
	t.Helper()
	f := &fakeOpenCodeServer{
		sessions:     make(map[string]*fakeSession),
		expectedUser: user,
		expectedPass: pass,
	}
	hs := &http.Server{Handler: f.handler()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.endpoint = fmt.Sprintf("http://%s", ln.Addr().String())
	go hs.Serve(ln)
	t.Cleanup(func() { _ = hs.Close() })
	return f, f.endpoint
}
