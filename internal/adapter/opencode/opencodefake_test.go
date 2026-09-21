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

type fakePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type fakeProbeError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
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
		sess, ok := f.sessions[r.PathValue("sessionID")]
		if !ok {
			http.Error(w, "not found", 404)
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
		if sess, ok := f.sessions[r.PathValue("sessionID")]; ok {
			sess.abortRequested = true
		}
		w.WriteHeader(200)
	})
	return mux
}

// startFakeServer launches the fake server and returns its endpoint.
func startFakeServer(t *testing.T) (*fakeOpenCodeServer, string) {
	t.Helper()
	f := &fakeOpenCodeServer{sessions: make(map[string]*fakeSession)}
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
