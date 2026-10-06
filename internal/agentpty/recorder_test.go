package agentpty

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/batremote"
)

type sharedLog struct {
	mu  sync.Mutex
	log []string
}

func (s *sharedLog) append(entry string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, entry)
}

func (s *sharedLog) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

type fakeRecorder struct {
	l       *sharedLog
	failAdd bool
}

func (r *fakeRecorder) Add(ptyID string) error {
	r.l.append("add:" + ptyID)
	if r.failAdd {
		return errors.New("fake recorder Add failed")
	}
	return nil
}

func (r *fakeRecorder) Remove(ptyID string) {
	r.l.append("remove:" + ptyID)
}

func newLoggingInvoker(t *testing.T, l *sharedLog, handler func(ctx context.Context, channel string, params json.RawMessage) (json.RawMessage, error)) *fakeInvoker {
	return &fakeInvoker{handler: func(ctx context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
		id := fieldString(t, objectFields(t, params), "id")
		l.append(channel + ":" + id)
		if handler != nil {
			return handler(ctx, channel, params)
		}
		return successfulResponse(channel, params)
	}}
}

func TestRecorderAddBeforeCreate(t *testing.T) {
	l := &sharedLog{}
	invoker := newLoggingInvoker(t, l, nil)
	mapper := New(invoker, "context-1", testHome, &fakeRecorder{l: l}, nil)
	openTerminal(t, mapper, "s1")
	want := []string{"add:aiintune-s1", "pty:create:aiintune-s1"}
	if got := l.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("log = %v; want %v", got, want)
	}
}

func TestRecorderFailedAdd(t *testing.T) {
	l := &sharedLog{}
	invoker := newLoggingInvoker(t, l, nil)
	frames := &frameCollector{}
	recorder := &fakeRecorder{l: l, failAdd: true}
	mapper := New(invoker, "context-1", testHome, recorder, frames.emit)

	err := mapper.HandleDownstream(context.Background(), openFrame("s1", ""))
	if !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("error = %v; want %v", err, ErrOpenFailed)
	}
	if len(invoker.snapshot()) != 0 {
		t.Fatalf("invoker calls = %d; want 0", len(invoker.snapshot()))
	}
	gotFrames := frames.snapshot()
	if len(gotFrames) != 1 || gotFrames[0].Type != agentrelay.UpstreamError || gotFrames[0].Session != "s1" || gotFrames[0].Reason != ReasonOpenFailed {
		t.Fatalf("frames = %+v", gotFrames)
	}

	recorder.failAdd = false
	openTerminal(t, mapper, "s1")
}

func TestRecorderClose(t *testing.T) {
	cases := []struct {
		name       string
		killResult func(ctx context.Context) (json.RawMessage, error)
		wantRemove bool
	}{
		{"true", func(ctx context.Context) (json.RawMessage, error) { return json.RawMessage(`true`), nil }, true},
		{"false", func(ctx context.Context) (json.RawMessage, error) { return json.RawMessage(`false`), nil }, false},
		{"error", func(ctx context.Context) (json.RawMessage, error) { return nil, errors.New("err") }, false},
		{"timeout", func(ctx context.Context) (json.RawMessage, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &sharedLog{}
			invoker := newLoggingInvoker(t, l, func(ctx context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
				if channel == "pty:kill" {
					return tc.killResult(ctx)
				}
				return successfulResponse(channel, params)
			})
			mapper := New(invoker, "context-1", testHome, &fakeRecorder{l: l}, nil)
			openTerminal(t, mapper, "s1")
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_ = mapper.HandleDownstream(ctx, agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: "s1"})

			log := l.snapshot()
			hasRemove := false
			if len(log) > 0 && log[len(log)-1] == "remove:aiintune-s1" {
				hasRemove = true
			}
			if hasRemove != tc.wantRemove {
				t.Fatalf("log ends with remove = %v; want %v (log: %v)", hasRemove, tc.wantRemove, log)
			}
		})
	}
}

func TestRecorderShutdown(t *testing.T) {
	l := &sharedLog{}
	invoker := newLoggingInvoker(t, l, func(ctx context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
		if channel == "pty:kill" {
			id := fieldString(t, objectFields(t, params), "id")
			switch id {
			case "aiintune-s-true":
				return json.RawMessage(`true`), nil
			case "aiintune-s-false":
				return json.RawMessage(`false`), nil
			case "aiintune-s-error":
				return nil, errors.New("err")
			}
		}
		return successfulResponse(channel, params)
	})
	mapper := New(invoker, "context-1", testHome, &fakeRecorder{l: l}, nil)
	openTerminal(t, mapper, "s-true")
	openTerminal(t, mapper, "s-false")
	openTerminal(t, mapper, "s-error")
	_ = mapper.Shutdown(context.Background())

	log := l.snapshot()
	var removes []string
	for _, e := range log {
		if len(e) > 7 && e[:7] == "remove:" {
			removes = append(removes, e)
		}
	}
	wantRemoves := []string{"remove:aiintune-s-true"}
	if !reflect.DeepEqual(removes, wantRemoves) {
		t.Fatalf("removes = %v; want %v (log: %v)", removes, wantRemoves, log)
	}
}

func TestRecorderPtyExit(t *testing.T) {
	setup := func() (*Mapper, *sharedLog) {
		l := &sharedLog{}
		invoker := newLoggingInvoker(t, l, nil)
		mapper := New(invoker, "context-1", testHome, &fakeRecorder{l: l}, nil)
		openTerminal(t, mapper, "s1")
		return mapper, l
	}

	t.Run("normal", func(t *testing.T) {
		mapper, l := setup()
		err := mapper.HandleEvent(batremote.Event{
			ContextID: "context-1",
			Channel:   "pty:exit",
			Params:    json.RawMessage(`{"id":"aiintune-s1","exitCode":0}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		log := l.snapshot()
		if log[len(log)-1] != "remove:aiintune-s1" {
			t.Fatalf("log ends with = %v; want remove:aiintune-s1", log[len(log)-1])
		}
	})

	t.Run("different context", func(t *testing.T) {
		mapper, l := setup()
		err := mapper.HandleEvent(batremote.Event{
			ContextID: "context-2",
			Channel:   "pty:exit",
			Params:    json.RawMessage(`{"id":"aiintune-s1","exitCode":0}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range l.snapshot() {
			if e == "remove:aiintune-s1" {
				t.Fatalf("unexpected remove in log: %v", l.snapshot())
			}
		}
	})

	t.Run("different id", func(t *testing.T) {
		mapper, l := setup()
		err := mapper.HandleEvent(batremote.Event{
			ContextID: "context-1",
			Channel:   "pty:exit",
			Params:    json.RawMessage(`{"id":"aiintune-other","exitCode":0}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range l.snapshot() {
			if e == "remove:aiintune-other" || e == "remove:aiintune-s1" {
				t.Fatalf("unexpected remove in log: %v", l.snapshot())
			}
		}
	})

	t.Run("missing exitCode", func(t *testing.T) {
		mapper, l := setup()
		err := mapper.HandleEvent(batremote.Event{
			ContextID: "context-1",
			Channel:   "pty:exit",
			Params:    json.RawMessage(`{"id":"aiintune-s1"}`),
		})
		if !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("error = %v; want %v", err, ErrInvalidEvent)
		}
		for _, e := range l.snapshot() {
			if e == "remove:aiintune-s1" {
				t.Fatalf("unexpected remove in log: %v", l.snapshot())
			}
		}
	})
}

func TestRecorderCreateDifferentID(t *testing.T) {
	cases := []struct {
		name       string
		killResult string
	}{
		{"true", `true`},
		{"false", `false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &sharedLog{}
			invoker := newLoggingInvoker(t, l, func(ctx context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
				if channel == "pty:create" {
					return json.RawMessage(`"other-id"`), nil
				}
				if channel == "pty:kill" {
					return json.RawMessage(tc.killResult), nil
				}
				return successfulResponse(channel, params)
			})
			mapper := New(invoker, "context-1", testHome, &fakeRecorder{l: l}, nil)
			err := mapper.HandleDownstream(context.Background(), openFrame("s1", ""))
			if !errors.Is(err, ErrCreateIDMismatch) {
				t.Fatalf("error = %v; want %v", err, ErrCreateIDMismatch)
			}

			log := l.snapshot()
			for _, e := range log {
				if e == "remove:aiintune-s1" {
					t.Fatalf("unexpected remove:aiintune-s1 in log: %v", log)
				}
			}

			want := []string{"add:aiintune-s1", "pty:create:aiintune-s1", "add:other-id", "pty:kill:other-id", "remove:other-id"}
			if tc.killResult == `false` {
				want = want[:len(want)-1]
			}
			if !reflect.DeepEqual(log, want) {
				t.Fatalf("log = %v; want %v", log, want)
			}
		})
	}
}

func TestRecorderCreateFails(t *testing.T) {
	cases := []struct {
		name       string
		killResult func() (json.RawMessage, error)
		wantRemove bool
	}{
		{"true", func() (json.RawMessage, error) { return json.RawMessage(`true`), nil }, true},
		{"false", func() (json.RawMessage, error) { return json.RawMessage(`false`), nil }, false},
		{"error", func() (json.RawMessage, error) { return nil, errors.New("err") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &sharedLog{}
			invoker := newLoggingInvoker(t, l, func(ctx context.Context, channel string, params json.RawMessage) (json.RawMessage, error) {
				if channel == "pty:create" {
					return nil, errors.New("err")
				}
				if channel == "pty:kill" {
					return tc.killResult()
				}
				return successfulResponse(channel, params)
			})
			mapper := New(invoker, "context-1", testHome, &fakeRecorder{l: l}, nil)
			_ = mapper.HandleDownstream(context.Background(), openFrame("s1", ""))
			log := l.snapshot()
			if len(log) < 2 {
				t.Fatalf("log too short: %v", log)
			}
			lastTwo := log[len(log)-2:]
			if tc.wantRemove {
				wantTwo := []string{"pty:kill:aiintune-s1", "remove:aiintune-s1"}
				if !reflect.DeepEqual(lastTwo, wantTwo) {
					t.Fatalf("log ends with = %v; want %v", lastTwo, wantTwo)
				}
			} else {
				if log[len(log)-1] == "remove:aiintune-s1" {
					t.Fatalf("unexpected remove in log: %v", log)
				}
			}
		})
	}
}
