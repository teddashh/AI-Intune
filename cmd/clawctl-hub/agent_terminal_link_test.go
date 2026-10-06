package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/sessionid"
	"github.com/teddashh/AI-Intune/internal/store"
)

const terminalLinkTestTimeout = 3 * time.Second

type terminalLinkTestFixture struct {
	t          *testing.T
	hub        *hub
	server     *httptest.Server
	machineID  string
	agentToken string
}

func newTerminalLinkTestFixture(t *testing.T) *terminalLinkTestFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, enrollmentToken, err := st.CreateEnrollTokenFor("terminal-test-machine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineID, agentToken, err := st.RedeemEnrollToken(enrollmentToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion,
		Hostname:      "terminal-test-machine",
		UnixUser:      "tester",
		OS:            "linux",
		Arch:          "amd64",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	h := &hub{store: st, agentLinks: agentlink.New()}
	mux := http.NewServeMux()
	h.machineAndPublicRoutes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &terminalLinkTestFixture{t: t, hub: h, server: server, machineID: machineID, agentToken: agentToken}
}

func (f *terminalLinkTestFixture) dial(headers http.Header, subprotocols ...string) (*websocket.Conn, *http.Response, error) {
	f.t.Helper()
	return f.dialAs(f.agentToken, headers, subprotocols...)
}

func (f *terminalLinkTestFixture) dialAs(agentToken string, headers http.Header, subprotocols ...string) (*websocket.Conn, *http.Response, error) {
	f.t.Helper()
	if headers == nil {
		headers = make(http.Header)
	}
	headers = headers.Clone()
	headers.Set("Authorization", "Bearer "+agentToken)
	ctx, cancel := context.WithTimeout(f.t.Context(), terminalLinkTestTimeout)
	defer cancel()
	return websocket.Dial(ctx, f.server.URL+"/v1/agent/terminal-link", &websocket.DialOptions{
		HTTPHeader:   headers,
		Subprotocols: subprotocols,
	})
}

func (f *terminalLinkTestFixture) dialLink() *websocket.Conn {
	f.t.Helper()
	conn, response, err := f.dial(nil, agentTerminalSubprotocol)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		f.t.Fatalf("dial terminal link: status=%d error=%v", status, err)
	}
	f.t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

type channelTerminalSink struct {
	frames chan agentrelay.Upstream
}

func newChannelTerminalSink() *channelTerminalSink {
	return &channelTerminalSink{frames: make(chan agentrelay.Upstream, 8)}
}

func (s *channelTerminalSink) Deliver(frame agentrelay.Upstream) error {
	select {
	case s.frames <- frame:
		return nil
	default:
		return errors.New("test sink full")
	}
}

func awaitTerminalFrame(t *testing.T, sink *channelTerminalSink) agentrelay.Upstream {
	t.Helper()
	select {
	case frame := <-sink.frames:
		return frame
	case <-time.After(terminalLinkTestTimeout):
		t.Fatal("timed out waiting for terminal sink frame")
		return agentrelay.Upstream{}
	}
}

func openTerminalSession(t *testing.T, registry *agentlink.Registry, sessionID, machineID string, sink agentlink.Sink) {
	t.Helper()
	deadline := time.Now().Add(terminalLinkTestTimeout)
	for {
		err := registry.OpenSession(sessionID, machineID, sink)
		if err == nil {
			return
		}
		if !errors.Is(err, agentlink.ErrNoLink) || time.Now().After(deadline) {
			t.Fatalf("OpenSession(%q): %v", sessionID, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func writeUpstreamFrame(t *testing.T, conn *websocket.Conn, frame agentrelay.Upstream) {
	t.Helper()
	payload, err := agentrelay.EncodeUpstream(frame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write upstream frame: %v", err)
	}
}

func TestAgentTerminalReadLimitExceedsLargestCanonicalFrame(t *testing.T) {
	session := strings.Repeat("<", sessionid.MaxLength)
	data := make([]byte, agentrelay.MaxDataSize)

	upstream, err := agentrelay.EncodeUpstream(agentrelay.Upstream{
		Type:    agentrelay.UpstreamOutput,
		Session: session,
		Data:    data,
	})
	if err != nil {
		t.Fatalf("EncodeUpstream() error = %v", err)
	}
	if len(upstream) >= agentTerminalReadLimit {
		t.Fatalf("largest canonical upstream frame is %d bytes, agentTerminalReadLimit is %d bytes; increase agentTerminalReadLimit", len(upstream), agentTerminalReadLimit)
	}

	downstream, err := agentrelay.EncodeDownstream(agentrelay.Downstream{
		Type:    agentrelay.DownstreamInput,
		Session: session,
		Data:    data,
	})
	if err != nil {
		t.Fatalf("EncodeDownstream() error = %v", err)
	}
	if len(downstream) >= agentTerminalReadLimit {
		t.Fatalf("largest canonical downstream frame is %d bytes, agentTerminalReadLimit is %d bytes; increase agentTerminalReadLimit", len(downstream), agentTerminalReadLimit)
	}
}

func awaitWebSocketClose(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("websocket remained open")
	}
	return websocket.CloseStatus(err)
}

func TestAgentTerminalLinkUnauthenticatedUpgradeRefusedBeforeAnyHandshake(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, f.server.URL+"/v1/agent/terminal-link", &websocket.DialOptions{
		Subprotocols: []string{agentTerminalSubprotocol},
	})
	if conn != nil {
		_ = conn.CloseNow()
		t.Fatal("unauthenticated request completed a websocket handshake")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dial: response=%v error=%v", response, err)
	}
}

func TestAgentTerminalLinkRequestCarryingOriginHeaderRefused(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	conn, response, err := f.dial(http.Header{"Origin": {f.server.URL}}, agentTerminalSubprotocol)
	if conn != nil {
		_ = conn.CloseNow()
		t.Fatal("request carrying Origin completed a websocket handshake")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin dial: response=%v error=%v", response, err)
	}
}

func TestAgentTerminalLinkNilRegistryRefusesWithPlainHTTPError(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/agent/terminal-link", nil)
	(&hub{}).handleAgentTerminalLink(recorder, request, "machine")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type=%q, want text/plain", contentType)
	}
}

func TestAgentTerminalLinkDialWithoutSubprotocolRefused(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	conn, _, err := f.dial(nil)
	if err != nil {
		t.Fatalf("dial reached no-subprotocol policy check: %v", err)
	}
	defer conn.CloseNow()
	if status := awaitWebSocketClose(t, conn); status != websocket.StatusPolicyViolation {
		t.Fatalf("close status=%v, want StatusPolicyViolation", status)
	}
}

func TestAgentTerminalLinkStaysAlivePastServerWriteTimeout(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	f.server.Close()
	mux := http.NewServeMux()
	f.hub.machineAndPublicRoutes(mux)
	server := httptest.NewUnstartedServer(mux)
	server.Config.WriteTimeout = 40 * time.Millisecond
	server.Config.ReadTimeout = 40 * time.Millisecond
	server.Start()
	defer server.Close()
	f.server = server

	conn := f.dialLink()
	time.Sleep(120 * time.Millisecond)
	sink := newChannelTerminalSink()
	openTerminalSession(t, f.hub.agentLinks, "past-write-timeout", f.machineID, sink)
	want := agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Session: "past-write-timeout", Data: []byte("still alive")}
	writeUpstreamFrame(t, conn, want)
	got := awaitTerminalFrame(t, sink)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered frame=%#v, want %#v", got, want)
	}

	// The read path alone would still pass if only the write half were broken,
	// so drive a downstream frame past the same expired WriteTimeout.
	downstream := agentrelay.Downstream{
		Type: agentrelay.DownstreamInput, Session: "past-write-timeout", Data: []byte("still writable"),
	}
	if err := f.hub.agentLinks.Send(downstream.Session, downstream); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
	defer cancel()
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read downstream frame past write timeout: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type=%v, want text", messageType)
	}
	decoded, err := agentrelay.DecodeDownstream(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, downstream) {
		t.Fatalf("downstream frame=%#v, want %#v", decoded, downstream)
	}
}

func TestAgentTerminalLinkDownstreamFrameThroughRegistryArrivesByteIdentical(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	conn := f.dialLink()
	openTerminalSession(t, f.hub.agentLinks, "downstream", f.machineID, newChannelTerminalSink())
	want := agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: "downstream", Data: []byte{0, 1, 2, 255, '<', '&'}}
	if err := f.hub.agentLinks.Send(want.Session, want); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
	defer cancel()
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type=%v, want text", messageType)
	}
	got, err := agentrelay.DecodeDownstream(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("downstream frame=%#v, want %#v", got, want)
	}
}

func TestAgentTerminalLinkUpstreamFrameFromAgentReachesRegisteredSink(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	conn := f.dialLink()
	sink := newChannelTerminalSink()
	openTerminalSession(t, f.hub.agentLinks, "upstream", f.machineID, sink)
	want := agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Session: "upstream", Data: []byte{0, 255, '<', '>'}}
	writeUpstreamFrame(t, conn, want)
	if got := awaitTerminalFrame(t, sink); !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream frame=%#v, want %#v", got, want)
	}
}

func TestAgentTerminalLinkDecodeUpstreamFailureClosesLinkAndDetachesMachine(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	conn := f.dialLink()
	sink := newChannelTerminalSink()
	openTerminalSession(t, f.hub.agentLinks, "decode-failure", f.machineID, sink)
	ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"output","session":"decode-failure","unknown":true}`)); err != nil {
		t.Fatal(err)
	}
	if status := awaitWebSocketClose(t, conn); status != websocket.StatusPolicyViolation {
		t.Fatalf("close status=%v, want StatusPolicyViolation", status)
	}
	closed := awaitTerminalFrame(t, sink)
	if closed.Type != agentrelay.UpstreamError || closed.Session != "decode-failure" {
		t.Fatalf("session closure=%#v", closed)
	}
	if err := f.hub.agentLinks.Send("decode-failure", agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "decode-failure"}); !errors.Is(err, agentlink.ErrUnknownSession) {
		t.Fatalf("Send after decode failure=%v, want ErrUnknownSession", err)
	}
}

func TestAgentTerminalLinkUpstreamFrameForAnotherMachinesSessionClosesLink(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	firstConn := f.dialLink()
	_, enrollmentToken, err := f.hub.store.CreateEnrollTokenFor("second-terminal-machine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	secondMachineID, secondAgentToken, err := f.hub.store.RedeemEnrollToken(enrollmentToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion,
		Hostname:      "second-terminal-machine",
		UnixUser:      "tester",
		OS:            "linux",
		Arch:          "amd64",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	secondConn, _, err := f.dialAs(secondAgentToken, nil, agentTerminalSubprotocol)
	if err != nil {
		t.Fatal(err)
	}
	defer secondConn.CloseNow()
	foreignSink := newChannelTerminalSink()
	openTerminalSession(t, f.hub.agentLinks, "foreign-session", secondMachineID, foreignSink)
	writeUpstreamFrame(t, firstConn, agentrelay.Upstream{Type: agentrelay.UpstreamReady, Session: "foreign-session"})
	if status := awaitWebSocketClose(t, firstConn); status != websocket.StatusPolicyViolation {
		t.Fatalf("close status=%v, want StatusPolicyViolation", status)
	}
}

func TestAgentTerminalLinkUpstreamFrameForUnknownSessionDoesNotCloseLink(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	conn := f.dialLink()
	writeUpstreamFrame(t, conn, agentrelay.Upstream{Type: agentrelay.UpstreamReady, Session: "already-closed"})
	sink := newChannelTerminalSink()
	openTerminalSession(t, f.hub.agentLinks, "known-after-unknown", f.machineID, sink)
	want := agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Session: "known-after-unknown", Data: []byte("accepted")}
	writeUpstreamFrame(t, conn, want)
	if got := awaitTerminalFrame(t, sink); !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream frame=%#v, want %#v", got, want)
	}
}

func TestAgentTerminalLinkSecondDialForSameMachineReplacesFirstAndClosesFirstSessions(t *testing.T) {
	f := newTerminalLinkTestFixture(t)

	// A replaced link must report a normal closure to its agent, and that
	// depends on the close frame winning a race against the connection
	// teardown. One replacement reproduces a regression only about one run in
	// ten, so repeat it: any goroutine ordering that drops the close frame
	// shows up here instead of as an occasional red build.
	const replacements = 24
	previous := f.dialLink()
	for i := range replacements {
		sessionID := fmt.Sprintf("replaced-session-%d", i)
		sink := newChannelTerminalSink()
		openTerminalSession(t, f.hub.agentLinks, sessionID, f.machineID, sink)

		next := f.dialLink()
		if status := awaitWebSocketClose(t, previous); status != websocket.StatusNormalClosure {
			t.Fatalf("replacement %d: close status=%v, want StatusNormalClosure", i, status)
		}
		closed := awaitTerminalFrame(t, sink)
		if closed.Type != agentrelay.UpstreamError || closed.Session != sessionID {
			t.Fatalf("replacement %d: session closure=%#v", i, closed)
		}
		previous = next
	}
	openTerminalSession(t, f.hub.agentLinks, "surviving-session", f.machineID, newChannelTerminalSink())
}

type inertAgentLink struct{}

func (inertAgentLink) Send(agentrelay.Downstream) error { return nil }
func (inertAgentLink) Close(string)                     {}

func captureUnstartedWSLink(t *testing.T) (*wsLink, *websocket.Conn, func()) {
	t.Helper()
	accepted := make(chan *wsLink, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			return
		}
		link := newWSLink(conn)
		accepted <- link
		<-release
		link.Close("test complete")
	}))
	ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
	conn, _, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{Subprotocols: []string{agentTerminalSubprotocol}})
	cancel()
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	link := <-accepted
	cleanup := func() {
		releaseOnce.Do(func() { close(release) })
		_ = conn.CloseNow()
		server.Close()
	}
	return link, conn, cleanup
}

func TestAgentTerminalLinkSendOnStaleReplacedLinkReturnsErrorInsteadOfSilentlyVanishing(t *testing.T) {
	stale, conn, cleanup := captureUnstartedWSLink(t)
	defer cleanup()
	registry := agentlink.New()
	if _, err := registry.Attach("machine", stale); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
		defer cancel()
		_, _, _ = conn.Read(ctx)
	}()
	if _, err := registry.Attach("machine", inertAgentLink{}); err != nil {
		t.Fatal(err)
	}
	<-readDone
	if err := stale.Send(agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "stale"}); !errors.Is(err, errAgentTerminalLinkClosed) {
		t.Fatalf("stale Send error=%v, want closed sentinel", err)
	}
}

func TestAgentTerminalLinkCloseReasonTruncatesOnRuneBoundary(t *testing.T) {
	reason := strings.Repeat("a", agentTerminalCloseReasonMax-1) + "界"
	got := truncateWebSocketCloseReason(reason)
	if !utf8.ValidString(got) || len(got) > agentTerminalCloseReasonMax {
		t.Fatalf("truncated close reason is invalid or too long: %q (%d bytes)", got, len(got))
	}
	if got != strings.Repeat("a", agentTerminalCloseReasonMax-1) {
		t.Fatalf("truncated close reason=%q", got)
	}
}

func startTerminalLinkWriter(t *testing.T, link *wsLink) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	go link.writeLoop(ctx)
	return cancel
}

func readDownstreamFrame(t *testing.T, conn *websocket.Conn) agentrelay.Downstream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), terminalLinkTestTimeout)
	defer cancel()
	conn.SetReadLimit(agentTerminalReadLimit)
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read downstream frame: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type=%v, want text", messageType)
	}
	frame, err := agentrelay.DecodeDownstream(payload)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestAgentTerminalLinkQueueOverFrameCapClosesLinkInsteadOfBlocking(t *testing.T) {
	link, conn, cleanup := captureUnstartedWSLink(t)
	defer cleanup()
	for i := 0; i < agentTerminalQueueFrames; i++ {
		if err := link.Send(agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "frame-cap"}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if err := link.Send(agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "frame-cap"}); !errors.Is(err, errAgentTerminalQueueFull) {
		t.Fatalf("over-cap Send error=%v, want queue sentinel", err)
	}
	if status := awaitWebSocketClose(t, conn); status != websocket.StatusNormalClosure {
		t.Fatalf("close status=%v, want StatusNormalClosure", status)
	}
}

func TestAgentTerminalLinkInvalidFrameLeavesLinkOpen(t *testing.T) {
	link, conn, cleanup := captureUnstartedWSLink(t)
	defer cleanup()
	err := link.Send(agentrelay.Downstream{Type: agentrelay.DownstreamResize, Session: "invalid", Cols: 0, Rows: 24})
	if !errors.Is(err, errAgentTerminalFrameInvalid) || !errors.Is(err, agentrelay.ErrInvalidGeometry) {
		t.Fatalf("resize error=%v, want invalid-frame and geometry sentinels", err)
	}
	oversized := bytes.Repeat([]byte("SECRET-PAYLOAD"), agentrelay.MaxDataSize/len("SECRET-PAYLOAD")+1)
	err = link.Send(agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: "invalid", Data: oversized})
	if !errors.Is(err, errAgentTerminalFrameInvalid) || !errors.Is(err, agentrelay.ErrDataTooLarge) {
		t.Fatalf("oversized input error=%v, want invalid-frame and data-size sentinels", err)
	}
	if strings.Contains(err.Error(), "SECRET-PAYLOAD") {
		t.Fatalf("frame data leaked into error: %v", err)
	}
	want := agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: "invalid", Data: []byte{0, 1, 2, 255, '<', '&'}}
	if err := link.Send(want); err != nil {
		t.Fatalf("valid frame after invalid frame: %v", err)
	}
	cancel := startTerminalLinkWriter(t, link)
	defer cancel()
	if got := readDownstreamFrame(t, conn); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered frame=%#v, want %#v", got, want)
	}
}

func TestAgentTerminalLinkInputOverByteBudgetReturnsBusy(t *testing.T) {
	link, conn, cleanup := captureUnstartedWSLink(t)
	defer cleanup()
	full := bytes.Repeat([]byte{'x'}, agentrelay.MaxDataSize)
	first := agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: "byte-budget", Data: full}
	if err := link.Send(first); err != nil {
		t.Fatalf("max frame on an empty queue: %v", err)
	}
	extra := agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: "byte-budget", Data: []byte{'y'}}
	if err := link.Send(extra); !errors.Is(err, agentlink.ErrLinkBusy) {
		t.Fatalf("over-budget input error=%v, want ErrLinkBusy", err)
	}
	cancel := startTerminalLinkWriter(t, link)
	defer cancel()
	if got := readDownstreamFrame(t, conn); !reflect.DeepEqual(got, first) {
		t.Fatal("drained frame differed")
	}
	if err := link.Send(extra); err != nil {
		t.Fatalf("retry after the agent read the queued frame: %v", err)
	}
	if got := readDownstreamFrame(t, conn); !reflect.DeepEqual(got, extra) {
		t.Fatalf("retried frame=%#v, want %#v", got, extra)
	}
}

func TestAgentTerminalLinkInputFrameCapLeavesRoomForClose(t *testing.T) {
	if agentTerminalInputQueueFrames != 192 || agentTerminalQueueFrames-agentTerminalInputQueueFrames != 64 {
		t.Fatalf("input cap=%d frame cap=%d, want 192 input frames and 64 control slots",
			agentTerminalInputQueueFrames, agentTerminalQueueFrames)
	}
	link, conn, cleanup := captureUnstartedWSLink(t)
	defer cleanup()
	input := agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: "input-cap", Data: []byte{'z'}}
	for i := 0; i < agentTerminalInputQueueFrames; i++ {
		if err := link.Send(input); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if err := link.Send(input); !errors.Is(err, agentlink.ErrLinkBusy) {
		t.Fatalf("input at the frame cap error=%v, want ErrLinkBusy", err)
	}
	closing := agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "input-cap"}
	if err := link.Send(closing); err != nil {
		t.Fatalf("close while input is at its cap: %v", err)
	}
	cancel := startTerminalLinkWriter(t, link)
	defer cancel()
	for i := 0; i < agentTerminalInputQueueFrames; i++ {
		got := readDownstreamFrame(t, conn)
		if got.Type != agentrelay.DownstreamInput || !bytes.Equal(got.Data, input.Data) || got.Session != input.Session {
			t.Fatalf("frame %d=%#v", i, got)
		}
	}
	if got := readDownstreamFrame(t, conn); !reflect.DeepEqual(got, closing) {
		t.Fatalf("close frame=%#v, want %#v", got, closing)
	}
}

func TestAgentTerminalLinkResizeFrameCapReturnsBusy(t *testing.T) {
	link, _, cleanup := captureUnstartedWSLink(t)
	defer cleanup()
	resize := agentrelay.Downstream{Type: agentrelay.DownstreamResize, Session: "resize-cap", Cols: 80, Rows: 24}
	for i := 0; i < agentTerminalInputQueueFrames; i++ {
		if err := link.Send(resize); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if err := link.Send(resize); !errors.Is(err, agentlink.ErrLinkBusy) {
		t.Fatalf("resize at the frame cap error=%v, want ErrLinkBusy", err)
	}
	opening := agentrelay.Downstream{Type: agentrelay.DownstreamOpen, Session: "resize-cap", Cols: 80, Rows: 24}
	if err := link.Send(opening); err != nil {
		t.Fatalf("open while resize is at its cap: %v", err)
	}
	closing := agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "resize-cap"}
	if err := link.Send(closing); err != nil {
		t.Fatalf("close while resize is at its cap: %v", err)
	}
}

func TestAgentTerminalLinkClosingSocketDetachesMachineAndClosesItsSessions(t *testing.T) {
	f := newTerminalLinkTestFixture(t)
	conn := f.dialLink()
	sink := newChannelTerminalSink()
	openTerminalSession(t, f.hub.agentLinks, "socket-close", f.machineID, sink)
	if err := conn.Close(websocket.StatusNormalClosure, "agent stopping"); err != nil {
		// The server may finish its matching close first.
		if !strings.Contains(err.Error(), "closed") {
			t.Fatal(err)
		}
	}
	closed := awaitTerminalFrame(t, sink)
	if closed.Type != agentrelay.UpstreamError || closed.Session != "socket-close" {
		t.Fatalf("socket-close session closure=%#v", closed)
	}
	if err := f.hub.agentLinks.Send("socket-close", agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "socket-close"}); !errors.Is(err, agentlink.ErrUnknownSession) {
		t.Fatalf("Send after socket close=%v, want ErrUnknownSession", err)
	}
}

func useAgentTerminalCredentialTicks(t *testing.T) (chan time.Time, chan bool) {
	t.Helper()
	ticks := make(chan time.Time)
	checked := make(chan bool, 4)
	previousTicks, previousChecked := agentTerminalCredentialTicks, agentTerminalCredentialChecked
	agentTerminalCredentialTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	agentTerminalCredentialChecked = func(current bool) { checked <- current }
	t.Cleanup(func() {
		agentTerminalCredentialTicks, agentTerminalCredentialChecked = previousTicks, previousChecked
	})
	return ticks, checked
}

func awaitAgentTerminalCredentialCheck(t *testing.T, checked <-chan bool) bool {
	t.Helper()
	select {
	case current := <-checked:
		return current
	case <-time.After(terminalLinkTestTimeout):
		t.Fatal("credential loop did not finish its check")
		return false
	}
}

func TestAgentTerminalLinkClosesWhenAgentTokenIsReplaced(t *testing.T) {
	ticks, checked := useAgentTerminalCredentialTicks(t)
	f := newTerminalLinkTestFixture(t)
	conn := f.dialLink()
	go func() {
		// Keep reading so the close frame is processed.
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	sink := newChannelTerminalSink()
	openTerminalSession(t, f.hub.agentLinks, "rotated-token", f.machineID, sink)

	// A tick while the token is still current keeps the link.
	select {
	case ticks <- time.Now():
	case <-time.After(terminalLinkTestTimeout):
		t.Fatal("credential loop did not take the first tick")
	}
	// Wait for that check to finish before rotating the token, so it cannot
	// observe the replacement.
	if !awaitAgentTerminalCredentialCheck(t, checked) {
		t.Fatal("first check reported the agent token as not current")
	}
	if !f.hub.agentLinks.HasLink(f.machineID) {
		t.Fatal("link closed while its agent token was still current")
	}

	if _, err := f.hub.store.DB().Exec(`UPDATE machine_registry SET agent_token_hash='replaced' WHERE machine_id=?`, f.machineID); err != nil {
		t.Fatal(err)
	}
	select {
	case ticks <- time.Now():
	case <-time.After(terminalLinkTestTimeout):
		t.Fatal("credential loop did not take the second tick")
	}
	if awaitAgentTerminalCredentialCheck(t, checked) {
		t.Fatal("second check still reported the replaced agent token as current")
	}
	closed := awaitTerminalFrame(t, sink)
	if closed.Type != agentrelay.UpstreamError || closed.Session != "rotated-token" {
		t.Fatalf("session closure after token replacement=%#v", closed)
	}
	deadline := time.Now().Add(terminalLinkTestTimeout)
	for f.hub.agentLinks.HasLink(f.machineID) {
		if time.Now().After(deadline) {
			t.Fatal("link still attached after its agent token was replaced")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
