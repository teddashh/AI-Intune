package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Two well-formed bat-server unit InvocationIDs.
var (
	testCurrentInvocation = strings.Repeat("a", 32)
	testOtherInvocation   = strings.Repeat("b", 32)
)

func TestTerminalRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terminal-ptys.json")

	record := loadTerminalRecord(path)
	if len(record.entries()) != 0 {
		t.Fatalf("entries = %v; want empty", record.entries())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not exist, got err: %v", err)
	}

	if err := record.Add("a", ""); err != nil {
		t.Fatal(err)
	}
	if err := record.Add("b", testCurrentInvocation); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := []byte(fmt.Sprintf(`{"version":1,"ptys":[{"id":"a","invocation":""},{"id":"b","invocation":"%s"}]}`, testCurrentInvocation))
	if !bytes.Equal(data, wantBytes) {
		t.Fatalf("bytes = %s; want %s", data, wantBytes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v; want 0600", info.Mode().Perm())
	}

	record2 := loadTerminalRecord(path)
	if !reflect.DeepEqual(record2.entries(), record.entries()) {
		t.Fatalf("reloaded entries = %v; want %v", record2.entries(), record.entries())
	}

	if err := record.Add("b", testOtherInvocation); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data2, wantBytes) {
		t.Fatalf("bytes changed to %s", data2)
	}

	if err := record.Remove("a"); err != nil {
		t.Fatal(err)
	}
	data3, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes3 := []byte(fmt.Sprintf(`{"version":1,"ptys":[{"id":"b","invocation":"%s"}]}`, testCurrentInvocation))
	if !bytes.Equal(data3, wantBytes3) {
		t.Fatalf("bytes = %s; want %s", data3, wantBytes3)
	}

	if err := record.Remove("absent"); err != nil {
		t.Fatal(err)
	}
	data4, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data4, wantBytes3) {
		t.Fatalf("bytes changed to %s", data4)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "terminal-ptys.json" {
		t.Fatalf("dir contents = %v; want only terminal-ptys.json", entries)
	}
}

func TestTerminalRecordReplaceNotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terminal-ptys.json")
	record := loadTerminalRecord(path)
	if err := record.Add("a", testCurrentInvocation); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Add("b", testCurrentInvocation); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(info1, info2) {
		t.Fatalf("file was overwritten in place, not replaced")
	}
}

func TestTerminalRecordStrictParse(t *testing.T) {
	valid65 := `{"version":1,"ptys":[`
	for i := 0; i < 65; i++ {
		valid65 += fmt.Sprintf(`{"id":"id%d","invocation":"%s"}`, i, testCurrentInvocation)
		if i < 64 {
			valid65 += `,`
		}
	}
	valid65 += `]}`

	cases := []struct {
		name  string
		bytes string
	}{
		{"unknown top-level field", `{"version":1,"unknown":1,"ptys":[]}`},
		{"unknown field inside an entry", `{"version":1,"ptys":[{"id":"a","unknown":1,"invocation":""}]}`},
		{"version 2", `{"version":2,"ptys":[]}`},
		{"no version", `{"ptys":[]}`},
		{"65 valid entries", valid65},
		{"duplicate id", `{"version":1,"ptys":[{"id":"a","invocation":""},{"id":"a","invocation":""}]}`},
		{"invocation in uppercase hex", fmt.Sprintf(`{"version":1,"ptys":[{"id":"a","invocation":"%s"}]}`, strings.ToUpper(testCurrentInvocation))},
		{"invocation of 31 characters", `{"version":1,"ptys":[{"id":"a","invocation":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`},
		{"empty id", `{"version":1,"ptys":[{"id":"","invocation":""}]}`},
		{"id of 257 bytes", fmt.Sprintf(`{"version":1,"ptys":[{"id":"%s","invocation":""}]}`, strings.Repeat("x", 257))},
		{"id containing \\u0001", `{"version":1,"ptys":[{"id":"a\u0001b","invocation":""}]}`},
		{"id containing \\u007f", `{"version":1,"ptys":[{"id":"a\u007fb","invocation":""}]}`},
		{"a second JSON value after the first", `{"version":1,"ptys":[]} {}`},
		{"the letter x after the first value", `{"version":1,"ptys":[]} x`},
		{"not json", `not json`},
		{"empty file", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "terminal-ptys.json")
			if err := os.WriteFile(path, []byte(tc.bytes), 0600); err != nil {
				t.Fatal(err)
			}
			record := loadTerminalRecord(path)
			if len(record.entries()) != 0 {
				t.Fatalf("entries = %v; want empty", record.entries())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("file %s still exists", path)
			}
			unreadable := path + ".unreadable"
			data, err := os.ReadFile(unreadable)
			if err != nil {
				t.Fatalf("read unreadable file: %v", err)
			}
			if string(data) != tc.bytes {
				t.Fatalf("unreadable file contents = %q; want %q", data, tc.bytes)
			}
		})
	}
}

func TestTerminalRecordAccepted(t *testing.T) {
	valid64 := `{"version":1,"ptys":[`
	for i := 0; i < 64; i++ {
		valid64 += fmt.Sprintf(`{"id":"id%d","invocation":"%s"}`, i, testCurrentInvocation)
		if i < 63 {
			valid64 += `,`
		}
	}
	valid64 += `]}`

	cases := []struct {
		name  string
		bytes string
		want  int
	}{
		{"64 valid entries", valid64, 64},
		{"a valid record followed by one newline", `{"version":1,"ptys":[]}` + "\n", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "terminal-ptys.json")
			if err := os.WriteFile(path, []byte(tc.bytes), 0600); err != nil {
				t.Fatal(err)
			}
			record := loadTerminalRecord(path)
			if len(record.entries()) != tc.want {
				t.Fatalf("entries = %d; want %d", len(record.entries()), tc.want)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("file %s missing: %v", path, err)
			}
			unreadable := path + ".unreadable"
			if _, err := os.Stat(unreadable); !os.IsNotExist(err) {
				t.Fatalf("unreadable file %s exists", unreadable)
			}
		})
	}
}

func TestTerminalRecordOlderUnreadableReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terminal-ptys.json")
	unreadable := path + ".unreadable"
	if err := os.WriteFile(unreadable, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = loadTerminalRecord(path)
	data, err := os.ReadFile(unreadable)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("unreadable file contents = %q; want %q", data, "new")
	}
}

func TestTerminalRecordCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terminal-ptys.json")
	record := loadTerminalRecord(path)
	for i := 0; i < 64; i++ {
		if err := record.Add(fmt.Sprintf("id%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := record.entries()

	err = record.Add("id64", "")
	if !errors.Is(err, errTerminalRecordFull) {
		t.Fatalf("add 65th err = %v; want %v", err, errTerminalRecordFull)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("bytes changed")
	}
	if !reflect.DeepEqual(record.entries(), entries) {
		t.Fatalf("entries changed")
	}
}

func TestTerminalRecordWriteFailureRollsBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "terminal-ptys.json")
	record := loadTerminalRecord(path)
	if err := record.Add("a", ""); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	if err := record.Add("b", ""); err == nil {
		t.Fatalf("expected error on add")
	}
	if len(record.entries()) != 1 || record.entries()[0].ID != "a" {
		t.Fatalf("entries = %v; want only 'a'", record.entries())
	}

	if err := record.Remove("a"); err == nil {
		t.Fatalf("expected error on remove")
	}
	if len(record.entries()) != 1 || record.entries()[0].ID != "a" {
		t.Fatalf("entries = %v; want only 'a'", record.entries())
	}
}

func TestTerminalRecordInvalidID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terminal-ptys.json")
	record := loadTerminalRecord(path)
	if err := record.Add("bad\u0001id", ""); !errors.Is(err, errTerminalRecordInvalid) {
		t.Fatalf("err = %v; want %v", err, errTerminalRecordInvalid)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not exist")
	}
}
