package batremote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	profileContextID     = "pc-0123456789abcdef0123456789abcdef"
	profileContextResult = `{"contextId":"pc-0123456789abcdef0123456789abcdef","profileId":"default","name":"Default","bindingKey":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","status":"ready"}`
	scopedEventFixture   = `{"type":"event","channel":"pty:output","contextId":"pc-0123456789abcdef0123456789abcdef","params":{"id":"pty-1","data":"hello\r\n"}}`
)

func newProfileContextPipeServer(t *testing.T, capabilities map[string]any, handle func(context.Context, *websocket.Conn) error) *pipeServer {
	t.Helper()
	return newPipeServer(t, func(ctx context.Context, conn *websocket.Conn, auth map[string]any) error {
		reply := successfulReply(auth)
		reply["capabilities"] = capabilities
		data, err := json.Marshal(reply)
		if err != nil {
			return err
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			return err
		}
		return handle(ctx, conn)
	})
}

func contextCapabilities(value any) map[string]any {
	return map[string]any{"profileContext": value}
}

func readChannel(request observedInvoke) (string, error) {
	var channel string
	if err := json.Unmarshal(request.fields["channel"], &channel); err != nil {
		return "", err
	}
	return channel, nil
}

func serveOpenContext(ctx context.Context, conn *websocket.Conn, result string) (observedInvoke, error) {
	request, err := readInvoke(ctx, conn)
	if err != nil {
		return observedInvoke{}, err
	}
	id, err := invokeID(request)
	if err != nil {
		return observedInvoke{}, err
	}
	return request, writeResult(ctx, conn, id, result)
}

func openTestContext(t *testing.T, p *pipeServer) (*Session, *Context) {
	t.Helper()
	session := dialInvokePipe(t, p)
	profileContext, err := session.OpenProfileContext(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	return session, profileContext
}

func noFrameReceived(ctx context.Context, conn *websocket.Conn) error {
	readCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, _, err := conn.Read(readCtx)
	if err == nil {
		return errors.New("unexpected client frame")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("waiting for absent client frame: %w", err)
	}
	return nil
}

func TestOpenProfileContextSendsNoContextID(t *testing.T) {
	requests := make(chan observedInvoke, 1)
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		request, err := serveOpenContext(ctx, conn, profileContextResult)
		requests <- request
		return err
	})
	_, profileContext := openTestContext(t, p)
	if profileContext.ID() == "" {
		t.Fatal("empty context ID")
	}
	request := <-requests
	if _, exists := request.fields["contextId"]; exists {
		t.Fatalf("profile:open contains contextId: %s", request.frame)
	}
	channel, err := readChannel(request)
	if err != nil || channel != "profile:open" {
		t.Fatalf("channel = %q, %v; want profile:open", channel, err)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(request.fields["params"], &params); err != nil {
		t.Fatal(err)
	}
	var profileID string
	if err := json.Unmarshal(params["profileId"], &profileID); err != nil || profileID != "default" {
		t.Fatalf("profileId = %q, %v; want default", profileID, err)
	}
}

func TestOpenProfileContextParsesResult(t *testing.T) {
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		_, err := serveOpenContext(ctx, conn, profileContextResult)
		return err
	})
	_, profileContext := openTestContext(t, p)
	if profileContext.ID() != profileContextID {
		t.Fatalf("ID() = %q; want %q", profileContext.ID(), profileContextID)
	}
}

func TestOpenProfileContextRejectsMissingContextID(t *testing.T) {
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		_, err := serveOpenContext(ctx, conn, `{"status":"ready"}`)
		return err
	})
	session := dialInvokePipe(t, p)
	profileContext, err := session.OpenProfileContext(context.Background(), "default")
	if profileContext != nil || !errors.Is(err, ErrContextResponse) {
		t.Fatalf("OpenProfileContext() = %#v, %v; want nil, %v", profileContext, err, ErrContextResponse)
	}
}

func TestOpenProfileContextClosesUnavailable(t *testing.T) {
	closes := make(chan observedInvoke, 1)
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		result := `{"contextId":"` + profileContextID + `","status":"unavailable"}`
		if _, err := serveOpenContext(ctx, conn, result); err != nil {
			return err
		}
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		closes <- request
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		return writeResult(ctx, conn, id, `{}`)
	})
	session := dialInvokePipe(t, p)
	profileContext, err := session.OpenProfileContext(context.Background(), "default")
	if profileContext != nil || !errors.Is(err, ErrContextUnavailable) {
		t.Fatalf("OpenProfileContext() = %#v, %v; want nil, %v", profileContext, err, ErrContextUnavailable)
	}
	request := <-closes
	channel, err := readChannel(request)
	if err != nil || channel != "profile:close" {
		t.Fatalf("close channel = %q, %v", channel, err)
	}
	var params struct {
		ContextID string `json:"contextId"`
	}
	if err := json.Unmarshal(request.fields["params"], &params); err != nil || params.ContextID != profileContextID {
		t.Fatalf("close params = %s, %v", request.fields["params"], err)
	}
}

func TestOpenProfileContextRequiresCapability(t *testing.T) {
	noFrame := make(chan error, 1)
	p := newProfileContextPipeServer(t, map[string]any{"terminal": true}, func(ctx context.Context, conn *websocket.Conn) error {
		err := noFrameReceived(ctx, conn)
		noFrame <- err
		return nil
	})
	session := dialInvokePipe(t, p)
	profileContext, err := session.OpenProfileContext(context.Background(), "default")
	if profileContext != nil || !errors.Is(err, ErrProfileContextUnsupported) {
		t.Fatalf("OpenProfileContext() = %#v, %v; want nil, %v", profileContext, err, ErrProfileContextUnsupported)
	}
	if err := <-noFrame; err != nil {
		t.Fatal(err)
	}
}

func TestProfileContextCapabilityAcceptsOtherVersions(t *testing.T) {
	t.Run("version 2", func(t *testing.T) {
		p := newProfileContextPipeServer(t, contextCapabilities(2), func(ctx context.Context, conn *websocket.Conn) error {
			_, err := serveOpenContext(ctx, conn, profileContextResult)
			return err
		})
		_, profileContext := openTestContext(t, p)
		if profileContext.ID() != profileContextID {
			t.Fatalf("ID() = %q; want %q", profileContext.ID(), profileContextID)
		}
	})

	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "false", value: false},
		{name: "zero", value: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			noFrame := make(chan error, 1)
			p := newProfileContextPipeServer(t, contextCapabilities(test.value), func(ctx context.Context, conn *websocket.Conn) error {
				err := noFrameReceived(ctx, conn)
				noFrame <- err
				return nil
			})
			session := dialInvokePipe(t, p)
			profileContext, err := session.OpenProfileContext(context.Background(), "default")
			if profileContext != nil || !errors.Is(err, ErrProfileContextUnsupported) {
				t.Fatalf("OpenProfileContext() = %#v, %v; want nil, %v", profileContext, err, ErrProfileContextUnsupported)
			}
			if err := <-noFrame; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContextInvokeCarriesContextIDAtTopLevel(t *testing.T) {
	invokes := make(chan observedInvoke, 1)
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		if _, err := serveOpenContext(ctx, conn, profileContextResult); err != nil {
			return err
		}
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		invokes <- request
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		return writeResult(ctx, conn, id, `{"ok":true}`)
	})
	_, profileContext := openTestContext(t, p)
	result, err := profileContext.Invoke(context.Background(), "pty:create", map[string]any{"id": "pty-1"})
	if err != nil || string(result) != `{"ok":true}` {
		t.Fatalf("Invoke() = %s, %v", result, err)
	}
	request := <-invokes
	channel, err := readChannel(request)
	if err != nil || channel != "pty:create" {
		t.Fatalf("channel = %q, %v; want pty:create", channel, err)
	}
	var contextID string
	if err := json.Unmarshal(request.fields["contextId"], &contextID); err != nil || contextID != profileContext.ID() {
		t.Fatalf("top-level contextId = %q, %v; want %q", contextID, err, profileContext.ID())
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(request.fields["params"], &params); err != nil {
		t.Fatal(err)
	}
	if _, exists := params["contextId"]; exists {
		t.Fatalf("params contains contextId: %s", request.fields["params"])
	}
}

func TestContextInvokeRejectsProfileAndAppChannels(t *testing.T) {
	noFrame := make(chan error, 1)
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		if _, err := serveOpenContext(ctx, conn, profileContextResult); err != nil {
			return err
		}
		err := noFrameReceived(ctx, conn)
		noFrame <- err
		return nil
	})
	_, profileContext := openTestContext(t, p)
	for _, channel := range []string{"profile:list", "profile:open", "app:quit"} {
		result, err := profileContext.Invoke(context.Background(), channel, nil)
		if result != nil || !errors.Is(err, ErrChannelNotAllowedInContext) {
			t.Errorf("Invoke(%q) = %s, %v; want nil, %v", channel, result, err, ErrChannelNotAllowedInContext)
		}
	}
	if err := <-noFrame; err != nil {
		t.Fatal(err)
	}
}

func TestContextCloseIsIdempotent(t *testing.T) {
	closes := make(chan observedInvoke, 1)
	noExtraFrame := make(chan error, 1)
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		if _, err := serveOpenContext(ctx, conn, profileContextResult); err != nil {
			return err
		}
		request, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		closes <- request
		id, err := invokeID(request)
		if err != nil {
			return err
		}
		if err := writeResult(ctx, conn, id, `{}`); err != nil {
			return err
		}
		err = noFrameReceived(ctx, conn)
		noExtraFrame <- err
		return nil
	})
	_, profileContext := openTestContext(t, p)
	for i := 0; i < 3; i++ {
		if err := profileContext.Close(context.Background()); err != nil {
			t.Fatalf("Close() call %d: %v", i+1, err)
		}
	}
	request := <-closes
	channel, err := readChannel(request)
	if err != nil || channel != "profile:close" {
		t.Fatalf("close channel = %q, %v", channel, err)
	}
	if _, exists := request.fields["contextId"]; exists {
		t.Fatalf("profile:close contains top-level contextId: %s", request.frame)
	}
	var params struct {
		ContextID string `json:"contextId"`
	}
	if err := json.Unmarshal(request.fields["params"], &params); err != nil || params.ContextID != profileContext.ID() {
		t.Fatalf("close params = %s, %v", request.fields["params"], err)
	}
	if err := <-noExtraFrame; err != nil {
		t.Fatal(err)
	}
}

func TestContextInvokeAfterCloseFails(t *testing.T) {
	noFrame := make(chan error, 1)
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		if _, err := serveOpenContext(ctx, conn, profileContextResult); err != nil {
			return err
		}
		closeRequest, err := readInvoke(ctx, conn)
		if err != nil {
			return err
		}
		id, err := invokeID(closeRequest)
		if err != nil {
			return err
		}
		if err := writeResult(ctx, conn, id, `{}`); err != nil {
			return err
		}
		err = noFrameReceived(ctx, conn)
		noFrame <- err
		return nil
	})
	_, profileContext := openTestContext(t, p)
	if err := profileContext.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := profileContext.Invoke(context.Background(), "pty:create", nil)
	if result != nil || !errors.Is(err, ErrContextClosed) {
		t.Fatalf("Invoke() = %s, %v; want nil, %v", result, err, ErrContextClosed)
	}
	if err := <-noFrame; err != nil {
		t.Fatal(err)
	}
}

func TestContextInvokeAfterSessionCloseFails(t *testing.T) {
	p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
		_, err := serveOpenContext(ctx, conn, profileContextResult)
		return err
	})
	session, profileContext := openTestContext(t, p)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := profileContext.Invoke(context.Background(), "pty:create", nil)
	if result != nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("Invoke() = %s, %v; want nil, %v", result, err, ErrClosed)
	}
}

func TestScopedEventExposesContextID(t *testing.T) {
	event := receiveFixtureEvent(t, scopedEventFixture)
	if event.ContextID != profileContextID || event.Channel != "pty:output" {
		t.Fatalf("event context/channel = %q/%q", event.ContextID, event.Channel)
	}
	var params struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(event.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.ID != "pty-1" || params.Data != "hello\r\n" {
		t.Fatalf("params = %#v", params)
	}
	if !bytes.Equal(event.Frame, []byte(scopedEventFixture)) {
		t.Fatalf("frame changed:\n got %q\nwant %q", event.Frame, scopedEventFixture)
	}
}

func TestBroadcastEventHasEmptyContextID(t *testing.T) {
	event := receiveFixtureEvent(t, ptyOutputFixture)
	if event.ContextID != "" {
		t.Fatalf("ContextID = %q; want empty", event.ContextID)
	}
	if event.Channel != "pty:output" || !bytes.Equal(event.Frame, []byte(ptyOutputFixture)) {
		t.Fatalf("broadcast event changed: %#v", event)
	}
	var params struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(event.Params, &params); err != nil || params.ID != "tt" || params.Data != "echo hi\r\n" {
		t.Fatalf("params = %#v, %v", params, err)
	}
}

func TestContextErrorsNeverContainToken(t *testing.T) {
	assertSafe := func(name string, err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s error = %v; want %v", name, err, want)
		}
		if err != nil && strings.Contains(err.Error(), sentinelToken) {
			t.Errorf("%s leaked token: %v", name, err)
		}
	}

	t.Run("unsupported", func(t *testing.T) {
		p := newProfileContextPipeServer(t, map[string]any{}, func(ctx context.Context, conn *websocket.Conn) error {
			return nil
		})
		session := dialInvokePipe(t, p)
		_, err := session.OpenProfileContext(context.Background(), sentinelToken)
		assertSafe("unsupported", err, ErrProfileContextUnsupported)
	})

	t.Run("invalid response", func(t *testing.T) {
		p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
			_, err := serveOpenContext(ctx, conn, `{"status":"ready","name":"`+sentinelToken+`"}`)
			return err
		})
		session := dialInvokePipe(t, p)
		_, err := session.OpenProfileContext(context.Background(), sentinelToken)
		assertSafe("invalid response", err, ErrContextResponse)
	})

	t.Run("unavailable", func(t *testing.T) {
		p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
			result := `{"contextId":"` + profileContextID + `","status":"unavailable","name":"` + sentinelToken + `"}`
			if _, err := serveOpenContext(ctx, conn, result); err != nil {
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
			return writeResult(ctx, conn, id, `{}`)
		})
		session := dialInvokePipe(t, p)
		_, err := session.OpenProfileContext(context.Background(), sentinelToken)
		assertSafe("unavailable", err, ErrContextUnavailable)
	})

	t.Run("context ID echoes token", func(t *testing.T) {
		p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
			result := `{"contextId":"pc-` + sentinelToken + `","status":"ready"}`
			if _, err := serveOpenContext(ctx, conn, result); err != nil {
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
			return writeResult(ctx, conn, id, `{}`)
		})
		session := dialInvokePipe(t, p)
		profileContext, err := session.OpenProfileContext(context.Background(), "default")
		if profileContext != nil {
			t.Fatalf("unsafe context returned: %#v", profileContext)
		}
		assertSafe("context ID echo", err, ErrContextResponse)
	})

	t.Run("invoke paths", func(t *testing.T) {
		p := newProfileContextPipeServer(t, contextCapabilities(1), func(ctx context.Context, conn *websocket.Conn) error {
			if _, err := serveOpenContext(ctx, conn, profileContextResult); err != nil {
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
			frame := fmt.Sprintf(`{"type":"invoke-error","id":%q,"error":%q}`, id, "rejected "+sentinelToken)
			return conn.Write(ctx, websocket.MessageText, []byte(frame))
		})
		session, profileContext := openTestContext(t, p)
		_, err := profileContext.Invoke(context.Background(), "app:quit", nil)
		assertSafe("forbidden channel", err, ErrChannelNotAllowedInContext)
		_, err = profileContext.Invoke(context.Background(), "pty:create", nil)
		assertSafe("server invoke error", err, ErrInvoke)
		profileContext.mu.Lock()
		profileContext.closed = true
		profileContext.mu.Unlock()
		_, err = profileContext.Invoke(context.Background(), "pty:create", nil)
		assertSafe("closed context", err, ErrContextClosed)
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = profileContext.Invoke(context.Background(), "pty:create", nil)
		assertSafe("closed session", err, ErrClosed)
	})
}
