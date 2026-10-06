package probe

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/teddashh/AI-Intune/internal/processenv"
)

const (
	darwinSwVersPath = "/usr/bin/sw_vers"
	// maxDarwinPlistBytes bounds SystemVersion.plist. The file is a small
	// dictionary; a huge payload is not a version identity.
	maxDarwinPlistBytes = 256 << 10
	// maxDarwinSwVersBytes bounds `sw_vers` stdout. Apple's listing is a
	// handful of short Key:\tValue lines.
	maxDarwinSwVersBytes = 4 << 10
)

var (
	errDarwinPlistTooLarge     = errors.New("SystemVersion.plist exceeds bound")
	errDarwinSwVersOutputBound = errors.New("sw_vers output exceeds bound")
	// darwinProductVersionPattern is the ProductVersion shape in sw_vers(1)
	// (for example 13.0, 15.1, 15.1.1). A non-numeric token is not identity.
	darwinProductVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+){0,2}$`)
)

type darwinSwVersFunc func() (stdout string, err error)

// darwinOSDisplay is the probe identity catalog maps to darwin. ProductName
// must be exactly macOS; any other name (including the SYSTEM_VERSION_COMPAT
// "Mac OS X" listing) stays empty unknown instead of looking like Linux.
func darwinOSDisplay(productName, version string) string {
	if productName != "macOS" || !darwinProductVersionPattern.MatchString(version) {
		return ""
	}
	return productName + " " + version
}

func looksLikeDarwinXMLPlist(b []byte) bool {
	s := bytes.TrimLeft(b, " \t\r\n")
	return bytes.HasPrefix(s, []byte("<?xml")) ||
		bytes.HasPrefix(s, []byte("<!DOCTYPE")) ||
		bytes.HasPrefix(s, []byte("<plist"))
}

func isDarwinBinaryPlist(b []byte) bool {
	return bytes.HasPrefix(b, []byte("bplist"))
}

// resolveDarwinPrettyOSName is the Mac OS identity decision. A working XML
// parse never launches sw_vers. Incomplete or malformed XML stays unknown.
// Missing, oversized, binary, or other non-XML bytes may use sw_vers.
func resolveDarwinPrettyOSName(plist []byte, readErr error, swVers darwinSwVersFunc) string {
	if readErr == nil {
		if name := parseDarwinSystemVersion(plist); name != "" {
			return name
		}
		if looksLikeDarwinXMLPlist(plist) {
			return ""
		}
	}
	if swVers == nil {
		return ""
	}
	out, err := swVers()
	if err != nil {
		return ""
	}
	return parseDarwinSwVers(out)
}

func withoutSystemVersionCompat(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, variable := range environment {
		name, _, hasValue := strings.Cut(variable, "=")
		if hasValue && name == "SYSTEM_VERSION_COMPAT" {
			continue
		}
		result = append(result, variable)
	}
	return result
}

type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		return 0, errDarwinSwVersOutputBound
	}
	return b.buf.Write(p)
}

// runDarwinSwVersAt runs Apple's sw_vers with a deadline, WaitDelay, bounded
// stdout, and SYSTEM_VERSION_COMPAT removed from this child only.
func runDarwinSwVersAt(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), quickTimeout)
	defer cancel()
	cmd := processenv.CommandContext(ctx, path)
	cmd.Env = withoutSystemVersionCompat(cmd.Env)
	cmd.WaitDelay = waitDelay
	stdout := &boundedBuffer{max: maxDarwinSwVersBytes}
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.buf.String(), nil
}

func invokeDarwinSwVers() (string, error) {
	return runDarwinSwVersAt(darwinSwVersPath)
}

func readDarwinSystemVersionPlist(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, maxDarwinPlistBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDarwinPlistBytes {
		return nil, errDarwinPlistTooLarge
	}
	return b, nil
}

// parseDarwinSwVers reads the default listing documented by sw_vers(1):
// ProductName, ProductVersion, optional ProductVersionExtra, BuildVersion.
// ProductUserVisibleVersion is not a sw_vers field; the native XML parse
// keeps that key's priority. Malformed listings stay empty.
func parseDarwinSwVers(stdout string) string {
	if stdout == "" || len(stdout) > maxDarwinSwVersBytes || strings.ContainsRune(stdout, '\x00') {
		return ""
	}
	values := make(map[string]string, 4)
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return ""
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return ""
		}
		if seen, dup := values[key]; dup && seen != value {
			return ""
		}
		values[key] = value
	}
	return darwinOSDisplay(values["ProductName"], values["ProductVersion"])
}

// parseOSReleasePrettyName 解析 /etc/os-release 的 PRETTY_NAME。
func parseOSReleasePrettyName(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == "PRETTY_NAME" {
			if unq, err := strconv.Unquote(v); err == nil {
				return unq
			}
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// parseDarwinSystemVersion 解析 macOS 的 SystemVersion.plist XML。
// Binary magic is not XML; the caller may then use sw_vers. This is
// detection, not a binary plist parser.
func parseDarwinSystemVersion(b []byte) string {
	if isDarwinBinaryPlist(b) {
		return ""
	}
	decoder := xml.NewDecoder(bytes.NewReader(b))
	values := make(map[string]string, 3)
	pendingKey := ""
	for {
		token, err := decoder.Token()
		if err != nil {
			// A visible version can follow ProductVersion, so only use the
			// fallback after the complete XML document has been read.
			if err == io.EOF && values["ProductName"] != "" && values["ProductVersion"] != "" {
				return values["ProductName"] + " " + values["ProductVersion"]
			}
			return ""
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			var key string
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return ""
			}
			if key == "ProductName" || key == "ProductUserVisibleVersion" || key == "ProductVersion" {
				pendingKey = key
			} else {
				pendingKey = ""
			}
		case "string":
			if pendingKey == "" {
				continue
			}
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return ""
			}
			values[pendingKey] = value
			pendingKey = ""
			if values["ProductName"] != "" && values["ProductUserVisibleVersion"] != "" {
				return values["ProductName"] + " " + values["ProductUserVisibleVersion"]
			}
		default:
			if pendingKey != "" {
				pendingKey = ""
			}
		}
	}
}
