package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
)

// SQLite primary result codes. modernc.org/sqlite reports them from Error.Code;
// the extended code lives in the high bits, so the primary code is Code()&0xff.
const (
	sqlitePrimaryBusy   = 5
	sqlitePrimaryLocked = 6

	defaultWriterWait = 15 * time.Second
	slowWriterLimit   = time.Second
)

// ErrWriterBusy is returned when this process cannot acquire the single writer
// connection before the wait budget expires. It is not a SQLite SQLITE_BUSY
// from another process; IsBusy treats both as hub contention.
var ErrWriterBusy = errors.New("store: writer queue timed out")

// IsBusy reports whether err is lock contention: a modernc SQLite primary code
// of SQLITE_BUSY or SQLITE_LOCKED, or a writer-queue timeout. Wrapped errors
// count.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrWriterBusy) {
		return true
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code() & 0xff
		return code == sqlitePrimaryBusy || code == sqlitePrimaryLocked
	}
	return false
}

// busyNoted marks an IsBusy error that the store has already added to
// clawctl_db_busy_total, so the HTTP edge does not count it again.
type busyNoted struct{ err error }

func (e busyNoted) Error() string { return e.err.Error() }
func (e busyNoted) Unwrap() error { return e.err }

// dbTx is the transaction surface shared by *sql.Tx (read-only snapshots,
// tests, and the separate rollback helpers) and *writeTx (the single writer).
type dbTx interface {
	Exec(query string, args ...any) (sql.Result, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	Prepare(query string) (*sql.Stmt, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	Commit() error
	Rollback() error
}

// writeTx is a writer transaction checked out from the single writer connection.
// Commit and Rollback return that connection to the pool. The context used to
// wait for the connection is not the transaction lifetime: a long reconcile or
// prune must not be rolled back just because the queue budget was 15s.
type writeTx struct {
	dbTx
	conn  *sql.Conn
	store *Store
	name  string
	wait  time.Duration
	began time.Time
	once  sync.Once
}

func (w *writeTx) Commit() error {
	err := w.dbTx.Commit()
	w.finish()
	return err
}

func (w *writeTx) Rollback() error {
	// A deferred Rollback after a successful Commit hits an already-finished
	// transaction. sql.Tx returns ErrTxDone and does not touch the connection;
	// finish is idempotent so the connection is closed exactly once.
	err := w.dbTx.Rollback()
	w.finish()
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

func (w *writeTx) finish() {
	w.once.Do(func() {
		hold := time.Since(w.began)
		w.store.observeWriteHold(w.name, hold)
		if w.wait > slowWriterLimit || hold > slowWriterLimit {
			log.Printf("WARN db writer slow name=%s wait=%s hold=%s", w.name, w.wait.Round(time.Millisecond), hold.Round(time.Millisecond))
		}
		if w.conn != nil {
			_ = w.conn.Close()
		}
	})
}

type writerGate struct {
	waitNS atomic.Int64
	stats  writeStats
}

// SetWriterWait overrides how long beginWrite waits for the single writer
// connection. Zero restores the 15s default. It does not limit how long the
// transaction may then be held.
func (s *Store) SetWriterWait(d time.Duration) {
	if s == nil {
		return
	}
	s.gate.waitNS.Store(int64(d))
}

func (s *Store) writerWait() time.Duration {
	v := s.gate.waitNS.Load()
	if v <= 0 {
		return defaultWriterWait
	}
	return time.Duration(v)
}

func (s *Store) beginWrite(ctx context.Context, name string) (*writeTx, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	acqCtx, cancel := context.WithTimeout(ctx, s.writerWait())
	start := time.Now()
	conn, err := s.db.Conn(acqCtx)
	waited := time.Since(start)
	cancel()
	s.observeWriteWait(name, waited)
	if waited > slowWriterLimit && err != nil {
		log.Printf("WARN db writer slow name=%s wait=%s hold=%s", name, waited.Round(time.Millisecond), time.Duration(0))
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, s.noteBusy(fmt.Errorf("store: writer queue timeout for %s: %w", name, ErrWriterBusy))
		}
		if IsBusy(err) {
			return nil, s.noteBusy(fmt.Errorf("store: acquire writer %s: %w", name, err))
		}
		return nil, fmt.Errorf("store: acquire writer %s: %w", name, err)
	}
	// Background, not acqCtx: cancelling the wait must not roll the transaction
	// back once the connection is in hand.
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		_ = conn.Close()
		if IsBusy(err) {
			return nil, s.noteBusy(fmt.Errorf("store: begin write %s: %w", name, err))
		}
		return nil, fmt.Errorf("store: begin write %s: %w", name, err)
	}
	return &writeTx{dbTx: tx, conn: conn, store: s, name: name, wait: waited, began: time.Now()}, nil
}

// boundExec adapts the single writer to helpers that take an Exec-only surface
// (audit rows that are not part of a caller's transaction).
type boundExec struct {
	s    *Store
	name string
}

func (b boundExec) Exec(query string, args ...any) (sql.Result, error) {
	return b.s.execWrite(context.Background(), b.name, query, args...)
}

// execWrite runs one autocommit statement on the single writer with the same
// bounded wait as beginWrite. The returned error is the statement error so
// callers keep their own wrapping text.
func (s *Store) execWrite(ctx context.Context, name, query string, args ...any) (sql.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.beginWrite(ctx, name)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(context.Background(), query, args...)
	if err != nil {
		_ = tx.Rollback()
		if IsBusy(err) {
			err = s.noteBusy(err)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		if IsBusy(err) {
			err = s.noteBusy(err)
		}
		return nil, err
	}
	return res, nil
}

func (s *Store) noteBusy(err error) error {
	if err == nil {
		return nil
	}
	s.gate.stats.busy.Add(1)
	return busyNoted{err: err}
}

// NoteBusy counts an IsBusy error that the store has not already counted.
// The HTTP edge calls it for statement and read contention. Writer-queue
// timeouts are counted inside beginWrite.
func (s *Store) NoteBusy(err error) {
	if s == nil || err == nil || !IsBusy(err) {
		return
	}
	var already busyNoted
	if errors.As(err, &already) {
		return
	}
	s.gate.stats.busy.Add(1)
}

// PingReader is the systemd watchdog's database probe. It uses the reader pool
// so a transaction holding the single writer cannot stall the liveness signal
// until the pool wait itself looks like a dead hub.
func (s *Store) PingReader(ctx context.Context) error {
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM machine_registry`).Scan(&n); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- histograms

// writeHistBounds are the Prometheus histogram buckets for writer wait and hold.
// The +Inf bucket is implicit.
var writeHistBounds = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type durationHist struct {
	mu     sync.Mutex
	bucket []uint64
	sum    float64
	count  uint64
}

func newDurationHist() *durationHist {
	return &durationHist{bucket: make([]uint64, len(writeHistBounds)+1)}
}

func (h *durationHist) observe(seconds float64) {
	h.mu.Lock()
	h.count++
	h.sum += seconds
	i := 0
	for i < len(writeHistBounds) && seconds > writeHistBounds[i] {
		i++
	}
	h.bucket[i]++
	h.mu.Unlock()
}

func (h *durationHist) snapshot() (bucket []uint64, sum float64, count uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	bucket = append([]uint64(nil), h.bucket...)
	return bucket, h.sum, h.count
}

type writeStats struct {
	mu   sync.Mutex
	wait map[string]*durationHist
	hold map[string]*durationHist
	busy atomic.Uint64
}

func (s *writeStats) init() {
	s.wait = make(map[string]*durationHist)
	s.hold = make(map[string]*durationHist)
}

func (s *writeStats) observe(which string, name string, d time.Duration) {
	s.mu.Lock()
	table := s.wait
	if which == "hold" {
		table = s.hold
	}
	if table == nil {
		if which == "hold" {
			s.hold = make(map[string]*durationHist)
			table = s.hold
		} else {
			s.wait = make(map[string]*durationHist)
			table = s.wait
		}
	}
	h := table[name]
	if h == nil {
		h = newDurationHist()
		table[name] = h
	}
	s.mu.Unlock()
	h.observe(d.Seconds())
}

func (s *Store) observeWriteWait(name string, d time.Duration) { s.gate.stats.observe("wait", name, d) }
func (s *Store) observeWriteHold(name string, d time.Duration) { s.gate.stats.observe("hold", name, d) }

// AppendDBMetrics writes the writer-pool histograms and the busy counter in
// hand-written Prometheus exposition format. Histogram families are omitted
// until the first observation so a TYPE line is never left without a sample.
func (s *Store) AppendDBMetrics(b *strings.Builder) {
	if s == nil {
		return
	}
	fmt.Fprintf(b, "# HELP clawctl_db_busy_total SQLite busy, locked, and writer-queue timeout errors observed by the hub.\n")
	fmt.Fprintf(b, "# TYPE clawctl_db_busy_total counter\n")
	fmt.Fprintf(b, "clawctl_db_busy_total %d\n", s.gate.stats.busy.Load())

	s.gate.stats.mu.Lock()
	wait := copyHists(s.gate.stats.wait)
	hold := copyHists(s.gate.stats.hold)
	s.gate.stats.mu.Unlock()

	writeHistFamily(b, "clawctl_db_write_wait_seconds", "Seconds spent waiting to acquire the single writer connection.", wait)
	writeHistFamily(b, "clawctl_db_write_hold_seconds", "Seconds a writer transaction was held, from begin until commit or rollback.", hold)
}

func copyHists(src map[string]*durationHist) map[string]*durationHist {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]*durationHist, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func writeHistFamily(b *strings.Builder, name, help string, series map[string]*durationHist) {
	if len(series) == 0 {
		return
	}
	names := make([]string, 0, len(series))
	for n := range series {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	// Emit every bucket, then every sum, then every count. A histogram is one
	// family, and each suffix must stay a contiguous block: interleaving
	// sum between two series' buckets makes a strict reader split the family.
	type histSnap struct {
		name   string
		bucket []uint64
		sum    float64
		count  uint64
	}
	snaps := make([]histSnap, 0, len(names))
	for _, seriesName := range names {
		bucket, sum, count := series[seriesName].snapshot()
		snaps = append(snaps, histSnap{seriesName, bucket, sum, count})
	}
	for _, snap := range snaps {
		var cum uint64
		for i := 0; i < len(snap.bucket); i++ {
			cum += snap.bucket[i]
			le := "+Inf"
			if i < len(writeHistBounds) {
				le = strconv.FormatFloat(writeHistBounds[i], 'f', -1, 64)
			}
			fmt.Fprintf(b, "%s_bucket{name=%s,le=%s} %d\n", name, promQuote(snap.name), promQuote(le), cum)
		}
	}
	for _, snap := range snaps {
		fmt.Fprintf(b, "%s_sum{name=%s} %s\n", name, promQuote(snap.name), strconv.FormatFloat(snap.sum, 'f', -1, 64))
	}
	for _, snap := range snaps {
		fmt.Fprintf(b, "%s_count{name=%s} %d\n", name, promQuote(snap.name), snap.count)
	}
}

func promQuote(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
