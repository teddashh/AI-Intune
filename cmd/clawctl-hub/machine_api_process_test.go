package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

// TestMain lets agent integration tests launch the machine/operator APIs and
// job read CLI in separate processes without production services. A normal
// test run just executes m.Run.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--test-machine-api-server" {
		if err := runMachineAPITestServer(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == "--test-job-read-cli" {
		if err := runJobReadTestCLI(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runMachineAPITestServer() error {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	var cfg struct {
		DBPath              string `json:"db_path"`
		ArtifactsDir        string `json:"artifacts_dir"`
		DropAssignmentReply bool   `json:"drop_assignment_reply"`
	}
	if err := json.Unmarshal(line, &cfg); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	mux := http.NewServeMux()
	h := &hub{store: st, artifactsDir: cfg.ArtifactsDir}
	h.machineAndPublicRoutes(mux)
	operatorMux := http.NewServeMux()
	h.operatorRoutes(operatorMux)
	// The HTTP routes and boundary are real; LocalAPI identity is a test fixture.
	authorizer := boundaryAuthorizeFunc(func(r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision) {
		principal := boundaryPrincipal(permission)
		principal.SourceAddr = r.RemoteAddr
		return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
			Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
		}
	})
	mux.Handle("/", newOperatorBoundary(operatorMux, authorizer,
		st, operatorRoutePolicies, testOperatorAuthority))
	var handler http.Handler = mux
	if cfg.DropAssignmentReply {
		handler = dropFirstNodeAssignmentResponse(handler)
	}

	server := httptest.NewServer(handler)
	defer server.Close()

	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"url": server.URL}); err != nil {
		return err
	}
	// Same buffered reader so no input is lost; blocks until stdin EOF.
	_, err = io.Copy(io.Discard, reader)
	return err
}

func dropFirstNodeAssignmentResponse(next http.Handler) http.Handler {
	var dropped atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/profile-assignments") || r.Header.Get("Idempotency-Key") != "node-reassignment" {
			next.ServeHTTP(w, r)
			return
		}
		rr := httptest.NewRecorder()
		next.ServeHTTP(rr, r)
		if rr.Code == http.StatusCreated && dropped.CompareAndSwap(false, true) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack failed", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				http.Error(w, "hijack failed", http.StatusInternalServerError)
				return
			}
			_ = conn.Close()
			return
		}
		h := w.Header()
		for k, vv := range rr.Header() {
			h[k] = append([]string(nil), vv...)
		}
		w.WriteHeader(rr.Code)
		_, _ = w.Write(rr.Body.Bytes())
	})
}
