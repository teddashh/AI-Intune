// Package batremote connects to BAT Server using a pinned TLS certificate.
package batremote

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	ProtocolV2       = "bat-remote/v2"
	ProtocolLegacyV1 = "bat-remote/legacy-v1"
	CompressionGzip  = "gzip"
	compressionNone  = "none"

	handshakeTimeout = 10 * time.Second
)

var (
	ErrEndpoint               = errors.New("batremote: missing or invalid endpoint field")
	ErrAuthRejected           = errors.New("batremote: authentication rejected")
	ErrAuthResult             = errors.New("batremote: invalid authentication response")
	ErrAuthResultID           = errors.New("batremote: authentication response ID mismatch")
	ErrUnsupportedProtocol    = errors.New("batremote: unsupported protocol")
	ErrUnsupportedCompression = errors.New("batremote: unsupported compression")
	ErrConnection             = errors.New("batremote: connection failed")
	ErrClose                  = errors.New("batremote: close failed")
	ErrSensitiveResponse      = errors.New("batremote: response contains credentials")
)

// Endpoint contains the four required connection parameters.
// Token is sent only in the authentication frame after certificate verification.
type Endpoint struct {
	Host        string
	Port        int
	Token       string
	Fingerprint string
}

type ClientInfo struct {
	AppName    string `json:"appName"`
	AppVersion string `json:"appVersion"`
	DeviceName string `json:"deviceName"`
	Label      string `json:"label"`
	Platform   string `json:"platform"`
	DeviceID   string `json:"deviceId,omitempty"`
}

// Session owns an authenticated connection and its negotiated metadata.
type Session struct {
	conn          *websocket.Conn
	protocol      string
	serverVersion string
	capabilities  json.RawMessage
	fingerprint   string
	runtime       *sessionRuntime
}

func (s *Session) Protocol() string      { return s.protocol }
func (s *Session) ServerVersion() string { return s.serverVersion }
func (s *Session) Fingerprint() string   { return s.fingerprint }
func (s *Session) Capabilities() json.RawMessage {
	return append(json.RawMessage(nil), s.capabilities...)
}

type failure struct {
	message string
	kind    error
}

func (e *failure) Error() string { return e.message }
func (e *failure) Unwrap() error { return e.kind }

// Dial completes TLS, WebSocket upgrade and authentication within ten seconds
// or the caller's earlier deadline. All failures close the connection.
func Dial(ctx context.Context, ep Endpoint, label, windowID string, info ClientInfo) (*Session, error) {
	dialer := &net.Dialer{}
	return dial(ctx, ep, label, windowID, info, dialer.DialContext, handshakeTimeout)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// Closing the underlying transport avoids waiting to write a TLS close alert
// to a peer that has stopped reading.
type transportConn struct {
	net.Conn
	raw net.Conn
}

func (c *transportConn) Close() error { return c.raw.Close() }

func dial(ctx context.Context, ep Endpoint, label, windowID string, info ClientInfo, connect dialContextFunc, timeout time.Duration) (_ *Session, err error) {
	// Only trusted error categories are retained, never transport errors that
	// can include a URL, peer-controlled text, or credentials.
	fail := func(kind error, detail string) error {
		message := kind.Error()
		if detail != "" {
			message += ": " + detail
		}
		if ep.Token != "" {
			message = strings.ReplaceAll(message, ep.Token, "")
		}
		return &failure{message: message, kind: kind}
	}
	if ep.Host == "" || ep.Port < 1 || ep.Port > 65535 || ep.Token == "" {
		return nil, fail(ErrEndpoint, "")
	}
	pin, err := NormalizeFingerprint(ep.Fingerprint)
	if err != nil {
		return nil, fail(ErrFingerprintFormat, "")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	transport.DialTLSContext = func(_ context.Context, network, address string) (net.Conn, error) {
		raw, err := connect(ctx, network, address)
		if err != nil {
			return nil, err
		}
		conn := tls.Client(raw, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: ep.Host,
			// The leaf DER pin replaces CA and hostname verification.
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 || certificateFingerprint(rawCerts[0]) != pin {
					// Close immediately so an unread TLS alert cannot delay rejection.
					raw.Close()
					return ErrFingerprintMismatch
				}
				return nil
			},
		})
		if err := conn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return &transportConn{Conn: conn, raw: raw}, nil
	}
	u := url.URL{Scheme: "wss", Host: net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)), Path: "/"}
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		CompressionMode: websocket.CompressionDisabled,
	})
	connectionFailure := func(cause error) error {
		if ctx.Err() != nil {
			return fail(ctx.Err(), "")
		}
		if errors.Is(cause, ErrFingerprintMismatch) {
			return fail(ErrFingerprintMismatch, "")
		}
		return fail(ErrConnection, "")
	}
	if err != nil {
		return nil, connectionFailure(err)
	}
	defer func() {
		if err != nil {
			conn.CloseNow()
		}
	}()
	id := fmt.Sprintf("%d-auth", time.Now().UnixMilli())
	protocols := []string{ProtocolV2, ProtocolLegacyV1}
	auth := struct {
		Type        string   `json:"type"`
		ID          string   `json:"id"`
		Token       string   `json:"token"`
		Protocols   []string `json:"protocols"`
		Compression []string `json:"compression"`
		Args        []any    `json:"args"`
	}{"auth", id, ep.Token, protocols, []string{}, []any{label, struct {
		WindowID   string     `json:"windowId"`
		ClientInfo ClientInfo `json:"clientInfo"`
	}{windowID, info}}}
	payload, err := json.Marshal(auth)
	if err != nil {
		return nil, fail(ErrAuthResult, "could not encode authentication")
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return nil, connectionFailure(err)
	}
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		return nil, connectionFailure(err)
	}
	var reply struct {
		Type          string          `json:"type"`
		ID            string          `json:"id"`
		Error         string          `json:"error"`
		Result        bool            `json:"result"`
		Protocol      string          `json:"protocol"`
		Compression   string          `json:"compression"`
		ServerVersion string          `json:"serverVersion"`
		Capabilities  json.RawMessage `json:"capabilities"`
	}
	if messageType != websocket.MessageText || json.Unmarshal(payload, &reply) != nil || reply.Type != "auth-result" {
		return nil, fail(ErrAuthResult, "")
	}
	if reply.ID != id {
		return nil, fail(ErrAuthResultID, "")
	}
	if reply.Error != "" {
		return nil, fail(ErrAuthRejected, reply.Error)
	}
	if !reply.Result {
		return nil, fail(ErrAuthResult, "result must be true")
	}
	if reply.Protocol != protocols[0] && reply.Protocol != protocols[1] {
		return nil, fail(ErrUnsupportedProtocol, reply.Protocol)
	}
	if reply.Compression != "" && reply.Compression != compressionNone {
		return nil, fail(ErrUnsupportedCompression, reply.Compression)
	}
	// Reject credential echoes in metadata rather than retaining the token in
	// the session or modifying the server's opaque capabilities.
	if strings.Contains(reply.Protocol, ep.Token) || strings.Contains(reply.ServerVersion, ep.Token) ||
		strings.Contains(pin, ep.Token) || capabilitiesContainToken(reply.Capabilities, ep.Token) {
		return nil, fail(ErrSensitiveResponse, "")
	}
	conn.SetReadLimit(sessionReadLimit)
	session := &Session{conn: conn, protocol: reply.Protocol, serverVersion: reply.ServerVersion,
		capabilities: reply.Capabilities, fingerprint: pin, runtime: newSessionRuntime(ep.Token)}
	go session.readLoop()
	return session, nil
}

func capabilitiesContainToken(raw json.RawMessage, token string) bool {
	if strings.Contains(string(raw), token) {
		return true
	}
	// Scan JSON strings only for escaped credential echoes. Numbers and the
	// capabilities schema remain opaque, and the original bytes are retained.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	for {
		item, err := decoder.Token()
		if err != nil {
			return false // The enclosing auth-result has already validated the JSON.
		}
		if value, ok := item.(string); ok && strings.Contains(value, token) {
			return true
		}
	}
}
