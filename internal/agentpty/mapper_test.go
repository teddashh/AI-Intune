package agentpty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/batremote"
)

type invocation struct {
	channel string
	params  json.RawMessage
}

type fakeInvoker struct {
	mu      sync.Mutex
	calls   []invocation
	handler func(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

func (f *fakeInvoker) Invoke(ctx context.Context, channel string, params any) (json.RawMessage, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, invocation{channel: channel, params: encoded})
	f.mu.Unlock()
	if f.handler != nil {
		return f.handler(ctx, channel, encoded)
	}
	return successfulResponse(channel, encoded)
}

func (f *fakeInvoker) snapshot() []invocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]invocation(nil), f.calls...)
}

func (f *fakeInvoker) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

// testHome stands in for the agent user's home directory.
const testHome = "/home/terminal-test"

func successfulResponse(channel string, params json.RawMessage) (json.RawMessage, error) {
	fields := objectFields(nil, params)
	switch channel {
	case "pty:create":
		// Measured against bat-server 3.2.12: a create without cwd is refused.
		if _, ok := fields["cwd"]; !ok {
			return nil, errors.New("pty:create: invalid options: missing field `cwd`")
		}
		return fields["id"], nil
	case "pty:write", "pty:kill":
		return json.RawMessage(`true`), nil
	case "pty:set-viewport-size":
		return json.Marshal(map[string]json.RawMessage{
			"cols": fields["cols"],
			"rows": fields["rows"],
			"ok":   json.RawMessage(`true`),
		})
	default:
		return nil, fmt.Errorf("unexpected channel %q", channel)
	}
}

type frameCollector struct {
	mu     sync.Mutex
	frames []agentrelay.Upstream
}

func (c *frameCollector) emit(frame agentrelay.Upstream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	frame.Data = append([]byte(nil), frame.Data...)
	c.frames = append(c.frames, frame)
}

func (c *frameCollector) snapshot() []agentrelay.Upstream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agentrelay.Upstream(nil), c.frames...)
}

func (c *frameCollector) reset() {
	c.mu.Lock()
	c.frames = nil
	c.mu.Unlock()
}

func objectFields(t *testing.T, payload json.RawMessage) map[string]json.RawMessage {
	if t != nil {
		t.Helper()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		if t == nil {
			panic(err)
		}
		t.Fatalf("unmarshal params %s: %v", payload, err)
	}
	return fields
}

func fieldString(t *testing.T, fields map[string]json.RawMessage, name string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(fields[name], &value); err != nil {
		t.Fatalf("unmarshal field %q: %v", name, err)
	}
	return value
}

func fieldInt(t *testing.T, fields map[string]json.RawMessage, name string) int {
	t.Helper()
	var value int
	if err := json.Unmarshal(fields[name], &value); err != nil {
		t.Fatalf("unmarshal field %q: %v", name, err)
	}
	return value
}

func openFrame(session, cwd string) agentrelay.Downstream {
	return agentrelay.Downstream{
		Type: agentrelay.DownstreamOpen, Session: session, CWD: cwd, Cols: 120, Rows: 40,
	}
}

func inputFrame(session, data string) agentrelay.Downstream {
	return agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: session, Data: []byte(data)}
}

func openTerminal(t *testing.T, mapper *Mapper, session string) {
	t.Helper()
	if err := mapper.HandleDownstream(context.Background(), openFrame(session, "/work")); err != nil {
		t.Fatalf("open: %v", err)
	}
}

func TestPtyIDIsDerivedFromSession(t *testing.T) {
	invoker := &fakeInvoker{}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	openTerminal(t, mapper, "session-1")

	calls := invoker.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d; want 1", len(calls))
	}
	fields := objectFields(t, calls[0].params)
	if got := fieldString(t, fields, "id"); got != "aiintune-session-1" {
		t.Fatalf("params.id = %q", got)
	}
}

func TestHubCannotChooseTheirPtyID(t *testing.T) {
	invoker := &fakeInvoker{}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	foreignID := "someone-elses-pty"
	if err := mapper.HandleDownstream(context.Background(), openFrame("ours", "/tmp/"+foreignID)); err != nil {
		t.Fatal(err)
	}
	if err := mapper.HandleDownstream(context.Background(), inputFrame("ours", foreignID)); err != nil {
		t.Fatal(err)
	}

	calls := invoker.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %d; want 2", len(calls))
	}
	for _, call := range calls {
		if got := fieldString(t, objectFields(t, call.params), "id"); got != "aiintune-ours" {
			t.Fatalf("%s params.id = %q", call.channel, got)
		}
	}
}

func TestOpenSendsMeasuredCreateParams(t *testing.T) {
	invoker := &fakeInvoker{}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	frame := openFrame("measured", "/srv/project")
	if err := mapper.HandleDownstream(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	calls := invoker.snapshot()
	if len(calls) != 1 || calls[0].channel != "pty:create" {
		t.Fatalf("calls = %#v", calls)
	}
	fields := objectFields(t, calls[0].params)
	if got := fieldString(t, fields, "type"); got != "shell" {
		t.Errorf("type = %q", got)
	}
	if got := fieldString(t, fields, "cwd"); got != frame.CWD {
		t.Errorf("cwd = %q", got)
	}
	if got := fieldInt(t, fields, "cols"); got != frame.Cols {
		t.Errorf("cols = %d", got)
	}
	if got := fieldInt(t, fields, "rows"); got != frame.Rows {
		t.Errorf("rows = %d", got)
	}
}

func TestOpenWithoutCWDStartsInTheDefaultDirectory(t *testing.T) {
	invoker := &fakeInvoker{}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	if err := mapper.HandleDownstream(context.Background(), openFrame("default-cwd", "")); err != nil {
		t.Fatal(err)
	}
	fields := objectFields(t, invoker.snapshot()[0].params)
	if got := fieldString(t, fields, "cwd"); got != testHome {
		t.Fatalf("cwd = %q; want %q", got, testHome)
	}
}

func TestOpenAlwaysSendsCWD(t *testing.T) {
	invoker := &fakeInvoker{}
	mapper := New(invoker, "context-1", "", nil, nil)
	if err := mapper.HandleDownstream(context.Background(), openFrame("no-default", "")); err != nil {
		t.Fatalf("open without a default directory: %v", err)
	}
	fields := objectFields(t, invoker.snapshot()[0].params)
	if got, ok := fields["cwd"]; !ok || string(got) != `""` {
		t.Fatalf("params = %s; want cwd present and empty", invoker.snapshot()[0].params)
	}
}

func TestCreateIDMismatchIsAnError(t *testing.T) {
	invoker := &fakeInvoker{handler: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"different-id"`), nil
	}}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	err := mapper.HandleDownstream(context.Background(), openFrame("mismatch", ""))
	if !errors.Is(err, ErrCreateIDMismatch) {
		t.Fatalf("error = %v", err)
	}
	got := frames.snapshot()
	if len(got) != 1 || got[0].Type != agentrelay.UpstreamError {
		t.Fatalf("frames = %#v", got)
	}
}

func TestOpenFailuresIssueCompensatingKill(t *testing.T) {
	tests := []struct {
		name       string
		create     func(json.RawMessage) (json.RawMessage, error)
		wantError  error
		wantKillID string
	}{
		{
			name: "invoke error", create: func(json.RawMessage) (json.RawMessage, error) {
				return nil, errors.New("create failed")
			}, wantError: ErrOpenFailed, wantKillID: "aiintune-invoke-error",
		},
		{
			name: "invalid response", create: func(json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"not":"an id"}`), nil
			}, wantError: ErrOpenFailed, wantKillID: "aiintune-invalid-response",
		},
		{
			name: "ID mismatch", create: func(json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`"created-by-server"`), nil
			}, wantError: ErrCreateIDMismatch, wantKillID: "created-by-server",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := strings.ReplaceAll(tt.name, " ", "-")
			invoker := &fakeInvoker{handler: func(_ context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
				switch channel {
				case "pty:create":
					return tt.create(params)
				case "pty:kill":
					return json.RawMessage(`true`), nil
				default:
					return nil, fmt.Errorf("unexpected channel %q", channel)
				}
			}}
			mapper := New(invoker, "context-1", testHome, nil, nil)
			err := mapper.HandleDownstream(context.Background(), openFrame(session, ""))
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("error = %v; want %v", err, tt.wantError)
			}
			calls := invoker.snapshot()
			if len(calls) != 2 || calls[0].channel != "pty:create" || calls[1].channel != "pty:kill" {
				t.Fatalf("calls = %#v; want create then compensating kill", calls)
			}
			if got := fieldString(t, objectFields(t, calls[1].params), "id"); got != tt.wantKillID {
				t.Fatalf("kill id = %q; want %q", got, tt.wantKillID)
			}
		})
	}
}

func TestShutdownKillsEveryTrackedPTYIncludingOpeningAndClosing(t *testing.T) {
	invoker := &fakeInvoker{handler: func(_ context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
		if channel == "pty:kill" && fieldString(t, objectFields(t, params), "id") == "aiintune-closing" {
			return nil, errors.New("first kill failed")
		}
		return successfulResponse(channel, params)
	}}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	openTerminal(t, mapper, "active")
	openTerminal(t, mapper, "closing")
	if err := mapper.HandleDownstream(context.Background(), agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "closing"}); !errors.Is(err, ErrCloseFailed) {
		t.Fatalf("close error = %v; want %v", err, ErrCloseFailed)
	}
	mapper.sessionsMu.Lock()
	mapper.sessions["opening"] = &sessionState{phase: phaseOpening}
	mapper.sessionsMu.Unlock()
	invoker.reset()
	invoker.handler = nil

	if err := mapper.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	want := map[string]bool{
		"aiintune-active": false, "aiintune-closing": false, "aiintune-opening": false,
	}
	for _, call := range invoker.snapshot() {
		if call.channel != "pty:kill" {
			t.Fatalf("call channel = %q; want pty:kill", call.channel)
		}
		want[fieldString(t, objectFields(t, call.params), "id")] = true
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("missing kill for %q", id)
		}
	}
	mapper.sessionsMu.Lock()
	remaining := len(mapper.sessions)
	mapper.sessionsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("tracked sessions = %d; want 0", remaining)
	}
}

func TestShutdownEmitsNoUpstreamFrames(t *testing.T) {
	frames := &frameCollector{}
	mapper := New(&fakeInvoker{}, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "quiet")
	frames.reset()
	if err := mapper.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := frames.snapshot(); len(got) != 0 {
		t.Fatalf("frames = %#v; want none", got)
	}
}

func TestShutdownReturnsAllKillFailures(t *testing.T) {
	errOne := errors.New("kill one")
	errTwo := errors.New("kill two")
	invoker := &fakeInvoker{handler: func(_ context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
		if channel == "pty:create" {
			return successfulResponse(channel, params)
		}
		switch fieldString(t, objectFields(t, params), "id") {
		case "aiintune-one":
			return nil, errOne
		case "aiintune-two":
			return nil, errTwo
		default:
			return nil, fmt.Errorf("unexpected kill")
		}
	}}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	openTerminal(t, mapper, "one")
	openTerminal(t, mapper, "two")
	err := mapper.Shutdown(context.Background())
	if !errors.Is(err, ErrCloseFailed) || !strings.Contains(err.Error(), errOne.Error()) || !strings.Contains(err.Error(), errTwo.Error()) {
		t.Fatalf("Shutdown() error = %v; want both kill failures and %v", err, ErrCloseFailed)
	}
}

func TestShutdownRacingOpenKillsAgainAfterCreateCompletes(t *testing.T) {
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	invoker := &fakeInvoker{handler: func(_ context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
		if channel == "pty:create" {
			close(createStarted)
			<-releaseCreate
			return successfulResponse(channel, params)
		}
		return successfulResponse(channel, params)
	}}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openResult := make(chan error, 1)
	go func() {
		openResult <- mapper.HandleDownstream(context.Background(), openFrame("racing-open", ""))
	}()
	<-createStarted
	if err := mapper.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(releaseCreate)
	if err := <-openResult; !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("open error = %v; want %v", err, ErrSessionNotFound)
	}
	var kills int
	for _, call := range invoker.snapshot() {
		if call.channel == "pty:kill" {
			kills++
		}
	}
	if kills != 2 {
		t.Fatalf("kill calls = %d; want cleanup kill plus post-create compensation", kills)
	}
	if got := frames.snapshot(); len(got) != 0 {
		t.Fatalf("frames = %#v; want no ready frame after shutdown", got)
	}
}

func TestResizeUsesSetViewportSize(t *testing.T) {
	invoker := &fakeInvoker{}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	openTerminal(t, mapper, "resize")
	invoker.reset()
	frame := agentrelay.Downstream{Type: agentrelay.DownstreamResize, Session: "resize", Cols: 91, Rows: 27}
	if err := mapper.HandleDownstream(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	calls := invoker.snapshot()
	if len(calls) != 1 || calls[0].channel != "pty:set-viewport-size" {
		t.Fatalf("calls = %#v", calls)
	}
	if calls[0].channel == "pty:resize" {
		t.Fatal("used mobile-only pty:resize channel")
	}
	fields := objectFields(t, calls[0].params)
	if got := fieldString(t, fields, "source"); got != "desktop" {
		t.Errorf("source = %q", got)
	}
	if fieldInt(t, fields, "cols") != frame.Cols || fieldInt(t, fields, "rows") != frame.Rows {
		t.Errorf("viewport params = %s", calls[0].params)
	}
}

func TestInputIsRawStringNotBase64(t *testing.T) {
	invoker := &fakeInvoker{}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	openTerminal(t, mapper, "input")
	invoker.reset()
	if err := mapper.HandleDownstream(context.Background(), inputFrame("input", "ls\r")); err != nil {
		t.Fatal(err)
	}
	fields := objectFields(t, invoker.snapshot()[0].params)
	if got := fieldString(t, fields, "data"); got != "ls\r" {
		t.Fatalf("data = %q; want raw input", got)
	}
	if got := fieldString(t, fields, "data"); got == "bHMN" {
		t.Fatal("input was base64 encoded")
	}
}

func TestInvalidUTF8InputIsRejected(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "invalid-utf8")
	invoker.reset()
	frames.reset()
	frame := agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: "invalid-utf8", Data: []byte{0xff, 0xfe}}
	err := mapper.HandleDownstream(context.Background(), frame)
	if !errors.Is(err, ErrInvalidUTF8Input) {
		t.Fatalf("error = %v", err)
	}
	if calls := invoker.snapshot(); len(calls) != 0 {
		t.Fatalf("invocations = %#v", calls)
	}
	got := frames.snapshot()
	if len(got) != 1 || got[0].Type != agentrelay.UpstreamError {
		t.Fatalf("frames = %#v", got)
	}
}

func TestOutputEventFromForeignPtyIsDropped(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "ours")
	frames.reset()
	err := mapper.HandleEvent(batremote.Event{
		Channel: "pty:output", ContextID: "context-1",
		Params: json.RawMessage(`{"id":"someone-elses-pty","data":"secret"}`),
	})
	if err != nil || len(frames.snapshot()) != 0 {
		t.Fatalf("error/frames = %v/%#v", err, frames.snapshot())
	}
}

func TestEventFromForeignContextIsDropped(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "ours")
	frames.reset()
	err := mapper.HandleEvent(batremote.Event{
		Channel: "pty:output", ContextID: "context-2",
		Params: json.RawMessage(`{"id":"aiintune-ours","data":"secret"}`),
	})
	if err != nil || len(frames.snapshot()) != 0 {
		t.Fatalf("error/frames = %v/%#v", err, frames.snapshot())
	}
}

func TestOutputEventBecomesUpstreamOutput(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "output")
	frames.reset()
	err := mapper.HandleEvent(batremote.Event{
		Channel: "pty:output", ContextID: "context-1",
		Params: json.RawMessage(`{"id":"aiintune-output","data":"echo hi\r\n"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := frames.snapshot()
	want := []agentrelay.Upstream{{Type: agentrelay.UpstreamOutput, Session: "output", Data: []byte("echo hi\r\n")}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %#v; want %#v", got, want)
	}
}

func TestExitEventBecomesUpstreamExit(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "exit")
	frames.reset()
	err := mapper.HandleEvent(batremote.Event{
		Channel: "pty:exit", ContextID: "context-1",
		Params: json.RawMessage(`{"id":"aiintune-exit","exitCode":7}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := frames.snapshot()
	if len(got) != 1 || got[0].Type != agentrelay.UpstreamExit || got[0].Session != "exit" || got[0].Code == nil || *got[0].Code != 7 {
		t.Fatalf("frames = %#v", got)
	}
}

func TestInputOrderIsPreservedPerSession(t *testing.T) {
	var mu sync.Mutex
	var received []string
	started := make(chan string, 10)
	releases := make(map[string]chan struct{}, 10)
	for i := 0; i < 10; i++ {
		releases[fmt.Sprintf("input-%02d", i)] = make(chan struct{}, 1)
	}
	invoker := &fakeInvoker{handler: func(_ context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
		if channel == "pty:create" {
			return successfulResponse(channel, params)
		}
		data := fieldString(t, objectFields(t, params), "data")
		mu.Lock()
		received = append(received, data)
		position := len(received)
		mu.Unlock()
		started <- data
		<-releases[data]
		time.Sleep(time.Duration(10-position) * 100 * time.Microsecond)
		return json.RawMessage(`true`), nil
	}}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	openTerminal(t, mapper, "ordered")

	want := make([]string, 10)
	results := make([]chan error, 10)
	for i := 0; i < 10; i++ {
		want[i] = fmt.Sprintf("input-%02d", i)
		results[i] = make(chan error, 1)
	}
	start := func(index int) {
		go func() {
			results[index] <- mapper.HandleDownstream(context.Background(), inputFrame("ordered", want[index]))
		}()
	}
	start(0)
	if got := <-started; got != want[0] {
		t.Fatalf("first invocation = %q; want %q", got, want[0])
	}
	for i := 0; i < 9; i++ {
		start(i + 1)
		select {
		case got := <-started:
			t.Fatalf("%q invoked while %q was still blocked", got, want[i])
		case <-time.After(20 * time.Millisecond):
		}
		releases[want[i]] <- struct{}{}
		if err := <-results[i]; err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-started:
			if got != want[i+1] {
				t.Fatalf("invocation %d = %q; want %q", i+1, got, want[i+1])
			}
		case <-time.After(time.Second):
			t.Fatalf("invocation %d did not start", i+1)
		}
	}
	releases[want[9]] <- struct{}{}
	if err := <-results[9]; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("received = %#v; want %#v", received, want)
	}
}

func TestConcurrentSessionsDoNotBlockEachOther(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	invoker := &fakeInvoker{handler: func(_ context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
		if channel != "pty:write" {
			return successfulResponse(channel, params)
		}
		id := fieldString(t, objectFields(t, params), "id")
		if id == "aiintune-slow" {
			close(blocked)
			<-release
		}
		return json.RawMessage(`true`), nil
	}}
	mapper := New(invoker, "context-1", testHome, nil, nil)
	openTerminal(t, mapper, "slow")
	openTerminal(t, mapper, "fast")
	slowDone := make(chan error, 1)
	go func() { slowDone <- mapper.HandleDownstream(context.Background(), inputFrame("slow", "a")) }()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("slow session did not reach invoker")
	}
	fastDone := make(chan error, 1)
	go func() { fastDone <- mapper.HandleDownstream(context.Background(), inputFrame("fast", "b")) }()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fast session was blocked by slow session")
	}
	close(release)
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateOpenIsRejected(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "duplicate")
	frames.reset()
	err := mapper.HandleDownstream(context.Background(), openFrame("duplicate", "/other"))
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("error = %v", err)
	}
	if len(invoker.snapshot()) != 1 {
		t.Fatalf("invocations = %d; want 1", len(invoker.snapshot()))
	}
	got := frames.snapshot()
	if len(got) != 1 || got[0].Reason != ReasonSessionAlreadyOpen {
		t.Fatalf("frames = %#v", got)
	}
}

func TestCloseUnknownSessionIsSilent(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	err := mapper.HandleDownstream(context.Background(), agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "unknown"})
	if err != nil || len(invoker.snapshot()) != 0 || len(frames.snapshot()) != 0 {
		t.Fatalf("error/calls/frames = %v/%d/%#v", err, len(invoker.snapshot()), frames.snapshot())
	}
}

func TestCloseEmitsNothingAndExitEventRemoves(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "closing")
	frames.reset()
	if err := mapper.HandleDownstream(context.Background(), agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "closing"}); err != nil {
		t.Fatal(err)
	}
	if got := frames.snapshot(); len(got) != 0 {
		t.Fatalf("close frames = %#v", got)
	}
	event := batremote.Event{
		Channel: "pty:exit", ContextID: "context-1",
		Params: json.RawMessage(`{"id":"aiintune-closing","exitCode":7}`),
	}
	if err := mapper.HandleEvent(event); err != nil {
		t.Fatal(err)
	}
	if err := mapper.HandleEvent(event); err != nil {
		t.Fatal(err)
	}
	got := frames.snapshot()
	if len(got) != 1 || got[0].Type != agentrelay.UpstreamExit {
		t.Fatalf("frames = %#v", got)
	}
}

func TestReasonsAreClosedVocabulary(t *testing.T) {
	sentinel := errors.New("pty:create: invalid `/private/path`: missing field `id`")
	collector := &frameCollector{}

	failedOpen := New(&fakeInvoker{handler: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return nil, sentinel
	}}, "context-1", testHome, nil, collector.emit)
	_ = failedOpen.HandleDownstream(context.Background(), openFrame("open-fails", ""))

	duplicateInvoker := &fakeInvoker{}
	duplicate := New(duplicateInvoker, "context-1", testHome, nil, collector.emit)
	openTerminal(t, duplicate, "duplicate-reason")
	_ = duplicate.HandleDownstream(context.Background(), openFrame("duplicate-reason", ""))

	missing := New(&fakeInvoker{}, "context-1", testHome, nil, collector.emit)
	_ = missing.HandleDownstream(context.Background(), inputFrame("missing", "x"))

	invalidInvoker := &fakeInvoker{}
	invalid := New(invalidInvoker, "context-1", testHome, nil, collector.emit)
	openTerminal(t, invalid, "invalid-reason")
	_ = invalid.HandleDownstream(context.Background(), agentrelay.Downstream{
		Type: agentrelay.DownstreamInput, Session: "invalid-reason", Data: []byte{0xff},
	})

	connectionInvoker := &fakeInvoker{handler: func(_ context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
		if channel == "pty:create" {
			return successfulResponse(channel, params)
		}
		return nil, sentinel
	}}
	connection := New(connectionInvoker, "context-1", testHome, nil, collector.emit)
	openTerminal(t, connection, "connection-reason")
	_ = connection.HandleDownstream(context.Background(), inputFrame("connection-reason", "x"))

	for _, test := range []struct {
		name      string
		channel   string
		result    json.RawMessage
		invokeErr error
		action    func(*Mapper) error
	}{
		{
			name: "create malformed response", channel: "pty:create", result: json.RawMessage(`{}`),
			action: func(mapper *Mapper) error {
				return mapper.HandleDownstream(context.Background(), openFrame("create-malformed", ""))
			},
		},
		{
			name: "create mismatched ID", channel: "pty:create", result: json.RawMessage(`"foreign"`),
			action: func(mapper *Mapper) error {
				return mapper.HandleDownstream(context.Background(), openFrame("create-mismatch-reason", ""))
			},
		},
		{
			name: "write false", channel: "pty:write", result: json.RawMessage(`false`),
			action: func(mapper *Mapper) error {
				openTerminal(t, mapper, "write-false")
				return mapper.HandleDownstream(context.Background(), inputFrame("write-false", "x"))
			},
		},
		{
			name: "resize malformed response", channel: "pty:set-viewport-size", result: json.RawMessage(`false`),
			action: func(mapper *Mapper) error {
				openTerminal(t, mapper, "resize-malformed")
				return mapper.HandleDownstream(context.Background(), agentrelay.Downstream{
					Type: agentrelay.DownstreamResize, Session: "resize-malformed", Cols: 80, Rows: 24,
				})
			},
		},
		{
			name: "resize invoke error", channel: "pty:set-viewport-size", invokeErr: sentinel,
			action: func(mapper *Mapper) error {
				openTerminal(t, mapper, "resize-invoke-error")
				return mapper.HandleDownstream(context.Background(), agentrelay.Downstream{
					Type: agentrelay.DownstreamResize, Session: "resize-invoke-error", Cols: 80, Rows: 24,
				})
			},
		},
		{
			name: "close false", channel: "pty:kill", result: json.RawMessage(`false`),
			action: func(mapper *Mapper) error {
				openTerminal(t, mapper, "close-false")
				return mapper.HandleDownstream(context.Background(), agentrelay.Downstream{
					Type: agentrelay.DownstreamClose, Session: "close-false",
				})
			},
		},
		{
			name: "close invoke error", channel: "pty:kill", invokeErr: sentinel,
			action: func(mapper *Mapper) error {
				openTerminal(t, mapper, "close-invoke-error")
				return mapper.HandleDownstream(context.Background(), agentrelay.Downstream{
					Type: agentrelay.DownstreamClose, Session: "close-invoke-error",
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			invoker := &fakeInvoker{handler: func(_ context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
				if channel == test.channel {
					return test.result, test.invokeErr
				}
				return successfulResponse(channel, params)
			}}
			if err := test.action(New(invoker, "context-1", testHome, nil, collector.emit)); err == nil {
				t.Fatal("error = nil")
			}
		})
	}

	unknownResize := New(&fakeInvoker{}, "context-1", testHome, nil, collector.emit)
	_ = unknownResize.HandleDownstream(context.Background(), agentrelay.Downstream{
		Type: agentrelay.DownstreamResize, Session: "missing-resize", Cols: 80, Rows: 24,
	})

	for _, event := range []batremote.Event{
		{Channel: "pty:output", ContextID: "context-1", Params: json.RawMessage(`{"id":"aiintune-bad-output"}`)},
		{Channel: "pty:exit", ContextID: "context-1", Params: json.RawMessage(`{"id":"aiintune-bad-exit"}`)},
	} {
		session := strings.TrimPrefix(fieldString(t, objectFields(t, event.Params), "id"), ptyIDPrefix)
		mapper := New(&fakeInvoker{}, "context-1", testHome, nil, collector.emit)
		openTerminal(t, mapper, session)
		if err := mapper.HandleEvent(event); err == nil {
			t.Errorf("HandleEvent(%s) error = nil", event.Channel)
		}
	}

	oversizedOutput, err := json.Marshal(map[string]string{
		"id": "aiintune-oversized-output", "data": strings.Repeat("x", agentrelay.MaxDataSize+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	oversizedMapper := New(&fakeInvoker{}, "context-1", testHome, nil, collector.emit)
	openTerminal(t, oversizedMapper, "oversized-output")
	if err := oversizedMapper.HandleEvent(batremote.Event{
		Channel: "pty:output", ContextID: "context-1", Params: oversizedOutput,
	}); err == nil {
		t.Error("oversized output error = nil")
	}

	overflowExit := New(&fakeInvoker{}, "context-1", testHome, nil, collector.emit)
	openTerminal(t, overflowExit, "overflow-exit")
	if err := overflowExit.HandleEvent(batremote.Event{
		Channel: "pty:exit", ContextID: "context-1",
		Params: json.RawMessage(`{"id":"aiintune-overflow-exit","exitCode":2147483648}`),
	}); err == nil {
		t.Error("overflow exit error = nil")
	}

	allowed := map[string]bool{
		ReasonOpenFailed:            true,
		ReasonTerminalUnavailable:   true,
		ReasonSessionAlreadyOpen:    true,
		ReasonInputInvalidUTF8:      true,
		ReasonConnectionInterrupted: true,
	}
	seen := make(map[string]bool)
	for _, frame := range collector.snapshot() {
		if frame.Type != agentrelay.UpstreamError {
			continue
		}
		if !allowed[frame.Reason] {
			t.Errorf("reason is outside closed vocabulary: %q", frame.Reason)
		}
		seen[frame.Reason] = true
		for _, fragment := range []string{"pty:create", "/private/path", "missing field", "`id`"} {
			if strings.Contains(frame.Reason, fragment) {
				t.Errorf("reason %q contains server fragment %q", frame.Reason, fragment)
			}
		}
		if _, err := agentrelay.EncodeUpstream(frame); err != nil {
			t.Errorf("EncodeUpstream(%#v): %v", frame, err)
		}
	}
	if len(seen) != len(allowed) {
		t.Fatalf("seen reasons = %#v; want all %#v", seen, allowed)
	}
}

func TestEverySentFrameEncodes(t *testing.T) {
	invoker := &fakeInvoker{}
	frames := &frameCollector{}
	mapper := New(invoker, "context-1", testHome, nil, frames.emit)
	openTerminal(t, mapper, "all-frames")
	if err := mapper.HandleEvent(batremote.Event{
		Channel: "pty:output", ContextID: "context-1",
		Params: json.RawMessage(`{"id":"aiintune-all-frames","data":"ok"}`),
	}); err != nil {
		t.Fatal(err)
	}
	_ = mapper.HandleDownstream(context.Background(), agentrelay.Downstream{
		Type: agentrelay.DownstreamInput, Session: "all-frames", Data: []byte{0xff},
	})
	if err := mapper.HandleEvent(batremote.Event{
		Channel: "pty:exit", ContextID: "context-1",
		Params: json.RawMessage(`{"id":"aiintune-all-frames","exitCode":0}`),
	}); err != nil {
		t.Fatal(err)
	}
	got := frames.snapshot()
	if len(got) != 4 {
		t.Fatalf("frames = %#v", got)
	}
	for _, frame := range got {
		if _, err := agentrelay.EncodeUpstream(frame); err != nil {
			t.Errorf("EncodeUpstream(%#v): %v", frame, err)
		}
	}
}
