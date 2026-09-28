package probe

import (
	"bytes"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
)

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

// parseDarwinSystemVersion 解析 macOS 的 SystemVersion.plist。
func parseDarwinSystemVersion(b []byte) string {
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
