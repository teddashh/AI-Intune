package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	terminalRecordVersion    = 1
	terminalRecordMaxEntries = 64
	terminalRecordMaxIDBytes = 256
)

var (
	errTerminalRecordHeld    = errors.New("terminal record is held by another process")
	errTerminalRecordFull    = errors.New("terminal record is full")
	errTerminalRecordInvalid = errors.New("terminal record entry is invalid")
)

// terminalRecord remembers the PTY ids this agent created, so a later link
// attempt can end shells that outlived the agent process. bat-server has no
// call that lists PTYs. The record holds ids and the bat-server unit's
// InvocationID, never terminal bytes.
type terminalRecord struct {
	path string

	mu   sync.Mutex
	ptys []terminalRecordEntry
}

type terminalRecordFile struct {
	Version int                   `json:"version"`
	PTYs    []terminalRecordEntry `json:"ptys"`
}

type terminalRecordEntry struct {
	ID         string `json:"id"`
	Invocation string `json:"invocation"`
}

// loadTerminalRecord reads the record at path. A missing file is an empty
// record. A file that cannot be read or parsed is moved aside to
// path+".unreadable" and the record starts empty, so one damaged file cannot
// keep the terminal away.
func loadTerminalRecord(path string) *terminalRecord {
	record := &terminalRecord{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return record
	}
	if err == nil {
		if ptys, ok := parseTerminalRecord(data); ok {
			record.ptys = ptys
			return record
		}
	}
	_ = os.Rename(path, path+".unreadable")
	return record
}

func parseTerminalRecord(data []byte) ([]terminalRecordEntry, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file terminalRecordFile
	if decoder.Decode(&file) != nil || file.Version != terminalRecordVersion || len(file.PTYs) > terminalRecordMaxEntries {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	seen := make(map[string]bool, len(file.PTYs))
	for _, entry := range file.PTYs {
		if !validTerminalRecordEntry(entry) || seen[entry.ID] {
			return nil, false
		}
		seen[entry.ID] = true
	}
	return file.PTYs, true
}

func validTerminalRecordEntry(entry terminalRecordEntry) bool {
	if entry.ID == "" || len(entry.ID) > terminalRecordMaxIDBytes || !utf8.ValidString(entry.ID) {
		return false
	}
	for _, r := range entry.ID {
		if unicode.IsControl(r) {
			return false
		}
	}
	return entry.Invocation == "" || validInvocationID(entry.Invocation)
}

// validInvocationID accepts systemd's InvocationID form: 32 lowercase hex digits.
func validInvocationID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Add records id before its shell exists. It fails, and leaves the record
// unchanged, when the entry is invalid, the record is full, or the file
// cannot be written. The caller then creates no shell.
func (r *terminalRecord) Add(id, invocation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.ContainsFunc(r.ptys, func(entry terminalRecordEntry) bool { return entry.ID == id }) {
		return nil
	}
	entry := terminalRecordEntry{ID: id, Invocation: invocation}
	if !validTerminalRecordEntry(entry) {
		return errTerminalRecordInvalid
	}
	if len(r.ptys) >= terminalRecordMaxEntries {
		return errTerminalRecordFull
	}
	next := append(slices.Clone(r.ptys), entry)
	if err := writeTerminalRecord(r.path, next); err != nil {
		return err
	}
	r.ptys = next
	return nil
}

// Remove forgets id once its shell is confirmed gone. When the file cannot be
// written the entry stays, and a later reap tries it again.
func (r *terminalRecord) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := slices.IndexFunc(r.ptys, func(entry terminalRecordEntry) bool { return entry.ID == id })
	if index < 0 {
		return nil
	}
	next := slices.Delete(slices.Clone(r.ptys), index, index+1)
	if err := writeTerminalRecord(r.path, next); err != nil {
		return err
	}
	r.ptys = next
	return nil
}

func (r *terminalRecord) entries() []terminalRecordEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ptys)
}

// writeTerminalRecord replaces the file atomically, so a reader sees the old
// record or the new one and never a partial write.
func writeTerminalRecord(path string, ptys []terminalRecordEntry) error {
	if ptys == nil {
		ptys = []terminalRecordEntry{}
	}
	data, err := json.Marshal(terminalRecordFile{Version: terminalRecordVersion, PTYs: ptys})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".terminal-ptys-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

// terminalRecordStore opens the record once per process, under a lock that
// lasts as long as the process. Holding the lock file here matters: once the
// *os.File is unreachable the garbage collector closes it, and closing it
// releases the lock. Every link attempt shares the one record, so the reap and
// a mapper still finishing an earlier link never write over each other.
type terminalRecordStore struct {
	mu     sync.Mutex
	lock   *os.File
	record *terminalRecord
}

// agentTerminalRecords is the record of this agent process.
var agentTerminalRecords terminalRecordStore

func terminalRecordDir(home string) string {
	return filepath.Join(home, ".local", "share", "clawctl")
}

// open returns the record kept in dir. The directory must already exist.
func (s *terminalRecordStore) open(dir string) (*terminalRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record != nil {
		return s.record, nil
	}
	lock, err := lockTerminalRecord(filepath.Join(dir, "terminal-ptys.lock"))
	if err != nil {
		return nil, err
	}
	s.lock = lock
	s.record = loadTerminalRecord(filepath.Join(dir, "terminal-ptys.json"))
	return s.record, nil
}
