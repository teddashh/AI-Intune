package batremote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	sessionReadLimit    = 8 << 20
	sessionWriteTimeout = 30 * time.Second
	eventBufferSize     = 256
	frameTypeInvoke     = "invoke"
	frameTypeResult     = "invoke-result"
	frameTypeError      = "invoke-error"
)

var (
	ErrClosed                     = errors.New("batremote: session closed")
	ErrEventOverflow              = errors.New("batremote: event buffer overflow")
	ErrInvalidFrame               = errors.New("batremote: invalid server frame")
	ErrInvalidResponse            = errors.New("batremote: invalid invocation response")
	ErrInvoke                     = errors.New("batremote: invocation failed")
	ErrInvokeRequest              = errors.New("batremote: invalid invocation request")
	ErrContextResponse            = errors.New("batremote: invalid profile context response")
	ErrContextUnavailable         = errors.New("batremote: profile context unavailable")
	ErrProfileContextUnsupported  = errors.New("batremote: profile context unsupported")
	ErrChannelNotAllowedInContext = errors.New("batremote: channel not allowed in profile context")
	ErrContextClosed              = errors.New("batremote: profile context closed")
)

// Event is an unmatched server frame. Params and Frame retain their original JSON.
type Event struct {
	Channel   string
	ContextID string
	Params    json.RawMessage
	Frame     json.RawMessage
}

// Context scopes invocations and events to a server profile context.
type Context struct {
	session *Session
	id      string

	mu     sync.Mutex
	closed bool
}

type invokeReply struct {
	result json.RawMessage
	err    error
}

type tokenMarker struct {
	length int
	digest [sha256.Size]byte
}

type sessionRuntime struct {
	writeMu sync.Mutex
	stateMu sync.Mutex

	pending map[string]chan invokeReply
	events  chan Event
	done    chan struct{}
	err     error

	endOnce       sync.Once
	closeOnce     sync.Once
	closeErr      error
	connectionID  int64
	invokeCounter atomic.Uint64
	token         tokenMarker
}

func newSessionRuntime(token string) *sessionRuntime {
	return &sessionRuntime{
		pending:      make(map[string]chan invokeReply),
		events:       make(chan Event, eventBufferSize),
		done:         make(chan struct{}),
		connectionID: time.Now().UnixNano(),
		token: tokenMarker{
			length: len(token),
			digest: sha256.Sum256([]byte(token)),
		},
	}
}

func (s *Session) nextInvokeID() string {
	sequence := s.runtime.invokeCounter.Add(1)
	return fmt.Sprintf("%d-%d", s.runtime.connectionID, sequence)
}

// Invoke sends a request and waits for the response with the same ID.
// Canceling ctx after the request is sent stops waiting, but the server may still execute the request.
func (s *Session) Invoke(ctx context.Context, channel string, params any) (json.RawMessage, error) {
	return s.invoke(ctx, channel, params, "")
}

func (s *Session) invoke(ctx context.Context, channel string, params any, contextID string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id := s.nextInvokeID()
	request := struct {
		ID        string `json:"id"`
		Channel   string `json:"channel"`
		Params    any    `json:"params"`
		Type      string `json:"type"`
		ContextID string `json:"contextId,omitempty"`
	}{ID: id, Channel: channel, Params: params, Type: frameTypeInvoke, ContextID: contextID}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, ErrInvokeRequest
	}

	replies := make(chan invokeReply, 1)
	runtime := s.runtime
	runtime.stateMu.Lock()
	if runtime.err != nil {
		err := runtime.err
		runtime.stateMu.Unlock()
		return nil, err
	}
	runtime.pending[id] = replies
	runtime.stateMu.Unlock()

	removePending := func() {
		runtime.stateMu.Lock()
		if runtime.pending[id] == replies {
			delete(runtime.pending, id)
		}
		runtime.stateMu.Unlock()
	}

	runtime.writeMu.Lock()
	if err := ctx.Err(); err != nil {
		runtime.writeMu.Unlock()
		removePending()
		return nil, err
	}
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), sessionWriteTimeout)
	err = s.conn.Write(writeCtx, websocket.MessageText, payload)
	cancelWrite()
	runtime.writeMu.Unlock()
	if err != nil {
		removePending()
		s.terminate(ErrConnection)
		return nil, s.Err()
	}

	select {
	case reply := <-replies:
		return reply.result, reply.err
	case <-ctx.Done():
		removePending()
		return nil, ctx.Err()
	case <-runtime.done:
		removePending()
		return nil, s.Err()
	}
}

// OpenProfileContext opens an isolated context for profile-scoped work.
func (s *Session) OpenProfileContext(ctx context.Context, profileID string) (*Context, error) {
	if !supportsProfileContext(s.capabilities) {
		return nil, ErrProfileContextUnsupported
	}
	result, err := s.Invoke(ctx, "profile:open", struct {
		ProfileID string `json:"profileId"`
	}{ProfileID: profileID})
	if err != nil {
		return nil, err
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(result, &response); err != nil || response == nil {
		return nil, ErrContextResponse
	}
	var contextID string
	if err := json.Unmarshal(response["contextId"], &contextID); err != nil || contextID == "" {
		return nil, ErrContextResponse
	}
	profileContext := &Context{session: s, id: contextID}
	if s.runtime.token.redact(contextID) != contextID {
		_ = profileContext.Close(ctx)
		return nil, ErrContextResponse
	}
	var status string
	_ = json.Unmarshal(response["status"], &status)
	if status != "ready" {
		_ = profileContext.Close(ctx)
		return nil, ErrContextUnavailable
	}
	return profileContext, nil
}

func supportsProfileContext(capabilities json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(capabilities, &fields) != nil || fields == nil {
		return false
	}
	encoded, ok := fields["profileContext"]
	if !ok {
		return false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return false
	}
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case json.Number:
		number, ok := new(big.Rat).SetString(value.String())
		return ok && number.Sign() != 0
	default:
		return true
	}
}

// ID returns the server-assigned profile context ID.
func (c *Context) ID() string { return c.id }

// Invoke sends an invocation in this profile context.
func (c *Context) Invoke(ctx context.Context, channel string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.session.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, ErrContextClosed
	}
	if strings.HasPrefix(channel, "profile:") || strings.HasPrefix(channel, "app:") {
		return nil, ErrChannelNotAllowedInContext
	}
	return c.session.invoke(ctx, channel, params, c.id)
}

// Close closes this profile context. Calls after the first are no-ops.
func (c *Context) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	_, err := c.session.Invoke(ctx, "profile:close", struct {
		ContextID string `json:"contextId"`
	}{ContextID: c.id})
	return err
}

// Events returns unmatched server frames. The channel closes with the session.
func (s *Session) Events() <-chan Event { return s.runtime.events }

// Err returns the first reason the session ended, or nil while it is alive.
func (s *Session) Err() error {
	s.runtime.stateMu.Lock()
	defer s.runtime.stateMu.Unlock()
	return s.runtime.err
}

// Ping performs a WebSocket protocol ping and waits for its pong.
func (s *Session) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime := s.runtime
	runtime.writeMu.Lock()
	defer runtime.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Err(); err != nil {
		return err
	}
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), sessionWriteTimeout)
	err := s.conn.Ping(writeCtx)
	cancelWrite()
	if err != nil {
		s.terminate(ErrConnection)
		return s.Err()
	}
	return nil
}

// Close releases the connection immediately, without waiting for the peer.
func (s *Session) Close() error {
	s.terminate(ErrClosed)
	if s.runtime.closeErr != nil {
		return ErrClose
	}
	return nil
}

func (s *Session) readLoop() {
	for {
		messageType, payload, err := s.conn.Read(context.Background())
		if err != nil {
			s.terminate(ErrConnection)
			return
		}
		if messageType != websocket.MessageText {
			s.terminate(ErrInvalidFrame)
			return
		}

		var object map[string]json.RawMessage
		if err := json.Unmarshal(payload, &object); err != nil || object == nil {
			s.terminate(ErrInvalidFrame)
			return
		}

		var id string
		if encodedID, ok := object["id"]; ok {
			_ = json.Unmarshal(encodedID, &id)
		}

		runtime := s.runtime
		runtime.stateMu.Lock()
		if replies, ok := runtime.pending[id]; ok && id != "" {
			delete(runtime.pending, id)
			runtime.stateMu.Unlock()
			replies <- s.decodeReply(object)
			continue
		}

		var channel string
		if encodedChannel, ok := object["channel"]; ok {
			_ = json.Unmarshal(encodedChannel, &channel)
		}
		var contextID string
		if encodedContextID, ok := object["contextId"]; ok {
			_ = json.Unmarshal(encodedContextID, &contextID)
		}
		event := Event{Channel: channel, ContextID: contextID, Params: object["params"], Frame: json.RawMessage(payload)}
		select {
		case runtime.events <- event:
			runtime.stateMu.Unlock()
		case <-runtime.done:
			runtime.stateMu.Unlock()
			return
		default:
			runtime.stateMu.Unlock()
			s.terminate(ErrEventOverflow)
			return
		}
	}
}

func (s *Session) decodeReply(object map[string]json.RawMessage) invokeReply {
	var frameType string
	if err := json.Unmarshal(object["type"], &frameType); err != nil {
		return invokeReply{err: ErrInvalidResponse}
	}
	result, hasResult := object["result"]
	encodedError, hasError := object["error"]
	switch frameType {
	case frameTypeResult:
		if !hasResult || hasError {
			return invokeReply{err: ErrInvalidResponse}
		}
		return invokeReply{result: append(json.RawMessage(nil), result...)}
	case frameTypeError:
		if !hasError || hasResult {
			return invokeReply{err: ErrInvalidResponse}
		}
	default:
		return invokeReply{err: ErrInvalidResponse}
	}
	var reason string
	if err := json.Unmarshal(encodedError, &reason); err != nil {
		return invokeReply{err: ErrInvalidResponse}
	}
	reason = s.runtime.token.redact(reason)
	if reason == "" {
		return invokeReply{err: ErrInvoke}
	}
	return invokeReply{err: &failure{message: ErrInvoke.Error() + ": " + reason, kind: ErrInvoke}}
}

func (m tokenMarker) redact(text string) string {
	if m.length == 0 || len(text) < m.length {
		return text
	}
	data := []byte(text)
	redacted := make([]byte, 0, len(data))
	start := 0
	for i := 0; i+m.length <= len(data); {
		if sha256.Sum256(data[i:i+m.length]) != m.digest {
			i++
			continue
		}
		redacted = append(redacted, data[start:i]...)
		redacted = append(redacted, "[redacted]"...)
		i += m.length
		start = i
	}
	if start == 0 {
		return text
	}
	redacted = append(redacted, data[start:]...)
	return string(redacted)
}

func (s *Session) terminate(cause error) {
	runtime := s.runtime
	runtime.endOnce.Do(func() {
		runtime.stateMu.Lock()
		runtime.err = cause
		clear(runtime.pending)
		close(runtime.done)
		close(runtime.events)
		runtime.stateMu.Unlock()

		runtime.closeOnce.Do(func() {
			runtime.closeErr = s.conn.CloseNow()
		})
	})
}
