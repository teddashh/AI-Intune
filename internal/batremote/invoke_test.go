package batremote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	invokeResultFixture = `{"id":"t2","result":{"activeProfileIds":["default"],"profiles":[{"createdAt":0,"id":"default","name":"Default","type":"local","updatedAt":0}]},"type":"invoke-result"}`
	invokeErrorFixture  = "{\"error\":\"pty:create: invalid options: missing field `id`\",\"id\":\"p1\",\"type\":\"invoke-error\"}"
	ptyOutputFixture    = `{"args":["tt","echo hi\r\n"],"channel":"pty:output","params":{"data":"echo hi\r\n","id":"tt"},"type":"event"}`
	ptyExitFixture      = `{"channel":"pty:exit","params":{"exitCode":7,"id":"yz"},"type":"event"}`
	notificationFixture = `{"args":[{"cwd":"","id":"notif-1790137379916-1","kind":"remote-client","profileId":null,"read":false,"reason":"connected","sessionId":"","timestamp":1790137379916,"title":"probe","windowId":null,"workspaceName":""}],"channel":"notification:update","params":[{"cwd":"","id":"notif-1790137379916-1","kind":"remote-client","profileId":null,"read":false,"reason":"connected","sessionId":"","timestamp":1790137379916,"title":"probe","windowId":null,"workspaceName":""}],"type":"event"}`
)

type observedInvoke struct {
	fields map[string]json.RawMessage
	frame  []byte
}

type writeStartedConn struct {
	net.Conn
	mu        sync.Mutex
	nextWrite chan struct{}
}

func (c *writeStartedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	started := c.nextWrite
	c.nextWrite = nil
	c.mu.Unlock()
	if started != nil {
		close(started)
	}
	return c.Conn.Write(p)
}

func (c *writeStartedConn) arm() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextWrite = make(chan struct{})
	return c.nextWrite
}

func observeClientWrites(p *pipeServer) *writeStartedConn {
	observed := &writeStartedConn{}
	connect := p.connect
	p.connect = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := connect(ctx, network, address)
		if err != nil {
			return nil, err
		}
		observed.Conn = conn
		return observed, nil
	}
	return observed
}

func newInvokePipeServer(t *testing.T, handle func(context.Context, *websocket.Conn) error) *pipeServer {
	t.Helper()
	return newPipeServer(t, func(ctx context.Context, conn *websocket.Conn, auth map[string]any) error {
		data, err := json.Marshal(successfulReply(auth))
		if err != nil {
			return err
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			return err
		}
		return handle(ctx, conn)
	})
}

func readInvoke(ctx context.Context, conn *websocket.Conn) (observedInvoke, error) {
	kind, frame, err := conn.Read(ctx)
	if err != nil {
		return observedInvoke{}, err
	}
	if kind != websocket.MessageText {
		return observedInvoke{}, fmt.Errorf("invoke message type = %v", kind)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(frame, &fields); err != nil {
		return observedInvoke{}, err
	}
	return observedInvoke{fields: fields, frame: frame}, nil
}

func invokeID(request observedInvoke) (string, error) {
	var id string
	if err := json.Unmarshal(request.fields["id"], &id); err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("empty invoke ID")
	}
	return id, nil
}

func writeResult(ctx context.Context, conn *websocket.Conn, id, result string) error {
	frame := fmt.Sprintf(`{"id":%q,"result":%s,"type":"invoke-result"}`, id, result)
	return conn.Write(ctx, websocket.MessageText, []byte(frame))
}

func dialInvokePipe(t *testing.T, p *pipeServer) *Session {
	t.Helper()
	session, err := dialPipe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func waitSessionError(t *testing.T, session *Session, want error) {
	t.Helper()
	select {
	case <-session.runtime.done:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not terminate")
	}
	if err := session.Err(); !errors.Is(err, want) {
		t.Fatalf("session error = %v; want %v", err, want)
	}
}

func waitForPendingInvokes(t *testing.T, session *Session, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		session.runtime.stateMu.Lock()
		got := len(session.runtime.pending)
		session.runtime.stateMu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending invokes did not reach %d", want)
}

func waitForWriteStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("client write did not start")
	}
}

func assertSessionAlive(t *testing.T, session *Session) {
	t.Helper()
	if err := session.Err(); err != nil {
		t.Fatalf("session error = %v; want nil", err)
	}
	select {
	case _, ok := <-session.Events():
		if !ok {
			t.Fatal("events channel was closed")
		}
		t.Fatal("unexpected event")
	default:
	}
}

func TestInvokeRequestCarriesInvokeType(t *testing.T) {
	requests := make(chan observedInvoke, 1)
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		requests <- request
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		return writeResult(ctx, conn, id, `{}`)
	})
	session := dialInvokePipe(t, p)
	params := map[string]any{"rows": 24, "nested": map[string]any{"enabled": true}}
	if _, err := session.Invoke(context.Background(), "terminal.resize", params); err != nil {
		t.Fatal(err)
	}

	request := <-requests
	if len(request.fields) != 4 {
		t.Fatalf("request fields = %s", request.frame)
	}
	var frameType string
	if err := json.Unmarshal(request.fields["type"], &frameType); err != nil || frameType != frameTypeInvoke {
		t.Fatalf("type = %q, %v", frameType, err)
	}
	if id, err := invokeID(request); err != nil || id == "" {
		t.Fatalf("request ID invalid: %q, %v", id, err)
	}
	var channel string
	if err := json.Unmarshal(request.fields["channel"], &channel); err != nil || channel != "terminal.resize" {
		t.Fatalf("channel = %q, %v", channel, err)
	}
	wantParams, _ := json.Marshal(params)
	if string(request.fields["params"]) != string(wantParams) {
		t.Fatalf("params = %s; want %s", request.fields["params"], wantParams)
	}
}

func TestInvokeAcceptsInvokeResult(t *testing.T) {
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		frame := strings.Replace(invokeResultFixture, `"t2"`, fmt.Sprintf("%q", id), 1)
		return conn.Write(ctx, websocket.MessageText, []byte(frame))
	})
	session := dialInvokePipe(t, p)
	result, err := session.Invoke(context.Background(), "profile:list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"activeProfileIds":["default"],"profiles":[{"createdAt":0,"id":"default","name":"Default","type":"local","updatedAt":0}]}`
	if string(result) != want {
		t.Fatalf("result = %s; want %s", result, want)
	}
}

func TestInvokeAcceptsInvokeError(t *testing.T) {
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		frame := strings.Replace(invokeErrorFixture, `"p1"`, fmt.Sprintf("%q", id), 1)
		return conn.Write(ctx, websocket.MessageText, []byte(frame))
	})
	session := dialInvokePipe(t, p)
	result, err := session.Invoke(context.Background(), "pty:create", map[string]any{})
	if result != nil {
		t.Fatalf("error returned result %s", result)
	}
	if !errors.Is(err, ErrInvoke) {
		t.Fatalf("error = %v; want %v", err, ErrInvoke)
	}
	const reason = "pty:create: invalid options: missing field `id`"
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("server reason was not retained: %v", err)
	}
}

func TestMatchedIDWithWrongTypeIsInvalidResponse(t *testing.T) {
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		frame := fmt.Sprintf(`{"id":%q,"result":{},"type":"event"}`, id)
		return conn.Write(ctx, websocket.MessageText, []byte(frame))
	})
	session := dialInvokePipe(t, p)
	result, err := session.Invoke(context.Background(), "status", map[string]any{})
	if result != nil || !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Invoke() = %s, %v; want nil, %v", result, err, ErrInvalidResponse)
	}
	select {
	case event := <-session.Events():
		t.Fatalf("matched response was routed as event: %s", event.Frame)
	default:
	}
}

func TestInvokeReturnsResult(t *testing.T) {
	const result = `{ "ok": true, "items": [1, 2, 3] }`
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		return writeResult(ctx, conn, id, result)
	})
	session := dialInvokePipe(t, p)
	got, err := session.Invoke(context.Background(), "status", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != result {
		t.Fatalf("result = %s; want %s", got, result)
	}
}

func TestInvokeReturnsError(t *testing.T) {
	const reason = "Remote server is busy; retry this request shortly"
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		frame := fmt.Sprintf(`{"type":"invoke-error","id":%q,"error":%q}`, id, reason)
		return conn.Write(ctx, websocket.MessageText, []byte(frame))
	})
	session := dialInvokePipe(t, p)
	result, err := session.Invoke(context.Background(), "status", nil)
	if result != nil {
		t.Fatalf("error returned result %s", result)
	}
	if !errors.Is(err, ErrInvoke) {
		t.Fatalf("error = %v; want %v", err, ErrInvoke)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("server reason was not retained: %v", err)
	}
}

func TestInvokeErrorRedactsToken(t *testing.T) {
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		frame := fmt.Sprintf(`{"type":"invoke-error","id":%q,"error":%q}`, id, "rejected "+sentinelToken)
		return conn.Write(ctx, websocket.MessageText, []byte(frame))
	})
	session := dialInvokePipe(t, p)
	_, err := session.Invoke(context.Background(), "status", nil)
	if !errors.Is(err, ErrInvoke) || strings.Contains(err.Error(), sentinelToken) {
		t.Fatalf("unsafe invocation error: %v", err)
	}
}

func TestInvokeMatchesRepliesOutOfOrder(t *testing.T) {
	const count = 3
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		requests := make([]observedInvoke, count)
		for i := range requests {
			request, err := readInvoke(ctx, conn)
			if err != nil {
				return err
			}
			requests[i] = request
		}
		for i := len(requests) - 1; i >= 0; i-- {
			id, err := invokeID(requests[i])
			if err != nil {
				return err
			}
			if err := writeResult(ctx, conn, id, string(requests[i].fields["params"])); err != nil {
				return err
			}
		}
		return nil
	})
	session := dialInvokePipe(t, p)

	var wg sync.WaitGroup
	errorsByCall := make(chan error, count)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := session.Invoke(context.Background(), "echo", map[string]int{"value": i})
			if err != nil {
				errorsByCall <- err
				return
			}
			var got map[string]int
			if err := json.Unmarshal(result, &got); err != nil || got["value"] != i {
				errorsByCall <- fmt.Errorf("call %d got %s: %v", i, result, err)
			}
		}()
	}
	wg.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		t.Error(err)
	}
}

func TestInvokeIDsAreUnique(t *testing.T) {
	const count = 20
	ids := make(chan string, count)
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		for range count {
			request, err := readInvoke(ctx, conn)
			if err != nil {
				return err
			}
			id, err := invokeID(request)
			if err != nil {
				return err
			}
			ids <- id
			if err := writeResult(ctx, conn, id, `{}`); err != nil {
				return err
			}
		}
		return nil
	})
	session := dialInvokePipe(t, p)
	for range count {
		if _, err := session.Invoke(context.Background(), "unique", nil); err != nil {
			t.Fatal(err)
		}
	}
	close(ids)
	seen := make(map[string]bool, count)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate invoke ID %q", id)
		}
		seen[id] = true
	}
}

func TestUnmatchedFrameBecomesEvent(t *testing.T) {
	frame := []byte(`{ "payload" : {"data":"untouched"}, "sequence": 7 }`)
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		return conn.Write(ctx, websocket.MessageText, frame)
	})
	session := dialInvokePipe(t, p)
	select {
	case event := <-session.Events():
		if string(event.Frame) != string(frame) {
			t.Fatalf("event frame changed:\n got %q\nwant %q", event.Frame, frame)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not delivered")
	}
}

func TestEventChannelLabel(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"channel":"terminal.output","payload":1}`),
		[]byte(`{"payload":2}`),
		[]byte(`{"channel":123,"payload":3}`),
	}
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		for _, frame := range frames {
			if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
				return err
			}
		}
		return nil
	})
	session := dialInvokePipe(t, p)
	want := []string{"terminal.output", "", ""}
	for i := range frames {
		select {
		case event := <-session.Events():
			if event.Channel != want[i] || string(event.Frame) != string(frames[i]) {
				t.Fatalf("event %d = %#v; want channel %q and frame %s", i, event, want[i], frames[i])
			}
		case <-time.After(time.Second):
			t.Fatalf("event %d was not delivered", i)
		}
	}
	if err := session.Err(); err != nil {
		t.Fatalf("channel-less event ended session: %v", err)
	}
}

func receiveFixtureEvent(t *testing.T, frame string) Event {
	t.Helper()
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		return conn.Write(ctx, websocket.MessageText, []byte(frame))
	})
	session := dialInvokePipe(t, p)
	select {
	case event := <-session.Events():
		return event
	case <-time.After(time.Second):
		t.Fatalf("event was not delivered; session error: %v", session.Err())
		return Event{}
	}
}

func TestPtyOutputEventExposesParams(t *testing.T) {
	event := receiveFixtureEvent(t, ptyOutputFixture)
	if event.Channel != "pty:output" {
		t.Fatalf("channel = %q; want pty:output", event.Channel)
	}
	var params struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(event.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.ID != "tt" || params.Data != "echo hi\r\n" {
		t.Fatalf("params = %#v", params)
	}
	if !bytes.Equal(event.Frame, []byte(ptyOutputFixture)) {
		t.Fatalf("frame changed:\n got %q\nwant %q", event.Frame, ptyOutputFixture)
	}
}

func TestPtyExitEventExposesParams(t *testing.T) {
	event := receiveFixtureEvent(t, ptyExitFixture)
	if event.Channel != "pty:exit" {
		t.Fatalf("channel = %q; want pty:exit", event.Channel)
	}
	var params struct {
		ID       string `json:"id"`
		ExitCode int    `json:"exitCode"`
	}
	if err := json.Unmarshal(event.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.ID != "yz" || params.ExitCode != 7 {
		t.Fatalf("params = %#v", params)
	}
	if !bytes.Equal(event.Frame, []byte(ptyExitFixture)) {
		t.Fatalf("frame changed:\n got %q\nwant %q", event.Frame, ptyExitFixture)
	}
}

func TestEventWithArrayParams(t *testing.T) {
	event := receiveFixtureEvent(t, notificationFixture)
	if event.Channel != "notification:update" {
		t.Fatalf("channel = %q; want notification:update", event.Channel)
	}
	var fixtureFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(notificationFixture), &fixtureFields); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(event.Params, fixtureFields["params"]) {
		t.Fatalf("params changed:\n got %s\nwant %s", event.Params, fixtureFields["params"])
	}
	var params []map[string]any
	if err := json.Unmarshal(event.Params, &params); err != nil {
		t.Fatal(err)
	}
	if len(params) != 1 || params[0]["reason"] != "connected" {
		t.Fatalf("params = %#v", params)
	}
	if !bytes.Equal(event.Frame, []byte(notificationFixture)) {
		t.Fatalf("frame changed:\n got %q\nwant %q", event.Frame, notificationFixture)
	}
}

func TestEventWithoutParams(t *testing.T) {
	event := receiveFixtureEvent(t, `{"channel":"ready","type":"event"}`)
	if event.Params != nil {
		t.Fatalf("params = %s; want nil", event.Params)
	}
}

func TestInvokeContextCancelReleasesSlot(t *testing.T) {
	firstID := make(chan string, 1)
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		first, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(first)
		if err != nil {
			return err
		}
		firstID <- id
		second, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		secondID, err := invokeID(second)
		if err != nil {
			return err
		}
		if err := writeResult(ctx, conn, id, `{"late":true}`); err != nil {
			return err
		}
		return writeResult(ctx, conn, secondID, `{"alive":true}`)
	})
	session := dialInvokePipe(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	invokeDone := make(chan error, 1)
	go func() {
		_, err := session.Invoke(ctx, "slow", nil)
		invokeDone <- err
	}()
	id := <-firstID
	pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
	if err := session.Ping(pingCtx); err != nil {
		pingCancel()
		t.Fatalf("ping after first request: %v", err)
	}
	pingCancel()
	cancel()
	if err := <-invokeDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled invoke error = %v", err)
	}
	session.runtime.stateMu.Lock()
	pendingCount := len(session.runtime.pending)
	session.runtime.stateMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("canceled invoke retained %d pending slots", pendingCount)
	}

	result, err := session.Invoke(context.Background(), "after-cancel", nil)
	if err != nil || string(result) != `{"alive":true}` {
		t.Fatalf("invoke after cancellation = %s, %v", result, err)
	}
	select {
	case event := <-session.Events():
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(event.Frame, &frame); err != nil {
			t.Fatal(err)
		}
		var eventID string
		if err := json.Unmarshal(frame["id"], &eventID); err != nil || eventID != id {
			t.Fatalf("late reply event ID = %q, %v; want %q", eventID, err, id)
		}
	case <-time.After(time.Second):
		t.Fatal("late reply was not routed as an event")
	}
	if err := session.Err(); err != nil {
		t.Fatalf("late reply ended session: %v", err)
	}
}

func TestInvokeCancellationDoesNotKillSession(t *testing.T) {
	serverReady := make(chan struct{})
	allowRead := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowRead) }) }
	t.Cleanup(release)
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		close(serverReady)
		<-allowRead
		if _, err := readInvoke(ctx, conn); err != nil {
			return err
		}
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		return writeResult(ctx, conn, id, `{"alive":true}`)
	})
	writes := observeClientWrites(p)
	session := dialInvokePipe(t, p)
	<-serverReady

	ctx, cancel := context.WithCancel(context.Background())
	invokeDone := make(chan error, 1)
	writeStarted := writes.arm()
	go func() {
		_, err := session.Invoke(ctx, "no-reply", nil)
		invokeDone <- err
	}()
	waitForPendingInvokes(t, session, 1)
	waitForWriteStart(t, writeStarted)
	cancel()
	release()
	if err := <-invokeDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled invoke error = %v; want %v", err, context.Canceled)
	}

	nextCtx, nextCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer nextCancel()
	result, err := session.Invoke(nextCtx, "after-cancel", nil)
	if err != nil || string(result) != `{"alive":true}` {
		t.Fatalf("invoke after cancellation = %s, %v", result, err)
	}
	assertSessionAlive(t, session)
}

func TestConcurrentInvokeCancellationKeepsSessionAlive(t *testing.T) {
	const count = 16
	serverReady := make(chan struct{})
	allowRead := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowRead) }) }
	t.Cleanup(release)
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		close(serverReady)
		<-allowRead
		if _, err := readInvoke(ctx, conn); err != nil {
			return err
		}
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		return writeResult(ctx, conn, id, `{"alive":true}`)
	})
	writes := observeClientWrites(p)
	session := dialInvokePipe(t, p)
	<-serverReady

	cancels := make([]context.CancelFunc, count)
	errorsByCall := make(chan error, count)
	var wg sync.WaitGroup
	writeStarted := writes.arm()
	for i := range count {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := session.Invoke(ctx, "cancel-concurrent", i)
			errorsByCall <- err
		}()
	}
	waitForPendingInvokes(t, session, count)
	waitForWriteStart(t, writeStarted)
	for _, cancel := range cancels {
		cancel()
	}
	release()
	wg.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("concurrent canceled invoke error = %v; want %v", err, context.Canceled)
		}
	}
	waitForPendingInvokes(t, session, 0)

	nextCtx, nextCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer nextCancel()
	result, err := session.Invoke(nextCtx, "after-concurrent-cancel", nil)
	if err != nil || string(result) != `{"alive":true}` {
		t.Fatalf("invoke after concurrent cancellation = %s, %v", result, err)
	}
	assertSessionAlive(t, session)
}

func TestCloseFailsPendingInvokes(t *testing.T) {
	requestRead := make(chan struct{})
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if _, err := readInvoke(ctx, conn); err != nil {
			return err
		}
		close(requestRead)
		_, _, err := conn.Read(ctx)
		return err
	})
	session := dialInvokePipe(t, p)
	invokeDone := make(chan error, 1)
	go func() {
		_, err := session.Invoke(context.Background(), "pending", nil)
		invokeDone <- err
	}()
	<-requestRead
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-invokeDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("pending invoke error = %v; want %v", err, ErrClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("pending invoke did not fail on Close")
	}
}

func TestCloseInterruptsBlockedInvokeWrite(t *testing.T) {
	serverReady := make(chan struct{})
	allowRead := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowRead) }) }
	t.Cleanup(release)
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		close(serverReady)
		<-allowRead
		_, err := readInvoke(ctx, conn)
		return err
	})
	writes := observeClientWrites(p)
	session := dialInvokePipe(t, p)
	<-serverReady

	invokeDone := make(chan error, 1)
	writeStarted := writes.arm()
	go func() {
		_, err := session.Invoke(context.Background(), "blocked-write", nil)
		invokeDone <- err
	}()
	waitForWriteStart(t, writeStarted)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-invokeDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked invoke error = %v; want %v", err, ErrClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt blocked invoke write")
	}
	release()
}

func TestConnectionErrorFailsPendingInvokes(t *testing.T) {
	requestRead := make(chan struct{})
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if _, err := readInvoke(ctx, conn); err != nil {
			return err
		}
		close(requestRead)
		return conn.CloseNow()
	})
	session := dialInvokePipe(t, p)
	invokeDone := make(chan error, 1)
	go func() {
		_, err := session.Invoke(context.Background(), "pending", nil)
		invokeDone <- err
	}()
	<-requestRead
	select {
	case err := <-invokeDone:
		if !errors.Is(err, ErrConnection) {
			t.Fatalf("pending invoke error = %v; want %v", err, ErrConnection)
		}
	case <-time.After(time.Second):
		t.Fatal("pending invoke did not fail on disconnect")
	}
	if err := session.Err(); !errors.Is(err, ErrConnection) {
		t.Fatalf("session error = %v; want %v", err, ErrConnection)
	}
}

func TestEventOverflowTerminatesSession(t *testing.T) {
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		for i := 0; i <= eventBufferSize; i++ {
			frame := fmt.Sprintf(`{"channel":"terminal.output","sequence":%d}`, i)
			if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				return err
			}
		}
		return nil
	})
	session := dialInvokePipe(t, p)
	waitSessionError(t, session, ErrEventOverflow)
	count := 0
	for range session.Events() {
		count++
	}
	if count != eventBufferSize {
		t.Fatalf("buffer retained %d events; want %d before explicit overflow failure", count, eventBufferSize)
	}
}

func TestRejectsNonObjectFrame(t *testing.T) {
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		return conn.Write(ctx, websocket.MessageText, []byte(`[1,2,3]`))
	})
	session := dialInvokePipe(t, p)
	waitSessionError(t, session, ErrInvalidFrame)
	if _, ok := <-session.Events(); ok {
		t.Fatal("events channel remained open after invalid frame")
	}
}

func TestRejectsMalformedOrNonTextFrame(t *testing.T) {
	for _, test := range []struct {
		name  string
		kind  websocket.MessageType
		frame []byte
	}{
		{name: "malformed JSON", kind: websocket.MessageText, frame: []byte(`{"broken"`)},
		{name: "binary", kind: websocket.MessageBinary, frame: []byte(`{"valid":"json"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
				return conn.Write(ctx, test.kind, test.frame)
			})
			session := dialInvokePipe(t, p)
			waitSessionError(t, session, ErrInvalidFrame)
		})
	}
}

func TestReadLimitIsExplicit(t *testing.T) {
	frame := []byte(`{"channel":"terminal.output","payload":"` + strings.Repeat("x", 64<<10) + `"}`)
	if len(frame) >= sessionReadLimit {
		t.Fatal("test frame exceeds configured read limit")
	}
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		return conn.Write(ctx, websocket.MessageText, frame)
	})
	session := dialInvokePipe(t, p)
	select {
	case event := <-session.Events():
		if string(event.Frame) != string(frame) {
			t.Fatal("large frame changed")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("large frame was not delivered; session error: %v", session.Err())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		_, _, err := conn.Read(ctx)
		return err
	})
	session := dialInvokePipe(t, p)
	done := make(chan error, 1)
	go func() {
		if err := session.Close(); err != nil {
			done <- err
			return
		}
		done <- session.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("repeated Close hung")
	}
}

func TestConcurrentInvokes(t *testing.T) {
	const count = 64
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		requests := make([]observedInvoke, count)
		for i := range requests {
			request, err := readInvoke(ctx, conn)
			if err != nil {
				return err
			}
			requests[i] = request
		}
		for i := len(requests) - 1; i >= 0; i-- {
			id, err := invokeID(requests[i])
			if err != nil {
				return err
			}
			if err := writeResult(ctx, conn, id, string(requests[i].fields["params"])); err != nil {
				return err
			}
		}
		return nil
	})
	session := dialInvokePipe(t, p)
	start := make(chan struct{})
	errorsByCall := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := session.Invoke(context.Background(), "concurrent", i)
			if err != nil {
				errorsByCall <- err
				return
			}
			var got int
			if err := json.Unmarshal(result, &got); err != nil || got != i {
				errorsByCall <- fmt.Errorf("call %d got %s: %v", i, result, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		t.Error(err)
	}
}

func TestPing(t *testing.T) {
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		_, _, err := conn.Read(ctx)
		return err
	})
	session := dialInvokePipe(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPingCancellationDoesNotKillSession(t *testing.T) {
	serverReady := make(chan struct{})
	allowRead := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowRead) }) }
	t.Cleanup(release)
	p := newInvokePipeServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		close(serverReady)
		<-allowRead
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		return writeResult(ctx, conn, id, `{"alive":true}`)
	})
	writes := observeClientWrites(p)
	session := dialInvokePipe(t, p)
	<-serverReady

	ctx, cancel := context.WithCancel(context.Background())
	pingDone := make(chan error, 1)
	writeStarted := writes.arm()
	go func() { pingDone <- session.Ping(ctx) }()
	waitForWriteStart(t, writeStarted)
	cancel()
	release()
	if err := <-pingDone; err != nil {
		t.Fatalf("ping after caller cancellation = %v; want nil", err)
	}

	nextCtx, nextCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer nextCancel()
	result, err := session.Invoke(nextCtx, "after-ping-cancel", nil)
	if err != nil || string(result) != `{"alive":true}` {
		t.Fatalf("invoke after ping cancellation = %s, %v", result, err)
	}
	assertSessionAlive(t, session)
}
