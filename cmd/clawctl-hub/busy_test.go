package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachineStoreErrorMapsBusyAndOtherFailures(t *testing.T) {
	h := newHub(t, "")

	busy := httptest.NewRecorder()
	h.writeStoreError(busy, "寫入", "machine-1", fmt.Errorf("lock: %w", store.ErrWriterBusy))
	assertHubBusy(t, busy)

	other := httptest.NewRecorder()
	h.writeStoreError(other, "寫入", "machine-1", errors.New("disk I/O"))
	assertInternal(t, other, "寫入失敗")
	if other.Header().Get("Retry-After") != "" {
		t.Fatalf("non-busy store error set Retry-After %q", other.Header().Get("Retry-After"))
	}
}

func TestAgentAuthMapsUnauthorizedBusyAndOtherFailures(t *testing.T) {
	h := newHub(t, "")
	handler := h.authed(func(http.ResponseWriter, *http.Request, string) {
		t.Fatal("handler ran for a rejected credential")
	})

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/v1/checkins", nil))
	assertStatusCode(t, missing, http.StatusUnauthorized, model.ErrUnauthorized)

	unknown := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/checkins", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	handler.ServeHTTP(unknown, req)
	assertStatusCode(t, unknown, http.StatusUnauthorized, model.ErrUnauthorized)
	if strings.Contains(unknown.Body.String(), "not-a-real-token") {
		t.Fatalf("unauthorized body echoed the token: %s", unknown.Body.String())
	}

	busy := httptest.NewRecorder()
	h.finishStoreError(busy, "authenticate agent", "", "internal error", store.ErrWriterBusy)
	assertHubBusy(t, busy)

	if err := h.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	closed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/checkins", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	handler.ServeHTTP(closed, req)
	assertInternal(t, closed, "internal error")
	if closed.Body.String() == unknown.Body.String() {
		t.Fatal("a closed store was answered 401, which tells the agent to drop its token")
	}
}

func TestVerifierAuthMapsUnauthorizedAndOtherFailures(t *testing.T) {
	h := newHub(t, "")
	handler := h.verifierAuthed(func(http.ResponseWriter, *http.Request, store.Verifier) {
		t.Fatal("handler ran for a rejected verifier credential")
	})

	unknown := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/verifications", nil)
	req.Header.Set("Authorization", "Bearer not-a-verifier")
	handler.ServeHTTP(unknown, req)
	assertStatusCode(t, unknown, http.StatusUnauthorized, model.ErrUnauthorized)

	busy := httptest.NewRecorder()
	h.finishStoreError(busy, "authenticate verifier", "", "internal error", fmt.Errorf("lock: %w", store.ErrWriterBusy))
	assertHubBusy(t, busy)

	if err := h.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	closed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/verifications", nil)
	req.Header.Set("Authorization", "Bearer not-a-verifier")
	handler.ServeHTTP(closed, req)
	assertInternal(t, closed, "internal error")
}

func TestOperatorWriteErrMapsBusyToRetryAfter(t *testing.T) {
	busyRec := httptest.NewRecorder()
	status, code, detail := operator.HTTPError(fmt.Errorf("hidden /private/hub.db: %w", store.ErrWriterBusy))
	writeErr(busyRec, status, code, detail)
	assertHubBusy(t, busyRec)
	if strings.Contains(busyRec.Body.String(), "/private") || strings.Contains(busyRec.Body.String(), "hub.db") {
		t.Fatalf("operator busy body leaked a path: %s", busyRec.Body.String())
	}

	other := httptest.NewRecorder()
	status, code, detail = operator.HTTPError(errors.New("sqlite secret path /private/hub.db"))
	writeErr(other, status, code, detail)
	assertInternal(t, other, "控制面操作失敗")
	if other.Header().Get("Retry-After") != "" {
		t.Fatalf("operator internal error set Retry-After %q", other.Header().Get("Retry-After"))
	}
	if strings.Contains(other.Body.String(), "/private") || strings.Contains(other.Body.String(), "hub.db") {
		t.Fatalf("operator internal body leaked a path: %s", other.Body.String())
	}
}

func assertHubBusy(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assertStatusCode(t, rec, http.StatusServiceUnavailable, model.ErrHubBusy)
	var body model.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("busy body: %v", err)
	}
	if body.Message != "the hub is busy; retry shortly" {
		t.Fatalf("busy message = %q", body.Message)
	}
	n, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || n < 5 || n > 15 {
		t.Fatalf("Retry-After = %q, want an integer in [5,15]", rec.Header().Get("Retry-After"))
	}
}

func assertInternal(t *testing.T, rec *httptest.ResponseRecorder, message string) {
	t.Helper()
	assertStatusCode(t, rec, http.StatusInternalServerError, "INTERNAL")
	var body model.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("internal body: %v", err)
	}
	if body.Message != message {
		t.Fatalf("internal message = %q, want %q", body.Message, message)
	}
}

func assertStatusCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	var body model.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%s)", err, rec.Body.String())
	}
	if body.Code != code {
		t.Fatalf("code = %q, want %q", body.Code, code)
	}
}
