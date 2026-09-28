package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceMiddlewareFollowsMarkerChanges(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "upgrade-maintenance")
	calls := 0
	handler := maintenanceMiddleware(marker, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))

	request := func(wantStatus int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/write", nil))
		if recorder.Code != wantStatus {
			t.Fatalf("status = %d, want %d", recorder.Code, wantStatus)
		}
	}

	request(http.StatusNoContent)
	if err := os.WriteFile(marker, []byte("upgrade\n"), 0o600); err != nil {
		t.Fatalf("create marker: %v", err)
	}
	request(http.StatusServiceUnavailable)
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	request(http.StatusNoContent)

	if calls != 2 {
		t.Fatalf("next called %d times, want 2", calls)
	}
}

func TestMaintenanceMiddlewareAllowsReadsAndBlocksWrites(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "upgrade-maintenance")
	if err := os.WriteFile(marker, []byte("upgrade\n"), 0o600); err != nil {
		t.Fatalf("create marker: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			called := false
			handler := maintenanceMiddleware(marker, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(method, "/read", nil))

			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
			}
			if !called {
				t.Fatal("read request did not reach next handler")
			}
		})
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			called := false
			handler := maintenanceMiddleware(marker, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(method, "/write", nil))

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
			}
			if called {
				t.Fatal("mutating request reached next handler during maintenance")
			}
		})
	}
}

func TestMaintenanceMiddlewareFailsClosedWithoutLeakingStatError(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "private-upgrade-marker")
	if err := os.Symlink(filepath.Base(marker), marker); err != nil {
		t.Fatalf("create symlink loop: %v", err)
	}

	called := false
	handler := maintenanceMiddleware(marker, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/write", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if called {
		t.Fatal("request reached next handler after marker stat error")
	}
	if body := recorder.Body.String(); strings.Contains(body, marker) || strings.Contains(strings.ToLower(body), "symlink") {
		t.Fatalf("response leaked marker details: %q", body)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			readCalled := false
			readHandler := maintenanceMiddleware(marker, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				readCalled = true
				w.WriteHeader(http.StatusNoContent)
			}))
			readRecorder := httptest.NewRecorder()
			readHandler.ServeHTTP(readRecorder, httptest.NewRequest(method, "/read", nil))
			if readRecorder.Code != http.StatusNoContent || !readCalled {
				t.Fatalf("read request status = %d, called = %v; want 204 and true", readRecorder.Code, readCalled)
			}
		})
	}
}
