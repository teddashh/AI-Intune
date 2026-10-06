package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/teddashh/AI-Intune/internal/agentpty"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/batremote"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestTerminalLinkUnsupportedPlatformReturnsWithoutFilesystemAccess(t *testing.T) {
	deps := defaultTerminalLinkDeps()
	deps.goos = "windows"
	deps.userHomeDir = func() (string, error) {
		t.Fatal("unsupported platform touched filesystem path discovery")
		return "", nil
	}
	deps.attempt = func(context.Context, config, terminalLinkDeps) terminalAttemptResult {
		t.Fatal("unsupported platform started a terminal attempt")
		return terminalAttemptResult{}
	}
	runTerminalLinkWithDeps(context.Background(), config{}, deps)
}

func TestTerminalBackoffResetsOnlyAfterSettledLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lifetimes := []time.Duration{
		39 * time.Second,
		39 * time.Second,
		40 * time.Second,
		0,
	}
	var attempts int
	clock := time.Unix(0, 0)
	var sleeps []time.Duration
	deps := defaultTerminalLinkDeps()
	deps.now = func() time.Time { return clock }
	deps.jitter = func(d time.Duration) time.Duration { return d }
	deps.attempt = func(_ context.Context, _ config, attemptDeps terminalLinkDeps) terminalAttemptResult {
		started := attemptDeps.now()
		clock = clock.Add(lifetimes[attempts])
		result := terminalAttemptResult{
			endpointAvailable: true,
			batAvailable:      true,
			linked:            attempts != len(lifetimes)-1,
			survived:          attemptDeps.now().Sub(started),
		}
		attempts++
		return result
	}
	deps.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) == len(lifetimes) {
			cancel()
			return context.Canceled
		}
		return nil
	}
	runTerminalLinkWithDeps(ctx, config{}, deps)

	want := []time.Duration{time.Second, 2 * time.Second, time.Second, time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleep count = %d; want %d", len(sleeps), len(want))
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Errorf("sleep[%d] = %s; want %s", i, sleeps[i], want[i])
		}
	}
}

func TestTerminalRetryDelayHasHardFortySecondCapAfterJitter(t *testing.T) {
	got := terminalRetryDelay(20, func(d time.Duration) time.Duration { return d + d/5 })
	if got != 40*time.Second {
		t.Fatalf("delay = %s; want 40s", got)
	}
}

func TestBATAvailabilityLogsOncePerTransition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := []terminalAttemptResult{
		{},
		{},
		{endpointAvailable: true},
		{endpointAvailable: true},
		{endpointAvailable: true, batAvailable: true},
		{endpointAvailable: true},
		{},
	}
	var attempt int
	deps := defaultTerminalLinkDeps()
	deps.jitter = func(d time.Duration) time.Duration { return d }
	deps.attempt = func(context.Context, config, terminalLinkDeps) terminalAttemptResult {
		result := results[attempt]
		attempt++
		return result
	}
	deps.sleep = func(context.Context, time.Duration) error {
		if attempt == len(results) {
			cancel()
			return context.Canceled
		}
		return nil
	}
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)
	runTerminalLinkWithDeps(ctx, config{}, deps)
	if got := strings.Count(logs.String(), "bat-server endpoint not available"); got != 2 {
		t.Fatalf("unavailable log count = %d; want 2 transitions; logs = %q", got, logs.String())
	}
	if got := strings.Count(logs.String(), "bat-server is not reachable"); got != 2 {
		t.Fatalf("unreachable log count = %d; want 2 transitions; logs = %q", got, logs.String())
	}
}

func TestTerminalWriterQueueOverflowTearsDownLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := newTerminalWriter(ctx, cancel, nil)
	for i := 0; i <= agentTerminalQueueFrames; i++ {
		writer.enqueue(agentrelay.Upstream{Type: agentrelay.UpstreamReady, Session: "queue-session"})
	}
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			t.Fatalf("context error = %v; want canceled", ctx.Err())
		}
	default:
		t.Fatal("writer overflow did not tear down link")
	}
}

type fakeTerminalBAT struct {
	server        *httptest.Server
	endpoint      batremote.Endpoint
	profileOpened atomic.Bool
	connections   chan *websocket.Conn
	created       chan string
	written       chan string
	closed        chan struct{}
	slowCreateID  string
	releaseCreate chan struct{}
	slowWriteID   string
	releaseWrite  chan struct{}
	failedCWD     string
	createdCWD    map[string]string
	slowKillID    string
	killStarted   chan string
	releaseKill   chan struct{}
	killResults   map[string]string
	killed        []string

	mu    sync.Mutex
	order []string
}

func startFakeTerminalBAT(t *testing.T) *fakeTerminalBAT {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(batremote.LoopbackHost, "0"))
	if err != nil {
		t.Fatalf("listen for fake bat-server: %v", err)
	}
	return startFakeTerminalBATWithListener(t, listener)
}

func startFakeTerminalBATWithListener(t *testing.T, listener net.Listener) *fakeTerminalBAT {
	t.Helper()
	fake := &fakeTerminalBAT{
		connections: make(chan *websocket.Conn, 1),
		created:     make(chan string, 8), written: make(chan string, 8),
		closed: make(chan struct{}, 8), killStarted: make(chan string, 8),
		killResults: make(map[string]string),
	}
	fake.server = httptest.NewUnstartedServer(http.HandlerFunc(fake.serveHTTP))
	_ = fake.server.Listener.Close()
	fake.server.Listener = listener
	fake.server.StartTLS()
	t.Cleanup(fake.server.Close)

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	envelope := fakeTerminalCertificateEnvelope(t, fake)
	fingerprint, err := batremote.CertificateFingerprintFromEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	fake.endpoint = batremote.Endpoint{
		Host: host, Port: port, Token: "fake-bat-token", Fingerprint: fingerprint,
	}
	return fake
}

func fakeTerminalCertificateEnvelope(t *testing.T, fake *fakeTerminalBAT) []byte {
	t.Helper()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fake.server.Certificate().Raw})
	envelope, err := json.Marshal(map[string]string{"certificate": string(certificate)})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func writeManagedTerminalEndpoint(t *testing.T, home string, fake *fakeTerminalBAT) {
	t.Helper()
	root := filepath.Join(home, ".local", "share", "clawctl", "bat-server")
	if err := os.MkdirAll(filepath.Join(root, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "credentials", "token"), []byte("fake-bat-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", batremote.CertificateFileName), fakeTerminalCertificateEnvelope(t, fake), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeTerminalBAT) serveHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	// Match batremote's sessionReadLimit: a 1 MiB agentrelay.MaxDataSize input
	// expands past the websocket default when base64-encoded.
	conn.SetReadLimit(8 << 20)
	select {
	case f.connections <- conn:
	default:
	}
	defer conn.CloseNow()
	defer func() {
		f.mu.Lock()
		f.order = append(f.order, "connection:closed")
		f.mu.Unlock()
		f.closed <- struct{}{}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payload, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var auth struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(payload, &auth) != nil || auth.ID == "" {
		return
	}
	authReply, _ := json.Marshal(map[string]any{
		"type": "auth-result", "id": auth.ID, "result": true,
		"protocol": batremote.ProtocolV2, "compression": "none",
		"serverVersion": "test", "capabilities": map[string]any{"profileContext": true},
	})
	if conn.Write(ctx, websocket.MessageText, authReply) != nil {
		return
	}

	for {
		messageType, payload, err := conn.Read(ctx)
		if err != nil || messageType != websocket.MessageText {
			return
		}
		var invoke struct {
			ID        string          `json:"id"`
			Channel   string          `json:"channel"`
			ContextID string          `json:"contextId"`
			Params    json.RawMessage `json:"params"`
		}
		if json.Unmarshal(payload, &invoke) != nil || invoke.ID == "" {
			return
		}
		f.mu.Lock()
		f.order = append(f.order, invoke.Channel)
		f.mu.Unlock()
		var result any = true
		invokeError := ""
		var invokedID string
		switch invoke.Channel {
		case "profile:open":
			f.profileOpened.Store(true)
			result = map[string]any{"contextId": "fake-profile-context", "status": "ready"}
		case "pty:create":
			var params struct {
				ID  string  `json:"id"`
				CWD *string `json:"cwd"`
			}
			if json.Unmarshal(invoke.Params, &params) != nil || params.ID == "" {
				return
			}
			invokedID = params.ID
			if params.CWD == nil {
				// Measured against bat-server 3.2.12.
				invokeError = "pty:create: invalid options: missing field `cwd`"
				break
			}
			f.mu.Lock()
			if f.createdCWD == nil {
				f.createdCWD = make(map[string]string)
			}
			f.createdCWD[params.ID] = *params.CWD
			f.mu.Unlock()
			if *params.CWD == f.failedCWD && f.failedCWD != "" {
				invokeError = "working directory unavailable"
				break
			}
			result = params.ID
			select {
			case f.created <- params.ID:
			default:
			}
		case "pty:write":
			var params struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(invoke.Params, &params) != nil || params.ID == "" {
				return
			}
			invokedID = params.ID
			select {
			case f.written <- params.ID:
			default:
			}
		case "pty:kill":
			var params struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(invoke.Params, &params) != nil || params.ID == "" {
				return
			}
			invokedID = params.ID
			f.mu.Lock()
			f.killed = append(f.killed, params.ID)
			resultStr := f.killResults[params.ID]
			f.mu.Unlock()
			if resultStr == "false" {
				result = false
			} else if resultStr == "error" {
				invokeError = "kill failed"
			}
		}
		var reply []byte
		if invokeError != "" {
			reply, _ = json.Marshal(map[string]any{"type": "invoke-error", "id": invoke.ID, "error": invokeError})
		} else {
			reply, _ = json.Marshal(map[string]any{"type": "invoke-result", "id": invoke.ID, "result": result})
		}
		if invoke.Channel == "pty:create" && invokedID == f.slowCreateID && f.releaseCreate != nil {
			go func() {
				<-f.releaseCreate
				_ = conn.Write(ctx, websocket.MessageText, reply)
			}()
			continue
		}
		if invoke.Channel == "pty:write" && invokedID == f.slowWriteID && f.releaseWrite != nil {
			go func() {
				<-f.releaseWrite
				_ = conn.Write(ctx, websocket.MessageText, reply)
			}()
			continue
		}
		if invoke.Channel == "pty:kill" && invokedID == f.slowKillID && f.releaseKill != nil {
			select {
			case f.killStarted <- invokedID:
			default:
			}
			go func() {
				<-f.releaseKill
				_ = conn.Write(ctx, websocket.MessageText, reply)
			}()
			continue
		}
		if conn.Write(ctx, websocket.MessageText, reply) != nil {
			return
		}
	}
}

func (f *fakeTerminalBAT) killedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

func (f *fakeTerminalBAT) note(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, event)
}

func (f *fakeTerminalBAT) cwdOf(ptyID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cwd, ok := f.createdCWD[ptyID]
	return cwd, ok
}

func (f *fakeTerminalBAT) invocationOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func fakeTerminalDeps(t *testing.T, fake *fakeTerminalBAT) terminalLinkDeps {
	t.Helper()
	deps := defaultTerminalLinkDeps()
	deps.userHomeDir = func() (string, error) { return "/unused", nil }
	deps.loadEndpoint = func(string, string) (batremote.Endpoint, error) {
		return fake.endpoint, nil
	}
	deps.openRecord = testTerminalRecordOpener(t, t.TempDir())
	deps.invocationID = func(context.Context) (string, bool) { return "", false }
	return deps
}

// testTerminalRecordOpener gives one test its own record and lock in dir,
// apart from the agent process's record.
func testTerminalRecordOpener(t *testing.T, dir string) func(string) (*terminalRecord, error) {
	t.Helper()
	store := &terminalRecordStore{}
	return func(string) (*terminalRecord, error) { return store.open(dir) }
}

func TestTerminalManagedEndpointLayoutAndCertificateEnvelope(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	home := t.TempDir()
	writeManagedTerminalEndpoint(t, home, fakeBAT)
	root := filepath.Join(home, ".local", "share", "clawctl", "bat-server")
	deps := defaultTerminalLinkDeps()
	managedEndpoint := deps.loadEndpoint
	managedRecord := &terminalRecordStore{}
	deps.openRecord = func(home string) (*terminalRecord, error) {
		return managedRecord.open(terminalRecordDir(home))
	}
	deps.invocationID = func(context.Context) (string, bool) { return "", false }
	deps.userHomeDir = func() (string, error) { return home, nil }
	deps.loadEndpoint = func(tokenPath, dataDir string) (batremote.Endpoint, error) {
		if want := filepath.Join(root, "credentials", "token"); tokenPath != want {
			t.Fatalf("token path = %q; want managed path %q", tokenPath, want)
		}
		if want := filepath.Join(root, "data"); dataDir != want {
			t.Fatalf("data path = %q; want managed path %q", dataDir, want)
		}
		endpoint, err := managedEndpoint(tokenPath, dataDir)
		if err != nil {
			return endpoint, err
		}
		if endpoint.Host != batremote.LoopbackHost || endpoint.Port != model.BATServerPort {
			t.Fatalf("managed endpoint = %s:%d; want %s:%d", endpoint.Host, endpoint.Port, batremote.LoopbackHost, model.BATServerPort)
		}
		// The fake listens on an ephemeral port: the managed port may belong to
		// this machine's own clawctl bat-server.
		endpoint.Port = fakeBAT.endpoint.Port
		return endpoint, nil
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, acceptErr := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if acceptErr == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "test complete")
		}
	}))
	defer hub.Close()
	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, deps)
	if !result.endpointAvailable || !result.batAvailable || !result.linked || result.logMessage != "" {
		t.Fatalf("attempt = %#v; want managed endpoint to establish and close normally", result)
	}
	lock, err := os.Stat(filepath.Join(home, ".local", "share", "clawctl", "terminal-ptys.lock"))
	if err != nil || lock.Mode().Perm() != 0o600 {
		t.Fatalf("terminal record lock = %v, %v; want a 0600 file beside the managed bat-server", lock, err)
	}
}

func writeTerminalDownstream(conn *websocket.Conn, frame agentrelay.Downstream) error {
	payload, err := agentrelay.EncodeDownstream(frame)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, payload)
}

func readTerminalUpstream(conn *websocket.Conn) (agentrelay.Upstream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		return agentrelay.Upstream{}, err
	}
	if messageType != websocket.MessageText {
		return agentrelay.Upstream{}, fmt.Errorf("upstream message type = %v", messageType)
	}
	return agentrelay.DecodeUpstream(payload)
}

func TestDeadBATSessionEndsTerminalLink(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	batSession, err := batremote.Dial(context.Background(), fakeBAT.endpoint, "clawctl-agent", "terminal-link", batremote.ClientInfo{
		AppName: "clawctl-agent", AppVersion: version, Label: "terminal-link",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batSession.Close() })
	profile, err := batSession.OpenProfileContext(context.Background(), agentTerminalProfile)
	if err != nil {
		t.Fatal(err)
	}

	linkCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	mapper := agentpty.New(profile, profile.ID(), "/home/terminal-test", nil, func(frame agentrelay.Upstream) {
		if frame.Type == agentrelay.UpstreamReady && frame.Session == "live-session" {
			ready <- struct{}{}
		}
	})
	dispatcher := newTerminalDispatcher(linkCtx, cancel, mapper, batSession, agentTerminalInvokeTimeout)
	if got := dispatcher.dispatch(agentrelay.Downstream{
		Type: agentrelay.DownstreamOpen, Session: "live-session", Cols: 80, Rows: 24,
	}, 0); got != terminalDispatchAccepted {
		t.Fatalf("open dispatch = %v; want accepted", got)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal session did not become ready")
	}

	serverConn := <-fakeBAT.connections
	_ = serverConn.CloseNow()
	select {
	case _, ok := <-batSession.Events():
		if ok {
			t.Fatal("unexpected BAT event while waiting for connection death")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bat-server connection did not die")
	}
	if err := batSession.Err(); err == nil {
		t.Fatal("bat-server connection did not die")
	}
	if got := dispatcher.dispatch(agentrelay.Downstream{
		Type: agentrelay.DownstreamInput, Session: "live-session", Data: []byte("after BAT died"),
	}, len("after BAT died")); got != terminalDispatchAccepted {
		t.Fatalf("input dispatch = %v; want accepted so the worker observes dead BAT", got)
	}
	select {
	case <-linkCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("dead bat-server session did not end terminal link")
	}
}

func TestTerminalRelayDataSizeFitsQueueByteBudget(t *testing.T) {
	if agentrelay.MaxDataSize > agentTerminalQueueByteBudget {
		t.Fatalf("agent relay maximum data size = %d; exceeds terminal queue byte budget %d",
			agentrelay.MaxDataSize, agentTerminalQueueByteBudget)
	}
}

func TestTerminalDispatcherCumulativeByteBudgetTearsDownLink(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	fakeBAT.slowWriteID = "aiintune-budget-session"
	fakeBAT.releaseWrite = make(chan struct{})
	var releaseWrite sync.Once
	release := func() { releaseWrite.Do(func() { close(fakeBAT.releaseWrite) }) }
	t.Cleanup(release)
	hubResult := make(chan error, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			hubResult <- err
			return
		}
		defer conn.CloseNow()
		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamOpen, Session: "budget-session", Cols: 80, Rows: 24,
		}); err != nil {
			hubResult <- err
			return
		}
		frame, err := readTerminalUpstream(conn)
		if err != nil || frame.Type != agentrelay.UpstreamReady || frame.Session != "budget-session" {
			hubResult <- fmt.Errorf("ready frame = %#v, %v", frame, err)
			return
		}
		fullFrame := agentrelay.Downstream{
			Type: agentrelay.DownstreamInput, Session: "budget-session",
			Data: bytes.Repeat([]byte("x"), agentTerminalQueueByteBudget),
		}
		if err := writeTerminalDownstream(conn, fullFrame); err != nil {
			hubResult <- err
			return
		}
		select {
		case id := <-fakeBAT.written:
			if id != fakeBAT.slowWriteID {
				hubResult <- fmt.Errorf("pty:write id = %q; want %q", id, fakeBAT.slowWriteID)
				return
			}
		case <-time.After(3 * time.Second):
			hubResult <- errors.New("first input did not reach stalled pty:write")
			return
		}
		if err := writeTerminalDownstream(conn, fullFrame); err != nil {
			hubResult <- err
			return
		}
		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamInput, Session: "budget-session", Data: []byte("x"),
		}); err != nil {
			hubResult <- err
			return
		}
		readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, _, err := conn.Read(readCtx); err == nil {
			hubResult <- errors.New("received an upstream frame instead of link teardown")
		} else if readCtx.Err() != nil {
			hubResult <- errors.New("dispatcher cumulative byte-budget refusal did not end terminal link")
		} else {
			hubResult <- nil
		}
	}))
	defer hub.Close()

	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, fakeTerminalDeps(t, fakeBAT))
	if err := <-hubResult; err != nil {
		t.Fatal(err)
	}
	if !result.endpointAvailable || !result.batAvailable || !result.linked || result.logMessage == "" {
		t.Fatalf("attempt = %#v; want established link ended by dispatcher refusal", result)
	}
}

func TestTerminalOpenWithoutCWDStartsInTheAgentHome(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	hubResult := make(chan error, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			hubResult <- err
			return
		}
		defer conn.CloseNow()
		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamOpen, Session: "home-session", Cols: 80, Rows: 24,
		}); err != nil {
			hubResult <- err
			return
		}
		frame, err := readTerminalUpstream(conn)
		if err != nil || frame.Type != agentrelay.UpstreamReady || frame.Session != "home-session" {
			hubResult <- fmt.Errorf("ready frame = %#v, %v", frame, err)
			return
		}
		hubResult <- conn.Close(websocket.StatusNormalClosure, "test complete")
	}))
	defer hub.Close()

	deps := fakeTerminalDeps(t, fakeBAT)
	deps.userHomeDir = func() (string, error) { return "/home/terminal-test", nil }
	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, deps)
	if err := <-hubResult; err != nil {
		t.Fatal(err)
	}
	if !result.linked {
		t.Fatalf("attempt = %#v; want an established link", result)
	}
	if cwd, ok := fakeBAT.cwdOf("aiintune-home-session"); !ok || cwd != "/home/terminal-test" {
		t.Fatalf("pty:create cwd = %q (sent %v); want the agent home", cwd, ok)
	}
}

func TestTerminalSessionOpenFailureDoesNotEndOtherSessionOrLink(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	fakeBAT.failedCWD = "/missing"
	hubResult := make(chan error, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			hubResult <- err
			return
		}
		defer conn.CloseNow()
		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamOpen, Session: "live-session", Cols: 80, Rows: 24,
		}); err != nil {
			hubResult <- err
			return
		}
		frame, err := readTerminalUpstream(conn)
		if err != nil || frame.Type != agentrelay.UpstreamReady || frame.Session != "live-session" {
			hubResult <- fmt.Errorf("live ready frame = %#v, %v", frame, err)
			return
		}
		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamOpen, Session: "failed-session", CWD: "/missing", Cols: 80, Rows: 24,
		}); err != nil {
			hubResult <- err
			return
		}
		frame, err = readTerminalUpstream(conn)
		if err != nil || frame.Type != agentrelay.UpstreamError || frame.Session != "failed-session" ||
			frame.Reason != agentpty.ReasonOpenFailed {
			hubResult <- fmt.Errorf("failed-session error frame = %#v, %v", frame, err)
			return
		}
		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamInput, Session: "live-session", Data: []byte("still alive"),
		}); err != nil {
			hubResult <- err
			return
		}
		select {
		case id := <-fakeBAT.written:
			if id != "aiintune-live-session" {
				hubResult <- fmt.Errorf("pty:write id = %q", id)
				return
			}
		case <-time.After(3 * time.Second):
			hubResult <- errors.New("live session stopped accepting input after another session failed")
			return
		}
		hubResult <- conn.Close(websocket.StatusNormalClosure, "test complete")
	}))
	defer hub.Close()

	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, fakeTerminalDeps(t, fakeBAT))
	if err := <-hubResult; err != nil {
		t.Fatal(err)
	}
	if !result.endpointAvailable || !result.batAvailable || !result.linked || result.logMessage != "" {
		t.Fatalf("attempt = %#v; want live link through normal close", result)
	}
}

func TestTerminalFrameAfterCloseIsDroppedWithoutEndingLink(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	fakeBAT.slowKillID = "aiintune-closing-session"
	fakeBAT.releaseKill = make(chan struct{})
	var releaseKill sync.Once
	release := func() { releaseKill.Do(func() { close(fakeBAT.releaseKill) }) }
	t.Cleanup(release)
	hubResult := make(chan error, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			hubResult <- err
			return
		}
		defer conn.CloseNow()
		for _, session := range []string{"closing-session", "surviving-session"} {
			if err := writeTerminalDownstream(conn, agentrelay.Downstream{
				Type: agentrelay.DownstreamOpen, Session: session, Cols: 80, Rows: 24,
			}); err != nil {
				hubResult <- err
				return
			}
		}
		ready := map[string]bool{}
		for len(ready) != 2 {
			frame, err := readTerminalUpstream(conn)
			if err != nil || frame.Type != agentrelay.UpstreamReady {
				hubResult <- fmt.Errorf("ready frame = %#v, %v", frame, err)
				return
			}
			ready[frame.Session] = true
		}
		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamClose, Session: "closing-session",
		}); err != nil {
			hubResult <- err
			return
		}
		select {
		case <-fakeBAT.killStarted:
		case <-time.After(3 * time.Second):
			hubResult <- errors.New("closing session did not start pty:kill")
			return
		}
		for _, frame := range []agentrelay.Downstream{
			{Type: agentrelay.DownstreamInput, Session: "closing-session", Data: []byte("late")},
			{Type: agentrelay.DownstreamInput, Session: "surviving-session", Data: []byte("still alive")},
		} {
			if err := writeTerminalDownstream(conn, frame); err != nil {
				hubResult <- err
				return
			}
		}
		select {
		case id := <-fakeBAT.written:
			if id != "aiintune-surviving-session" {
				hubResult <- fmt.Errorf("late frame reached BAT as pty:write for %q", id)
				return
			}
		case <-time.After(3 * time.Second):
			hubResult <- errors.New("surviving session stopped accepting input after late frame")
			return
		}
		release()
		hubResult <- conn.Close(websocket.StatusNormalClosure, "test complete")
	}))
	defer hub.Close()

	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, fakeTerminalDeps(t, fakeBAT))
	if err := <-hubResult; err != nil {
		t.Fatal(err)
	}
	if !result.endpointAvailable || !result.batAvailable || !result.linked || result.logMessage != "" {
		t.Fatalf("attempt = %#v; want live link through normal close", result)
	}
}

func TestTerminalLinkDropKillsPTYBeforeBATConnectionCloses(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	hubErr := make(chan error, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fakeBAT.profileOpened.Load() {
			hubErr <- errors.New("Hub was dialed before profile context opened")
			http.Error(w, "too early", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			hubErr <- err
			return
		}
		payload, err := agentrelay.EncodeDownstream(agentrelay.Downstream{
			Type: agentrelay.DownstreamOpen, Session: "drop-session", Cols: 100, Rows: 30,
		})
		if err == nil {
			writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, payload)
			cancel()
		}
		if err != nil {
			hubErr <- err
			_ = conn.CloseNow()
			return
		}
		select {
		case <-fakeBAT.created:
		case <-time.After(3 * time.Second):
			hubErr <- errors.New("fake bat-server did not observe pty:create")
		}
		_ = conn.CloseNow()
		hubErr <- nil
	}))
	defer hub.Close()

	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, fakeTerminalDeps(t, fakeBAT))
	if !result.batAvailable || !result.linked {
		t.Fatalf("attempt = %#v; want established link", result)
	}
	if err := <-hubErr; err != nil {
		t.Fatal(err)
	}
	select {
	case <-fakeBAT.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("fake bat-server connection did not close")
	}
	order := fakeBAT.invocationOrder()
	kill := indexString(order, "pty:kill")
	profileClose := indexString(order, "profile:close")
	connectionClose := indexString(order, "connection:closed")
	if kill < 0 || profileClose < 0 || connectionClose < 0 || kill > profileClose || profileClose > connectionClose {
		t.Fatalf("BAT invocation order = %v; want pty:kill, profile:close, connection close", order)
	}
}

func indexString(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func TestTerminalLinkDoesNotDialHubWhenBATUnavailable(t *testing.T) {
	deps := defaultTerminalLinkDeps()
	deps.userHomeDir = func() (string, error) { return "/does-not-exist", nil }
	deps.openRecord = testTerminalRecordOpener(t, t.TempDir())
	deps.loadEndpoint = func(string, string) (batremote.Endpoint, error) {
		return batremote.Endpoint{}, batremote.ErrTokenRead
	}
	deps.dialHub = func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		t.Fatal("Hub dialed without usable bat-server endpoint")
		return nil, nil, nil
	}
	result := runTerminalLinkAttempt(context.Background(), config{}, deps)
	if result.endpointAvailable || result.batAvailable || result.linked {
		t.Fatalf("attempt = %#v; want BAT unavailable", result)
	}
}

func TestTerminalAttemptClassifiesBATDialAndProfileFailuresAsUnreachable(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	t.Run("dial", func(t *testing.T) {
		deps := fakeTerminalDeps(t, fakeBAT)
		deps.dialBAT = func(context.Context, batremote.Endpoint, string, string, batremote.ClientInfo) (*batremote.Session, error) {
			return nil, errors.New("dial failed")
		}
		result := runTerminalLinkAttempt(context.Background(), config{}, deps)
		if !result.endpointAvailable || result.batAvailable || result.linked {
			t.Fatalf("attempt = %#v; want loaded but unreachable BAT", result)
		}
	})
	t.Run("profile", func(t *testing.T) {
		deps := fakeTerminalDeps(t, fakeBAT)
		deps.openProfile = func(context.Context, *batremote.Session, string) (*batremote.Context, error) {
			return nil, errors.New("profile failed")
		}
		result := runTerminalLinkAttempt(context.Background(), config{}, deps)
		if !result.endpointAvailable || result.batAvailable || result.linked {
			t.Fatalf("attempt = %#v; want loaded but unreachable BAT", result)
		}
	})
}

func TestSlowBATInvokeDoesNotStopHubReader(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	fakeBAT.slowCreateID = "aiintune-slow-session"
	fakeBAT.releaseCreate = make(chan struct{})
	hubErr := make(chan error, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			hubErr <- err
			return
		}
		defer conn.CloseNow()
		for _, session := range []string{"slow-session", "following-session"} {
			payload, encodeErr := agentrelay.EncodeDownstream(agentrelay.Downstream{
				Type: agentrelay.DownstreamOpen, Session: session, Cols: 80, Rows: 24,
			})
			if encodeErr != nil {
				hubErr <- encodeErr
				return
			}
			writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				hubErr <- err
				return
			}
		}
		seen := map[string]bool{}
		deadline := time.After(3 * time.Second)
		for len(seen) != 2 {
			select {
			case id := <-fakeBAT.created:
				seen[id] = true
			case <-deadline:
				hubErr <- fmt.Errorf("bat-server creates = %v; reader stalled behind slow invoke", seen)
				return
			}
		}
		close(fakeBAT.releaseCreate)
		hubErr <- nil
	}))
	defer hub.Close()

	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, fakeTerminalDeps(t, fakeBAT))
	if err := <-hubErr; err != nil {
		t.Fatal(err)
	}
	if !result.linked {
		t.Fatalf("attempt = %#v; want established link", result)
	}
}

func TestTerminalHubHandshakeFailuresDoNotLeakAgentToken(t *testing.T) {
	const agentToken = "agent-token-must-never-be-logged"
	fakeBAT := startFakeTerminalBAT(t)
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantLog    string
		checkAfter func(*testing.T)
	}{
		{
			name:    "redirect",
			wantLog: "redirect refused",
		},
		{
			name: "non-101",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Sec-WebSocket-Accept", agentToken)
				http.Error(w, "refused", http.StatusTeapot)
			},
			wantLog: "handshake refused",
		},
		{
			name: "missing subprotocol",
			handler: func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err == nil {
					defer conn.CloseNow()
					readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_, _, _ = conn.Read(readCtx)
				}
			},
			wantLog: "required subprotocol",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var redirected atomic.Bool
			var redirectedAuthorization atomic.Value
			redirectTarget := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				redirected.Store(true)
				redirectedAuthorization.Store(r.Header.Get("Authorization"))
			}))
			defer redirectTarget.Close()
			handler := tt.handler
			if handler == nil {
				handler = func(w http.ResponseWriter, _ *http.Request) {
					http.Redirect(w, &http.Request{}, redirectTarget.URL, http.StatusTemporaryRedirect)
				}
			}
			hub := httptest.NewServer(handler)
			defer hub.Close()

			var logs bytes.Buffer
			oldOutput := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(oldOutput) })
			ctx, cancel := context.WithCancel(context.Background())
			deps := fakeTerminalDeps(t, fakeBAT)
			deps.sleep = func(context.Context, time.Duration) error {
				cancel()
				return context.Canceled
			}
			runTerminalLinkWithDeps(ctx, config{
				HubURL: hub.URL, MachineID: "machine-test", AgentToken: agentToken,
			}, deps)
			gotLog := logs.String()
			if strings.Contains(gotLog, agentToken) {
				t.Fatalf("captured log contains agent token: %q", gotLog)
			}
			if !strings.Contains(gotLog, tt.wantLog) {
				t.Fatalf("captured log = %q; want category %q", gotLog, tt.wantLog)
			}
			if tt.name == "redirect" && redirected.Load() {
				authorization, _ := redirectedAuthorization.Load().(string)
				t.Fatalf("redirect was followed with Authorization %q", authorization)
			}
		})
	}
}

func TestTerminalHubRequestContract(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	requestResult := make(chan error, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch {
		case r.Method != http.MethodGet:
			err = fmt.Errorf("method = %s", r.Method)
		case r.URL.Path != agentTerminalPath:
			err = fmt.Errorf("path = %s", r.URL.Path)
		case r.Header.Get("Authorization") != "Bearer request-contract-token":
			err = errors.New("Authorization header mismatch")
		case len(r.Header.Values("Origin")) != 0:
			err = errors.New("Origin header was present")
		}
		requestResult <- err
		conn, acceptErr := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if acceptErr == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "retired")
		}
	}))
	defer hub.Close()

	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "request-contract-token",
	}, fakeTerminalDeps(t, fakeBAT))
	if err := <-requestResult; err != nil {
		t.Fatal(err)
	}
	if !result.batAvailable || !result.linked {
		t.Fatalf("attempt = %#v; want completed upgrade", result)
	}
}
