package probe

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/catalog"
)

const appleSwVersListing = "ProductName:\tmacOS\nProductVersion:\t15.1\nBuildVersion:\t24B83\n"

func writeSwVersFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sw_vers")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustStayUnknown(t *testing.T, display string) {
	t.Helper()
	if display != "" {
		t.Fatalf("OS display=%q, want empty unknown", display)
	}
	if _, err := catalog.PlatformFromProbeIdentity(display, "arm64"); err == nil {
		t.Fatal("empty OS must not become a catalog target")
	}
}

func checkDarwinXMLIdentityDoesNotInvokeSwVers(t *testing.T) {
	called := 0
	swVers := func() (string, error) {
		called++
		return appleSwVersListing, nil
	}
	xmlIdentity := parseDarwinSystemVersion([]byte(darwinSystemVersionFixture))
	if xmlIdentity != "macOS 15.1" {
		t.Fatalf("XML identity=%q", xmlIdentity)
	}
	got := resolveDarwinPrettyOSName([]byte(darwinSystemVersionFixture), nil, swVers)
	if got != "macOS 15.1" || called != 0 {
		t.Fatalf("XML fast path got=%q calls=%d", got, called)
	}
}

func checkDarwinXMLKeepsUserVisibleVersionWhenSwVersWouldDiffer(t *testing.T) {
	const name = `<key>ProductName</key><string>macOS</string>`
	const version = `<key>ProductVersion</key><string>15.1</string>`
	const visible = `<key>ProductUserVisibleVersion</key><string>15.1.1</string>`
	body := []byte(`<plist><dict>` + name + version + visible + `</dict></plist>`)
	called := 0
	got := resolveDarwinPrettyOSName(body, nil, func() (string, error) {
		called++
		return "ProductName:\tmacOS\nProductVersion:\t15.1\nBuildVersion:\t24B83\n", nil
	})
	if got != "macOS 15.1.1" || called != 0 {
		t.Fatalf("user-visible XML must win without sw_vers: got=%q calls=%d", got, called)
	}
}

func checkDarwinIncompleteXMLStaysUnknownWithoutSwVers(t *testing.T) {
	called := 0
	swVers := func() (string, error) {
		called++
		return appleSwVersListing, nil
	}
	for _, test := range []struct {
		name string
		in   string
	}{
		{"missing ProductName", `<plist><dict><key>ProductUserVisibleVersion</key><string>15.1</string></dict></plist>`},
		{"name only", `<plist><dict><key>ProductName</key><string>macOS</string></dict></plist>`},
		{"truncated suffix", `<plist><dict><key>ProductName</key><string>macOS</string><key>ProductVersion</key><string>15.1</string><key>ProductBuildVersion</key><string>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			called = 0
			got := resolveDarwinPrettyOSName([]byte(test.in), nil, swVers)
			if got != "" || called != 0 {
				t.Fatalf("incomplete XML got=%q calls=%d; native parse already failed closed", got, called)
			}
		})
	}
}

func checkDarwinBinaryPlistUsesSwVersFallback(t *testing.T) {
	binary := append([]byte("bplist00"), make([]byte, 64)...)
	if parseDarwinSystemVersion(binary) != "" {
		t.Fatal("binary plist must not parse as XML identity")
	}
	called := 0
	got := resolveDarwinPrettyOSName(binary, nil, func() (string, error) {
		called++
		return appleSwVersListing, nil
	})
	if got != "macOS 15.1" || called != 1 {
		t.Fatalf("binary plist fallback got=%q calls=%d", got, called)
	}
	target, err := catalog.PlatformFromProbeIdentity(got, "arm64")
	if err != nil || target != (catalog.Platform{OS: "darwin", Arch: "arm64"}) {
		t.Fatalf("fallback identity must remain darwin: target=%+v err=%v", target, err)
	}
}

func checkDarwinUnreadablePlistUsesSwVersFallback(t *testing.T) {
	called := 0
	got := resolveDarwinPrettyOSName(nil, errors.New("no such file"), func() (string, error) {
		called++
		return appleSwVersListing, nil
	})
	if got != "macOS 15.1" || called != 1 {
		t.Fatalf("unreadable plist fallback got=%q calls=%d", got, called)
	}
}

func checkDarwinSwVersFailureStaysUnknown(t *testing.T) {
	mustStayUnknown(t, resolveDarwinPrettyOSName([]byte("bplist00"), nil, func() (string, error) {
		return "", errors.New("sw_vers: timeout")
	}))
	mustStayUnknown(t, resolveDarwinPrettyOSName([]byte("bplist00"), nil, func() (string, error) {
		return "not a listing", nil
	}))
}

func checkParseDarwinSwVersAcceptsAppleListingAndRejectsMalformed(t *testing.T) {
	for _, test := range []struct {
		name string
		in   string
		want string
	}{
		{"tab listing", appleSwVersListing, "macOS 15.1"},
		{"spaces", "ProductName:    macOS\nProductVersion:    13.0\nBuildVersion:    22A100\n", "macOS 13.0"},
		{"rapid extra", "ProductName:\tmacOS\nProductVersion:\t13.0\nProductVersionExtra:\t(a)\nBuildVersion:\t22A100\n", "macOS 13.0"},
		{"empty", "", ""},
		{"missing product name", "ProductVersion:\t15.1\nBuildVersion:\t24B83\n", ""},
		{"mac os x", "ProductName:\tMac OS X\nProductVersion:\t10.15.7\nBuildVersion:\t19H15\n", ""},
		{"non numeric", "ProductName:\tmacOS\nProductVersion:\tfifteen\n", ""},
		{"bare major", "ProductName:\tmacOS\nProductVersion:\t15\n", ""},
		{"no colon", "macOS 15.1\n", ""},
		{"empty value", "ProductName:\tmacOS\nProductVersion:\t\n", ""},
		{"duplicate conflict", "ProductName:\tmacOS\nProductVersion:\t15.1\nProductVersion:\t15.2\n", ""},
		{"nul", "ProductName:\tmacOS\nProductVersion:\t15.1\n\x00", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := parseDarwinSwVers(test.in)
			if got != test.want {
				t.Fatalf("parseDarwinSwVers=%q want %q", got, test.want)
			}
			if test.want == "" {
				mustStayUnknown(t, got)
				return
			}
			target, err := catalog.PlatformFromProbeIdentity(got, "x86_64")
			if err != nil || target != (catalog.Platform{OS: "darwin", Arch: "amd64"}) {
				t.Fatalf("sw_vers identity must remain darwin: target=%+v err=%v", target, err)
			}
		})
	}
}

func checkRunDarwinSwVersAtUsesChildEnvBoundOutputAndDeadline(t *testing.T) {
	t.Run("well formed listing", func(t *testing.T) {
		path := writeSwVersFixture(t, `printf '%s' $'ProductName:\tmacOS\nProductVersion:\t15.1\nBuildVersion:\t24B83\n'`)
		out, err := runDarwinSwVersAt(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := parseDarwinSwVers(out); got != "macOS 15.1" {
			t.Fatalf("fixture listing got=%q out=%q", got, out)
		}
	})
	t.Run("non-zero exit", func(t *testing.T) {
		path := writeSwVersFixture(t, `echo fail >&2; exit 2`)
		if _, err := runDarwinSwVersAt(path); err == nil {
			t.Fatal("non-zero sw_vers must fail")
		}
		mustStayUnknown(t, resolveDarwinPrettyOSName([]byte("bplist00"), nil, func() (string, error) {
			return runDarwinSwVersAt(path)
		}))
	})
	t.Run("missing command", func(t *testing.T) {
		if _, err := runDarwinSwVersAt(filepath.Join(t.TempDir(), "missing-sw_vers")); err == nil {
			t.Fatal("missing sw_vers must fail")
		}
	})
	t.Run("output bound", func(t *testing.T) {
		path := writeSwVersFixture(t, `head -c 5000 /dev/zero`)
		if _, err := runDarwinSwVersAt(path); err == nil {
			t.Fatal("oversized sw_vers stdout must fail")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		path := writeSwVersFixture(t, `sleep 30`)
		start := time.Now()
		_, err := runDarwinSwVersAt(path)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("sleeping sw_vers must fail the deadline")
		}
		if elapsed > quickTimeout+waitDelay+2*time.Second {
			t.Fatalf("deadline took %s, want around %s", elapsed, quickTimeout)
		}
	})
	t.Run("SYSTEM_VERSION_COMPAT stripped from child only", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "compat.log")
		t.Setenv("SYSTEM_VERSION_COMPAT", "1")
		path := writeSwVersFixture(t, `printf '%s\n' "${SYSTEM_VERSION_COMPAT-UNSET}" >"$SW_VERS_COMPAT_LOG"`)
		t.Setenv("SW_VERS_COMPAT_LOG", logPath)
		if _, err := runDarwinSwVersAt(path); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != "UNSET" {
			t.Fatalf("child SYSTEM_VERSION_COMPAT=%q, want UNSET", got)
		}
		if os.Getenv("SYSTEM_VERSION_COMPAT") != "1" {
			t.Fatal("parent SYSTEM_VERSION_COMPAT must stay set")
		}
	})
}

func checkReadDarwinSystemVersionPlistRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SystemVersion.plist")
	if err := os.WriteFile(path, bytesRepeat('x', maxDarwinPlistBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readDarwinSystemVersionPlist(path)
	if !errors.Is(err, errDarwinPlistTooLarge) {
		t.Fatalf("oversized plist err=%v", err)
	}
	called := 0
	got := resolveDarwinPrettyOSName(nil, err, func() (string, error) {
		called++
		return appleSwVersListing, nil
	})
	if got != "macOS 15.1" || called != 1 {
		t.Fatalf("oversized plist should use sw_vers: got=%q calls=%d", got, called)
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestDarwinNativeIdentityFallback(t *testing.T) {
	t.Run("DarwinXMLIdentityDoesNotInvokeSwVers", checkDarwinXMLIdentityDoesNotInvokeSwVers)
	t.Run("DarwinXMLKeepsUserVisibleVersionWhenSwVersWouldDiffer", checkDarwinXMLKeepsUserVisibleVersionWhenSwVersWouldDiffer)
	t.Run("DarwinIncompleteXMLStaysUnknownWithoutSwVers", checkDarwinIncompleteXMLStaysUnknownWithoutSwVers)
	t.Run("DarwinBinaryPlistUsesSwVersFallback", checkDarwinBinaryPlistUsesSwVersFallback)
	t.Run("DarwinUnreadablePlistUsesSwVersFallback", checkDarwinUnreadablePlistUsesSwVersFallback)
	t.Run("DarwinSwVersFailureStaysUnknown", checkDarwinSwVersFailureStaysUnknown)
	t.Run("ParseDarwinSwVersAcceptsAppleListingAndRejectsMalformed", checkParseDarwinSwVersAcceptsAppleListingAndRejectsMalformed)
	t.Run("RunDarwinSwVersAtUsesChildEnvBoundOutputAndDeadline", checkRunDarwinSwVersAtUsesChildEnvBoundOutputAndDeadline)
	t.Run("ReadDarwinSystemVersionPlistRejectsOversizedFile", checkReadDarwinSystemVersionPlistRejectsOversizedFile)
}
