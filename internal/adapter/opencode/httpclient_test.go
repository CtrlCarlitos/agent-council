package opencode

// Typed client evidence against the fake OpenCode server: every endpoint
// the adapter contract requires — health, model inventory, session
// lifecycle, messages, prompt dispatch, abort, per-session SSE events,
// and permission handling — exercised with authentication, ledger
// verification, and transport classification.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func typedClientFixture(t *testing.T) (*NativeClient, *fakeOpenCodeServer) {
	t.Helper()
	fake, endpoint := startFakeServerWithAuth(t, gate1User, gate1Password)
	fake.mu.Lock()
	fake.sessions[gate1Session] = &fakeSession{id: gate1Session}
	fake.mu.Unlock()
	return newNativeClient(endpoint, gate1User, gate1Password), fake
}

func TestNativeClient_HealthAndModelsAuthenticated(t *testing.T) {
	client, fake := typedClientFixture(t)
	ctx := context.Background()

	if err := client.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	if _, err := client.Models(ctx); err != nil {
		t.Fatalf("models: %v", err)
	}
	if got := fake.ledger.modelCalls; got != 1 {
		t.Fatalf("expected 1 model call, got %d", got)
	}
	if got := fake.ledger.authFailureCount(); got != 0 {
		t.Fatalf("authenticated calls must not fail auth, failures: %d", got)
	}

	// An unauthenticated client must be rejected with 401.
	fake2, endpoint := startFakeServerWithAuth(t, gate1User, gate1Password)
	_ = fake2
	unauth := newNativeClient(endpoint, gate1User, "wrong")
	if err := unauth.Health(ctx); err == nil {
		t.Fatal("unauthenticated health must fail")
	}
}

func TestNativeClient_CreateSessionAndDirectoryRejection(t *testing.T) {
	client, fake := typedClientFixture(t)
	ctx := context.Background()

	id, err := client.CreateSession(ctx, "council test", "/allowed/dir")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if id == "" {
		t.Fatal("server-assigned native session ID must not be empty")
	}

	// Directory-context rejection: a server bound to one working directory
	// must refuse sessions for another.
	fake.setWorkspaceDir("/allowed/dir")
	if _, err := client.CreateSession(ctx, "council test", "/elsewhere"); err == nil {
		t.Fatal("mismatched directory context must be rejected")
	}
	if got := fake.ledger.directoryRejections; got != 1 {
		t.Fatalf("expected 1 recorded directory rejection, got %d", got)
	}
}

func TestNativeClient_MessageLifecycle(t *testing.T) {
	client, fake := typedClientFixture(t)
	ctx := context.Background()

	if err := client.PromptAsync(ctx, gate1Session, "msg_user_1", "hello"); err != nil {
		t.Fatalf("prompt_async: %v", err)
	}
	if fake.ledger.promptAsyncCount() != 1 {
		t.Fatal("prompt must hit the native endpoint once")
	}

	// Exact message lookup: found and verified-404 paths.
	if _, found, err := client.GetMessage(ctx, gate1Session, "msg_user_1"); err != nil || !found {
		t.Fatalf("exact message must be found, found=%v err=%v", found, err)
	}
	if _, found, err := client.GetMessage(ctx, gate1Session, "msg_missing"); err != nil || found {
		t.Fatalf("missing message must be a verified 404, found=%v err=%v", found, err)
	}

	msgs, err := client.ListMessages(ctx, gate1Session)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].ParentID != "msg_user_1" {
		t.Fatalf("unexpected message list: %+v", msgs)
	}

	if err := client.Abort(ctx, gate1Session); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if fake.ledger.abortCount() != 1 {
		t.Fatal("abort must hit the native endpoint once")
	}

	// Session existence: known and unknown.
	if exists, err := client.GetSession(ctx, gate1Session); err != nil || !exists {
		t.Fatalf("session must exist, exists=%v err=%v", exists, err)
	}
	if exists, err := client.GetSession(ctx, "sess-nope"); err != nil || exists {
		t.Fatalf("unknown session must be a verified 404, exists=%v err=%v", exists, err)
	}
}

func TestNativeClient_SSEScriptedEventsWithTiming(t *testing.T) {
	client, fake := typedClientFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const delay = 250 * time.Millisecond
	fake.scriptEvents(gate1Session, delay,
		`{"type":"message.updated","parts":[{"type":"text","text":"chunk 1"}]}`,
		`{"type":"message.updated","parts":[{"type":"text","text":"chunk 2"}]}`,
	)

	start := time.Now()
	sc, resp, err := client.Events(ctx, gate1Session)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()

	// The first event must not arrive before the scripted delay.
	var first string
	var firstAt time.Time
	for {
		payload, ok := ScanNativeEvent(sc)
		if ok {
			first = payload
			firstAt = time.Now()
			break
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("no SSE event before deadline: %v", err)
		}
	}
	if elapsed := firstAt.Sub(start); elapsed < delay {
		t.Fatalf("scripted event timing violated: first event after %v, delay %v", elapsed, delay)
	}
	if first != `{"type":"message.updated","parts":[{"type":"text","text":"chunk 1"}]}` {
		t.Fatalf("unexpected first event payload: %q", first)
	}

	// The ledger must record the exact approved native endpoint path.
	if got := fake.ledger.sseConnects; len(got) != 1 || got[0] != "/session/"+gate1Session+"/event" {
		t.Fatalf("SSE must use the native /event endpoint, ledger: %v", got)
	}
}

// A body that begins transmitting and then fails mid-write classifies as
// post-write ambiguity, not as a safe retry.
func TestNativeClient_PartialBodyTransmissionIsPostWrite(t *testing.T) {
	// Simulate the transport consuming one body byte and then failing:
	// transmission has begun, so the outcome is ambiguous regardless of
	// how much of the body reached the server.
	tb := newTransmissionBody(io.MultiReader(
		strings.NewReader("x"),
		errReader{},
	))
	buf := make([]byte, 8)
	n, err := tb.Read(buf)
	if n != 1 || err != nil {
		t.Fatalf("first read must succeed with 1 byte, got n=%d err=%v", n, err)
	}
	if !tb.began.Load() {
		t.Fatal("first successful read must mark transmission begun")
	}
	if _, err := tb.Read(buf); err == nil {
		t.Fatal("second read must surface the transport error")
	}
	if !tb.began.Load() {
		t.Fatal("transmission state must remain set")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset mid-body") }

// A connection that dies before any body byte is transmitted remains a
// rejection: the server cannot have seen the turn.
func TestNativeClient_PreBodyFailureIsRejected(t *testing.T) {
	// Reserve a port and close it: dial fails before any write.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	endpoint := fmt.Sprintf("http://%s", ln.Addr().String())
	_ = ln.Close()

	client := newNativeClient(endpoint, gate1User, gate1Password)
	err = client.PromptAsync(context.Background(), gate1Session, "msg_x", "hi")
	if err == nil {
		t.Fatal("dial failure must fail the request")
	}
	if IsPostWriteError(err) {
		t.Fatalf("failure before body transmission must not be post-write, got %v", err)
	}
}

func TestNativeClient_PermissionRequestsDeniedByDefault(t *testing.T) {
	client, fake := typedClientFixture(t)
	ctx := context.Background()

	fake.addPermission(gate1Session, "perm_1", "bash")

	perms, err := client.PermissionRequests(ctx, gate1Session)
	if err != nil {
		t.Fatalf("permission requests: %v", err)
	}
	if len(perms) != 1 || perms[0].ID != "perm_1" || perms[0].Status != "pending" {
		t.Fatalf("expected pending perm_1, got %+v", perms)
	}

	// Council denies permission requests by default.
	if err := client.PermissionReply(ctx, gate1Session, "perm_1", NativePermissionReply{Response: "denied"}); err != nil {
		t.Fatalf("permission reply: %v", err)
	}
	if status := fake.permissionStatus(gate1Session, "perm_1"); status != "denied" {
		t.Fatalf("permission must be denied, got %q", status)
	}
	if got := fake.ledger.permissionReplies; len(got) != 1 || got[0] != gate1Session+"/perm_1" {
		t.Fatalf("unexpected permission reply ledger: %v", got)
	}
}

// Pre-write refusal is a plain error (never classified post-write); the
// ErrPostWrite classification is proven by the hijack faults in the Gate 1
// dispatch tests.
func TestNativeClient_TransportClassification(t *testing.T) {
	// Reserve a port and close it: connections to it are refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	endpoint := fmt.Sprintf("http://%s", ln.Addr().String())
	_ = ln.Close()

	client := newNativeClient(endpoint, gate1User, gate1Password)

	err = client.Health(context.Background())
	if err == nil {
		t.Fatal("health against a dead endpoint must fail")
	}
	if IsPostWriteError(err) {
		t.Fatalf("connection refusal before the write must not be post-write, got %v", err)
	}
}
