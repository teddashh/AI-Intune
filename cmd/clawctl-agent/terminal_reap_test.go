package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/batremote"
)

func TestTerminalRecordReap(t *testing.T) {
	cases := []struct {
		name              string
		seedEntries       []terminalRecordEntry
		invocationID      string
		invocationOK      bool
		killResults       map[string]string
		wantKilled        []string
		wantRecordEntries []terminalRecordEntry
	}{
		{
			name:              "other invocation ID skips kill and drops entry",
			seedEntries:       []terminalRecordEntry{{ID: "aiintune-old", Invocation: testOtherInvocation}},
			invocationID:      testCurrentInvocation,
			invocationOK:      true,
			killResults:       nil,
			wantKilled:        []string{},
			wantRecordEntries: []terminalRecordEntry{},
		},
		{
			name:              "current invocation ID kills and drops entry",
			seedEntries:       []terminalRecordEntry{{ID: "aiintune-cur", Invocation: testCurrentInvocation}},
			invocationID:      testCurrentInvocation,
			invocationOK:      true,
			killResults:       nil,
			wantKilled:        []string{"aiintune-cur"},
			wantRecordEntries: []terminalRecordEntry{},
		},
		{
			name:              "kill returning false keeps entry",
			seedEntries:       []terminalRecordEntry{{ID: "aiintune-cur", Invocation: testCurrentInvocation}},
			invocationID:      testCurrentInvocation,
			invocationOK:      true,
			killResults:       map[string]string{"aiintune-cur": "false"},
			wantKilled:        []string{"aiintune-cur"},
			wantRecordEntries: []terminalRecordEntry{{ID: "aiintune-cur", Invocation: testCurrentInvocation}},
		},
		{
			name:              "kill returning error keeps entry",
			seedEntries:       []terminalRecordEntry{{ID: "aiintune-cur", Invocation: testCurrentInvocation}},
			invocationID:      testCurrentInvocation,
			invocationOK:      true,
			killResults:       map[string]string{"aiintune-cur": "error"},
			wantKilled:        []string{"aiintune-cur"},
			wantRecordEntries: []terminalRecordEntry{{ID: "aiintune-cur", Invocation: testCurrentInvocation}},
		},
		{
			name:              "empty invocation ID kills and drops entry",
			seedEntries:       []terminalRecordEntry{{ID: "aiintune-blank", Invocation: ""}},
			invocationID:      testCurrentInvocation,
			invocationOK:      true,
			killResults:       nil,
			wantKilled:        []string{"aiintune-blank"},
			wantRecordEntries: []terminalRecordEntry{},
		},
		{
			name:              "unavailable current invocation kills both",
			seedEntries:       []terminalRecordEntry{{ID: "aiintune-old", Invocation: testOtherInvocation}, {ID: "aiintune-cur", Invocation: testCurrentInvocation}},
			invocationID:      "",
			invocationOK:      false,
			killResults:       nil,
			wantKilled:        []string{"aiintune-old", "aiintune-cur"},
			wantRecordEntries: []terminalRecordEntry{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeBAT := startFakeTerminalBAT(t)
			if tc.killResults != nil {
				fakeBAT.killResults = tc.killResults
			}

			dir := t.TempDir()
			store := &terminalRecordStore{}
			record, err := store.open(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range tc.seedEntries {
				if err := record.Add(entry.ID, entry.Invocation); err != nil {
					t.Fatal(err)
				}
			}

			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, acceptErr := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
				if acceptErr == nil {
					_ = conn.Close(websocket.StatusNormalClosure, "test complete")
				}
			}))
			defer hub.Close()

			deps := fakeTerminalDeps(t, fakeBAT)
			deps.openRecord = func(string) (*terminalRecord, error) { return store.open(dir) }
			deps.invocationID = func(context.Context) (string, bool) { return tc.invocationID, tc.invocationOK }
			deps.dialHub = func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
				fakeBAT.note("hub:dial")
				return websocket.Dial(ctx, url, opts)
			}

			result := runTerminalLinkAttempt(context.Background(), config{
				HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
			}, deps)

			if !result.linked {
				t.Errorf("result.linked = %v; want true", result.linked)
			}

			killed := fakeBAT.killedIDs()
			if len(killed) != len(tc.wantKilled) {
				t.Errorf("killed = %v; want %v", killed, tc.wantKilled)
			} else {
				for i, id := range tc.wantKilled {
					if killed[i] != id {
						t.Errorf("killed[%d] = %q; want %q", i, killed[i], id)
					}
				}
			}

			entries := record.entries()
			if len(entries) != len(tc.wantRecordEntries) {
				t.Errorf("record entries = %v; want %v", entries, tc.wantRecordEntries)
			} else {
				for i, entry := range tc.wantRecordEntries {
					if entries[i] != entry {
						t.Errorf("record entries[%d] = %v; want %v", i, entries[i], entry)
					}
				}
			}

			order := fakeBAT.invocationOrder()
			hubDialIdx := -1
			profileOpenIdx := -1
			lastKillIdx := -1
			for i, event := range order {
				if event == "hub:dial" {
					hubDialIdx = i
				}
				if event == "profile:open" {
					profileOpenIdx = i
				}
				if event == "pty:kill" {
					lastKillIdx = i
				}
			}
			if hubDialIdx == -1 {
				t.Errorf("hub:dial not found in order: %v", order)
			}
			if profileOpenIdx == -1 {
				t.Errorf("profile:open not found in order: %v", order)
			}
			if profileOpenIdx > hubDialIdx {
				t.Errorf("profile:open at %d, hub:dial at %d; want hub:dial after profile:open", profileOpenIdx, hubDialIdx)
			}
			if lastKillIdx != -1 && lastKillIdx > hubDialIdx {
				t.Errorf("pty:kill at %d, hub:dial at %d; want hub:dial after every pty:kill", lastKillIdx, hubDialIdx)
			}
		})
	}
}

func TestTerminalLinkRecordsInvocationID(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	dir := t.TempDir()
	store := &terminalRecordStore{}
	record, err := store.open(dir)
	if err != nil {
		t.Fatal(err)
	}

	snapshotCh := make(chan []terminalRecordEntry, 1)

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()

		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamOpen, Session: "s1", Cols: 80, Rows: 24,
		}); err != nil {
			t.Error(err)
			return
		}

		frame, err := readTerminalUpstream(conn)
		if err != nil || frame.Type != agentrelay.UpstreamReady || frame.Session != "s1" {
			t.Errorf("ready frame = %#v, %v", frame, err)
			return
		}

		snapshotCh <- append([]terminalRecordEntry(nil), record.entries()...)
		_ = conn.Close(websocket.StatusNormalClosure, "test complete")
	}))
	defer hub.Close()

	deps := fakeTerminalDeps(t, fakeBAT)
	deps.openRecord = func(string) (*terminalRecord, error) { return store.open(dir) }
	deps.invocationID = func(ctx context.Context) (string, bool) {
		if fakeBAT.profileOpened.Load() {
			return testCurrentInvocation, true
		}
		return testOtherInvocation, true
	}
	deps.dialHub = func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		fakeBAT.note("hub:dial")
		return websocket.Dial(ctx, url, opts)
	}

	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, deps)

	if !result.linked {
		t.Fatalf("linked = false; want true")
	}

	select {
	case snapshot := <-snapshotCh:
		if len(snapshot) != 1 || snapshot[0].ID != "aiintune-s1" || snapshot[0].Invocation != testCurrentInvocation {
			t.Errorf("record entries = %v; want [{ID: \"aiintune-s1\", Invocation: %q}]", snapshot, testCurrentInvocation)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive record snapshot")
	}
}

func TestTerminalLinkEndToEndRecordLifecycle(t *testing.T) {
	fakeBAT := startFakeTerminalBAT(t)
	dir := t.TempDir()
	store := &terminalRecordStore{}
	record, err := store.open(dir)
	if err != nil {
		t.Fatal(err)
	}

	hubResult := make(chan error, 1)

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentTerminalSubprotocol}})
		if err != nil {
			hubResult <- err
			return
		}
		defer conn.CloseNow()

		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamOpen, Session: "record-a", Cols: 80, Rows: 24,
		}); err != nil {
			hubResult <- err
			return
		}
		frame, err := readTerminalUpstream(conn)
		if err != nil || frame.Type != agentrelay.UpstreamReady || frame.Session != "record-a" {
			hubResult <- fmt.Errorf("record-a ready frame = %#v, %v", frame, err)
			return
		}

		entries := record.entries()
		if len(entries) != 1 || entries[0].ID != "aiintune-record-a" || entries[0].Invocation != testCurrentInvocation {
			hubResult <- fmt.Errorf("after record-a open, entries = %v", entries)
			return
		}

		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamClose, Session: "record-a",
		}); err != nil {
			hubResult <- err
			return
		}

		deadline := time.Now().Add(3 * time.Second)
		var removedA bool
		for time.Now().Before(deadline) {
			entries := record.entries()
			found := false
			for _, e := range entries {
				if e.ID == "aiintune-record-a" {
					found = true
					break
				}
			}
			if !found {
				removedA = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !removedA {
			hubResult <- fmt.Errorf("aiintune-record-a not removed after 3 seconds")
			return
		}

		if err := writeTerminalDownstream(conn, agentrelay.Downstream{
			Type: agentrelay.DownstreamOpen, Session: "record-b", Cols: 80, Rows: 24,
		}); err != nil {
			hubResult <- err
			return
		}
		frame, err = readTerminalUpstream(conn)
		if err != nil || frame.Type != agentrelay.UpstreamReady || frame.Session != "record-b" {
			hubResult <- fmt.Errorf("record-b ready frame = %#v, %v", frame, err)
			return
		}

		exitFrame := []byte(`{"channel":"pty:exit","contextId":"fake-profile-context","params":{"id":"aiintune-record-b","exitCode":0}}`)
		serverConn := <-fakeBAT.connections
		if err := serverConn.Write(context.Background(), websocket.MessageText, exitFrame); err != nil {
			hubResult <- err
			return
		}

		select {
		case fakeBAT.connections <- serverConn:
		default:
		}

		frame, err = readTerminalUpstream(conn)
		if err != nil || frame.Type != agentrelay.UpstreamExit || frame.Session != "record-b" {
			hubResult <- fmt.Errorf("record-b exit frame = %#v, %v", frame, err)
			return
		}

		deadline = time.Now().Add(3 * time.Second)
		var empty bool
		for time.Now().Before(deadline) {
			if len(record.entries()) == 0 {
				empty = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !empty {
			hubResult <- fmt.Errorf("record not empty after 3 seconds: %v", record.entries())
			return
		}

		hubResult <- conn.Close(websocket.StatusNormalClosure, "test complete")
	}))
	defer hub.Close()

	deps := fakeTerminalDeps(t, fakeBAT)
	deps.openRecord = func(string) (*terminalRecord, error) { return store.open(dir) }
	deps.invocationID = func(context.Context) (string, bool) { return testCurrentInvocation, true }
	deps.dialHub = func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		fakeBAT.note("hub:dial")
		return websocket.Dial(ctx, url, opts)
	}

	result := runTerminalLinkAttempt(context.Background(), config{
		HubURL: hub.URL, MachineID: "machine-test", AgentToken: "agent-test-token",
	}, deps)

	if err := <-hubResult; err != nil {
		t.Fatal(err)
	}

	if !result.linked {
		t.Fatalf("linked = false; want true")
	}
}

func TestTerminalLinkHeldRecord(t *testing.T) {
	deps := defaultTerminalLinkDeps()
	deps.openRecord = func(string) (*terminalRecord, error) {
		return nil, errTerminalRecordHeld
	}
	deps.loadEndpoint = func(string, string) (batremote.Endpoint, error) {
		t.Fatal("called loadEndpoint")
		return batremote.Endpoint{}, nil
	}
	deps.dialBAT = func(context.Context, batremote.Endpoint, string, string, batremote.ClientInfo) (*batremote.Session, error) {
		t.Fatal("called dialBAT")
		return nil, nil
	}
	deps.dialHub = func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		t.Fatal("called dialHub")
		return nil, nil, nil
	}

	result := runTerminalLinkAttempt(context.Background(), config{}, deps)
	if !reflect.DeepEqual(result, terminalAttemptResult{recordHeld: true}) {
		t.Fatalf("result = %+v; want {recordHeld: true}", result)
	}

	deps2 := defaultTerminalLinkDeps()
	deps2.openRecord = func(string) (*terminalRecord, error) {
		return nil, errors.New("x")
	}
	deps2.loadEndpoint = func(string, string) (batremote.Endpoint, error) {
		t.Fatal("called loadEndpoint")
		return batremote.Endpoint{}, nil
	}
	deps2.dialBAT = func(context.Context, batremote.Endpoint, string, string, batremote.ClientInfo) (*batremote.Session, error) {
		t.Fatal("called dialBAT")
		return nil, nil
	}
	deps2.dialHub = func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		t.Fatal("called dialHub")
		return nil, nil, nil
	}
	result2 := runTerminalLinkAttempt(context.Background(), config{}, deps2)
	if !reflect.DeepEqual(result2, terminalAttemptResult{}) {
		t.Fatalf("result2 = %+v; want {}", result2)
	}
}

func TestTerminalLinkHeldRecordLogsOncePerTransition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := []terminalAttemptResult{
		{recordHeld: true},
		{recordHeld: true},
		{recordHeld: true},
		{endpointAvailable: true, batAvailable: true},
		{recordHeld: true},
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

	wantSentence := "another clawctl-agent holds the terminal record; terminal link will retry"
	if got := strings.Count(logs.String(), wantSentence); got != 2 {
		t.Fatalf("log count = %d; want 2 transitions; logs = %q", got, logs.String())
	}
}

func TestTerminalLinkSourceGuardLogMessages(t *testing.T) {
	fset := token.NewFileSet()
	linkFile, err := parser.ParseFile(fset, "terminal_link.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	recordFile, err := parser.ParseFile(fset, "terminal_record.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	ast.Inspect(recordFile, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == "log" {
			t.Errorf("found log call in terminal_record.go: %v", fset.Position(call.Pos()))
		}
		return true
	})

	ast.Inspect(linkFile, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok {
			for i, lhs := range assign.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "logMessage" {
					rhs := assign.Rhs[i]
					if !isStringLiteralOrHubDialCategory(rhs) {
						t.Errorf("invalid assignment to logMessage: %v", fset.Position(rhs.Pos()))
					}
				}
			}
		}
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "logMessage" {
				if !isStringLiteralOrHubDialCategory(kv.Value) {
					t.Errorf("invalid field assignment to logMessage: %v", fset.Position(kv.Value.Pos()))
				}
			}
		}

		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "log" {
					if len(call.Args) == 1 {
						if argSel, ok := call.Args[0].(*ast.SelectorExpr); ok && argSel.Sel.Name == "logMessage" {
							return true
						}
					}
					for _, arg := range call.Args {
						if _, ok := arg.(*ast.BasicLit); !ok {
							t.Errorf("log call has non-string-literal argument: %v", fset.Position(call.Pos()))
						}
					}
				}
			}
		}

		return true
	})

	for _, decl := range linkFile.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "terminalHubDialCategory" {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if ret, ok := n.(*ast.ReturnStmt); ok {
					for _, res := range ret.Results {
						if _, ok := res.(*ast.BasicLit); !ok {
							t.Errorf("terminalHubDialCategory returns non-string-literal: %v", fset.Position(res.Pos()))
						}
					}
				}
				return true
			})
		}
	}
}

func isStringLiteralOrHubDialCategory(n ast.Node) bool {
	if _, ok := n.(*ast.BasicLit); ok {
		return true
	}
	if call, ok := n.(*ast.CallExpr); ok {
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "terminalHubDialCategory" {
			return true
		}
	}
	return false
}
