package batremote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	sentinelToken         = "SENTINEL-TOKEN-VALUE"
	authResultNoneFixture = `{"capabilities":{"profileContext":1,"remoteAuth":{"claude":"paste-code-v1","codex":"device-code-v1"}},"compression":"none","id":"1790137379916-auth","protocol":"bat-remote/v2","result":true,"serverVersion":"3.2.10","type":"auth-result"}`
)

var testInfo = ClientInfo{
	AppName: "clawctl-agent", AppVersion: "1.2.3", DeviceName: "workstation",
	Label: "client-info-label", Platform: "windows", DeviceID: "device-123",
}

type singleListener struct {
	conn      net.Conn
	once      sync.Once
	done      chan struct{}
	closeOnce sync.Once
}

func (l *singleListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = l.conn })
	if conn != nil {
		return conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *singleListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }

type countedConn struct {
	net.Conn
	bytes atomic.Int64
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.bytes.Add(int64(n))
	return n, err
}

type observedAuth struct {
	kind    websocket.MessageType
	data    []byte
	request *http.Request
}

type pipeServer struct {
	ep          Endpoint
	connect     dialContextFunc
	frames      atomic.Int64
	calls       atomic.Int64
	application *countedConn
	auth        chan observedAuth
	handshake   chan error
	peerClosed  chan error
	handlerDone chan struct{}
}

type replyFunc func(context.Context, *websocket.Conn, map[string]any) error

func testCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "BAT pipe test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	// Compute the expected pin independently of the production helper.
	digest := sha256.Sum256(der)
	hexValue := strings.ToUpper(hex.EncodeToString(digest[:]))
	parts := make([]string, len(digest))
	for i := range parts {
		parts[i] = hexValue[2*i : 2*i+2]
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, strings.Join(parts, ":")
}

func newPipeServer(t *testing.T, reply replyFunc) *pipeServer {
	t.Helper()
	cert, fingerprint := testCertificate(t)
	clientRaw, serverRaw := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	serverTLS := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	p := &pipeServer{
		ep:          Endpoint{Host: "127.0.0.1", Port: 9876, Token: sentinelToken, Fingerprint: fingerprint},
		application: &countedConn{Conn: serverTLS}, auth: make(chan observedAuth, 1),
		handshake: make(chan error, 1), peerClosed: make(chan error, 1), handlerDone: make(chan struct{}),
	}
	p.connect = func(ctx context.Context, network, address string) (net.Conn, error) {
		p.calls.Add(1)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if network != "tcp" || address != "127.0.0.1:9876" {
			return nil, errors.New("unexpected dial address")
		}
		return clientRaw, nil
	}
	listener := &singleListener{conn: p.application, done: make(chan struct{})}
	server := &http.Server{ErrorLog: log.New(io.Discard, "", 0)}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(p.handlerDone)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			p.peerClosed <- err
			return
		}
		defer conn.CloseNow()
		kind, data, err := conn.Read(ctx)
		if err != nil {
			p.peerClosed <- err
			return
		}
		p.frames.Add(1)
		p.auth <- observedAuth{kind, data, r}
		var auth map[string]any
		if err = json.Unmarshal(data, &auth); err != nil {
			p.peerClosed <- err
			return
		}
		if err = reply(ctx, conn, auth); err != nil {
			p.peerClosed <- err
			return
		}
		_, _, err = conn.Read(ctx)
		p.peerClosed <- err
	})
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		err := serverTLS.HandshakeContext(ctx)
		p.handshake <- err
		if err != nil {
			serverRaw.Close()
			return
		}
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		cancel()
		clientRaw.Close()
		serverRaw.Close()
		server.Close()
		listener.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("pipe server did not exit")
		}
		if p.frames.Load() > 0 {
			select {
			case <-p.handlerDone:
			case <-time.After(time.Second):
				t.Error("pipe handler did not exit")
			}
		}
	})
	return p
}

func successfulReply(auth map[string]any) map[string]any {
	return map[string]any{
		"type": "auth-result", "id": auth["id"], "result": true,
		"protocol": ProtocolV2, "compression": "", "serverVersion": "3.2.10",
		"capabilities": map[string]any{"terminal": true, "unknown": []any{1, "preserved"}},
	}
}

func sendReply(edit func(map[string]any)) replyFunc {
	return func(ctx context.Context, conn *websocket.Conn, auth map[string]any) error {
		reply := successfulReply(auth)
		if edit != nil {
			edit(reply)
		}
		data, err := json.Marshal(reply)
		if err != nil {
			return err
		}
		return conn.Write(ctx, websocket.MessageText, data)
	}
}

func dialPipe(ctx context.Context, p *pipeServer) (*Session, error) {
	return dial(ctx, p.ep, "session-label", "window-123", testInfo, p.connect, time.Second)
}

func assertFailure(t *testing.T, s *Session, err, want error) {
	t.Helper()
	if s != nil {
		s.Close()
		t.Fatal("failure returned a session")
	}
	if !errors.Is(err, want) {
		t.Fatalf("got %v; want %v", err, want)
	}
	if strings.Contains(err.Error(), sentinelToken) {
		t.Fatal("error leaked token")
	}
}

func assertPeerClosed(t *testing.T, p *pipeServer) {
	t.Helper()
	select {
	case err := <-p.peerClosed:
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("peer did not observe client closing: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not close connection")
	}
}

func TestDialRejectsFingerprintMismatch(t *testing.T) {
	p := newPipeServer(t, sendReply(nil))
	p.ep.Fingerprint = strings.Repeat("00", 32)
	s, err := dialPipe(context.Background(), p)
	assertFailure(t, s, err, ErrFingerprintMismatch)
	select {
	case err := <-p.handshake:
		if err == nil {
			t.Fatal("server TLS handshake unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server TLS handshake did not exit")
	}
	if frames := p.frames.Load(); frames != 0 {
		t.Fatalf("server received %d frames before pin verification", frames)
	}
	if bytes := p.application.bytes.Load(); bytes != 0 {
		t.Fatalf("server received %d application bytes before pin verification", bytes)
	}
	select {
	case <-p.auth:
		t.Fatal("server received an auth frame")
	default:
	}
}

func TestDialRejectsEmptyFingerprint(t *testing.T) {
	p := newPipeServer(t, sendReply(nil))
	p.ep.Fingerprint = ""
	s, err := dialPipe(context.Background(), p)
	assertFailure(t, s, err, ErrFingerprintFormat)
	if p.calls.Load() != 0 {
		t.Fatal("empty fingerprint reached dialer")
	}
}

func TestDialSendsExactAuthFrame(t *testing.T) {
	p := newPipeServer(t, sendReply(nil))
	p.ep.Token = " " + sentinelToken + "\n"
	start := time.Now().UnixMilli()
	s, err := dialPipe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := <-p.auth
	if got.kind != websocket.MessageText {
		t.Fatalf("first frame type = %v", got.kind)
	}
	if got.request.URL.RequestURI() != "/" {
		t.Fatalf("request URI = %q", got.request.URL.RequestURI())
	}
	for _, header := range []string{"Authorization", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions"} {
		if got.request.Header.Get(header) != "" {
			t.Errorf("unexpected %s header", header)
		}
	}
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(got.data, &auth); err != nil {
		t.Fatal(err)
	}
	if len(auth) != 6 {
		t.Fatalf("auth has %d fields", len(auth))
	}
	var id string
	if err := json.Unmarshal(auth["id"], &id); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(id, "-auth") {
		t.Fatalf("invalid auth ID %q", id)
	}
	millis, err := strconv.ParseInt(strings.TrimSuffix(id, "-auth"), 10, 64)
	if err != nil || millis < start || millis > time.Now().UnixMilli() {
		t.Fatalf("ID is not current unix milliseconds: %q", id)
	}
	checks := map[string]any{
		"type": "auth", "token": p.ep.Token,
		"protocols": []string{ProtocolV2, ProtocolLegacyV1}, "compression": []string{},
		"args": []any{"session-label", map[string]any{"windowId": "window-123", "clientInfo": testInfo}},
	}
	for field, want := range checks {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(auth[field]) != string(encoded) && field != "args" {
			t.Errorf("incorrect %s field", field)
		}
		var actualValue, expectedValue any
		if err := json.Unmarshal(auth[field], &actualValue); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &expectedValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actualValue, expectedValue) {
			t.Errorf("incorrect %s contents", field)
		}
	}
}

func TestDialAcceptsAuthResult(t *testing.T) {
	for _, protocol := range []string{ProtocolV2, ProtocolLegacyV1} {
		t.Run(protocol, func(t *testing.T) {
			const rawCaps = `{ "nested" : [true, 12345678901234567890, 1e400, {"future":"value"}] }`
			p := newPipeServer(t, func(ctx context.Context, conn *websocket.Conn, auth map[string]any) error {
				data := fmt.Sprintf(`{"type":"auth-result","id":%q,"result":true,"protocol":%q,"compression":"","serverVersion":"3.2.10","capabilities":%s}`, auth["id"], protocol, rawCaps)
				if err := conn.Write(ctx, websocket.MessageText, []byte(data)); err != nil {
					return err
				}
				return conn.Write(ctx, websocket.MessageText, []byte(`{"channel":"ready"}`))
			})
			wantPin := p.ep.Fingerprint
			p.ep.Fingerprint = strings.ToLower(strings.ReplaceAll(wantPin, ":", " \n"))
			ctx, cancel := context.WithCancel(context.Background())
			s, err := dialPipe(ctx, p)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.Protocol() != protocol || s.ServerVersion() != "3.2.10" || s.Fingerprint() != wantPin {
				t.Fatal("incorrect session metadata")
			}
			if string(s.Capabilities()) != rawCaps {
				t.Fatalf("capabilities changed: %s", s.Capabilities())
			}
			copy := s.Capabilities()
			copy[0] = '!'
			if string(s.Capabilities()) != rawCaps {
				t.Fatal("caller mutated session capabilities")
			}
			// Canceling the handshake context must not close an established session.
			select {
			case event := <-s.Events():
				if string(event.Frame) != `{"channel":"ready"}` {
					t.Fatalf("session event changed: %s", event.Frame)
				}
			case <-time.After(time.Second):
				t.Fatal("session unusable after Dial")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			assertPeerClosed(t, p)
		})
	}
}

func TestDialAcceptsCompressionNone(t *testing.T) {
	p := newPipeServer(t, func(ctx context.Context, conn *websocket.Conn, auth map[string]any) error {
		id, ok := auth["id"].(string)
		if !ok {
			return errors.New("auth request has no string ID")
		}
		frame := strings.Replace(authResultNoneFixture, `"1790137379916-auth"`, fmt.Sprintf("%q", id), 1)
		return conn.Write(ctx, websocket.MessageText, []byte(frame))
	})
	session, err := dialPipe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.Protocol() != ProtocolV2 {
		t.Fatalf("protocol = %q; want %q", session.Protocol(), ProtocolV2)
	}
	if session.ServerVersion() != "3.2.10" {
		t.Fatalf("server version = %q; want 3.2.10", session.ServerVersion())
	}
	wantCapabilities := `{"profileContext":1,"remoteAuth":{"claude":"paste-code-v1","codex":"device-code-v1"}}`
	if string(session.Capabilities()) != wantCapabilities {
		t.Fatalf("capabilities = %s; want %s", session.Capabilities(), wantCapabilities)
	}
}

func TestDialStillRejectsGzipCompression(t *testing.T) {
	p := newPipeServer(t, func(ctx context.Context, conn *websocket.Conn, auth map[string]any) error {
		id, ok := auth["id"].(string)
		if !ok {
			return errors.New("auth request has no string ID")
		}
		frame := strings.Replace(authResultNoneFixture, `"1790137379916-auth"`, fmt.Sprintf("%q", id), 1)
		frame = strings.Replace(frame, `"compression":"none"`, `"compression":"gzip"`, 1)
		return conn.Write(ctx, websocket.MessageText, []byte(frame))
	})
	session, err := dialPipe(context.Background(), p)
	assertFailure(t, session, err, ErrUnsupportedCompression)
	assertPeerClosed(t, p)
}

type rejectionCase struct {
	name string
	edit func(map[string]any)
	want error
}

func rejectionCases() []rejectionCase {
	return []rejectionCase{
		{"invalid token", func(r map[string]any) { delete(r, "result"); r["error"] = "Invalid token" }, ErrAuthRejected},
		{"server error echoes token", func(r map[string]any) { r["error"] = "Rejected " + sentinelToken }, ErrAuthRejected},
		{"server protocol rejection", func(r map[string]any) { r["error"] = "Unsupported remote protocol" }, ErrAuthRejected},
		{"wrong ID", func(r map[string]any) { r["id"] = sentinelToken }, ErrAuthResultID},
		{"missing ID", func(r map[string]any) { delete(r, "id") }, ErrAuthResultID},
		{"unknown protocol", func(r map[string]any) { r["protocol"] = sentinelToken }, ErrUnsupportedProtocol},
		{"gzip", func(r map[string]any) { r["compression"] = CompressionGzip }, ErrUnsupportedCompression},
		{"unknown compression", func(r map[string]any) { r["compression"] = sentinelToken }, ErrUnsupportedCompression},
		{"wrong type", func(r map[string]any) { r["type"] = sentinelToken }, ErrAuthResult},
		{"false result", func(r map[string]any) { r["result"] = false }, ErrAuthResult},
		{"missing result", func(r map[string]any) { delete(r, "result") }, ErrAuthResult},
		{"invalid result type", func(r map[string]any) { r["result"] = sentinelToken }, ErrAuthResult},
		{"version echoes token", func(r map[string]any) { r["serverVersion"] = sentinelToken }, ErrSensitiveResponse},
		{"capabilities echo token", func(r map[string]any) { r["capabilities"] = map[string]any{sentinelToken: []any{sentinelToken}} }, ErrSensitiveResponse},
	}
}

func runRejection(t *testing.T, test rejectionCase) {
	t.Helper()
	p := newPipeServer(t, sendReply(test.edit))
	s, err := dialPipe(context.Background(), p)
	assertFailure(t, s, err, test.want)
	if test.name == "invalid token" && !strings.Contains(err.Error(), "Invalid token") {
		t.Fatal("server rejection text was lost")
	}
	assertPeerClosed(t, p)
}

func TestDialRejectsInvalidToken(t *testing.T)           { runRejection(t, rejectionCases()[0]) }
func TestDialRejectsMismatchedAuthResultID(t *testing.T) { runRejection(t, rejectionCases()[3]) }
func TestDialRejectsUnknownProtocol(t *testing.T)        { runRejection(t, rejectionCases()[5]) }
func TestDialRejectsUnsupportedCompression(t *testing.T) { runRejection(t, rejectionCases()[6]) }

func TestDialNeverLeaksTokenInErrors(t *testing.T) {
	t.Run("fingerprint mismatch", TestDialRejectsFingerprintMismatch)
	t.Run("empty fingerprint", TestDialRejectsEmptyFingerprint)
	t.Run("endpoint validation", TestDialRejectsInvalidEndpoint)
	t.Run("transport errors", TestDialTransportErrors)
	t.Run("context and timeout", TestDialContextAndTimeout)
	t.Run("malformed frames", TestDialRejectsMalformedFrames)
	for _, test := range rejectionCases() {
		t.Run(test.name, func(t *testing.T) { runRejection(t, test) })
	}
}

func TestDialRejectsInvalidEndpoint(t *testing.T) {
	for _, field := range []string{"host", "port zero", "port negative", "port large", "token", "pin malformed"} {
		t.Run(field, func(t *testing.T) {
			ep := Endpoint{Host: "localhost", Port: 9876, Token: sentinelToken, Fingerprint: strings.Repeat("AB", 32)}
			want := ErrEndpoint
			switch field {
			case "host":
				ep.Host = ""
			case "port zero":
				ep.Port = 0
			case "port negative":
				ep.Port = -1
			case "port large":
				ep.Port = 65536
			case "token":
				ep.Token = ""
			case "pin malformed":
				ep.Fingerprint = sentinelToken
				want = ErrFingerprintFormat
			}
			connect := func(context.Context, string, string) (net.Conn, error) {
				t.Error("invalid endpoint reached dialer")
				return nil, errors.New("unexpected dial")
			}
			s, err := dial(context.Background(), ep, "", "", ClientInfo{}, connect, time.Second)
			assertFailure(t, s, err, want)
		})
	}
}

func TestDialTransportErrors(t *testing.T) {
	ep := Endpoint{Host: "localhost", Port: 9876, Token: sentinelToken, Fingerprint: strings.Repeat("AB", 32)}
	connect := func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial " + sentinelToken)
	}
	s, err := dial(context.Background(), ep, "", "", ClientInfo{}, connect, time.Second)
	assertFailure(t, s, err, ErrConnection)
	// The public entry point also validates before any network activity.
	ep.Host = ""
	s, err = Dial(context.Background(), ep, "", "", ClientInfo{})
	assertFailure(t, s, err, ErrEndpoint)
}

func TestDialRejectsMalformedFrames(t *testing.T) {
	for _, mode := range []string{"binary", "malformed JSON", "close echoes token", "escaped credential"} {
		t.Run(mode, func(t *testing.T) {
			want := ErrAuthResult
			p := newPipeServer(t, func(ctx context.Context, conn *websocket.Conn, auth map[string]any) error {
				switch mode {
				case "binary":
					data, _ := json.Marshal(successfulReply(auth))
					return conn.Write(ctx, websocket.MessageBinary, data)
				case "close echoes token":
					return conn.Close(websocket.StatusPolicyViolation, sentinelToken)
				case "escaped credential":
					data := fmt.Sprintf(`{"type":"auth-result","id":%q,"result":true,"protocol":%q,"capabilities":{"value":"\u0053ENTINEL-TOKEN-VALUE"}}`, auth["id"], ProtocolV2)
					return conn.Write(ctx, websocket.MessageText, []byte(data))
				default:
					return conn.Write(ctx, websocket.MessageText, []byte(`{"`+sentinelToken))
				}
			})
			if mode == "close echoes token" {
				want = ErrConnection
			}
			if mode == "escaped credential" {
				want = ErrSensitiveResponse
			}
			s, err := dialPipe(context.Background(), p)
			assertFailure(t, s, err, want)
			assertPeerClosed(t, p)
		})
	}
}

func TestDialContextAndTimeout(t *testing.T) {
	for _, stage := range []string{"dial", "TLS", "auth"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%t", stage, canceled), func(t *testing.T) {
				p := newPipeServer(t, func(context.Context, *websocket.Conn, map[string]any) error { return nil })
				started := make(chan struct{})
				if stage == "dial" {
					p.connect = func(ctx context.Context, _, _ string) (net.Conn, error) {
						close(started)
						<-ctx.Done()
						return nil, ctx.Err()
					}
				} else if stage == "TLS" {
					client, server := net.Pipe()
					t.Cleanup(func() { client.Close(); server.Close() })
					p.connect = func(context.Context, string, string) (net.Conn, error) { close(started); return client, nil }
				} else {
					go func() {
						select {
						case <-p.auth:
							close(started)
						case <-time.After(time.Second):
						}
					}()
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if canceled {
					go func() {
						select {
						case <-started:
							cancel()
						case <-ctx.Done():
						}
					}()
				}
				begin := time.Now()
				s, err := dial(ctx, p.ep, "", "", testInfo, p.connect, 100*time.Millisecond)
				want := context.DeadlineExceeded
				if canceled {
					want = context.Canceled
				}
				assertFailure(t, s, err, want)
				if time.Since(begin) > time.Second {
					t.Fatal("handshake deadline not respected")
				}
				if stage == "auth" {
					assertPeerClosed(t, p)
				}
			})
		}
	}
}

func TestClientInfoOmitsEmptyDeviceID(t *testing.T) {
	data, err := json.Marshal(ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 {
		t.Fatalf("expected five required fields, got %s", data)
	}
	if _, ok := fields["deviceId"]; ok {
		t.Fatal("empty deviceId must be omitted")
	}
}
