package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/teddashh/AI-Intune/internal/certificates"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (h *hub) certificateLoop(ctx context.Context) {
	// Alert delivery has its own loop so a slow notifier cannot delay TLS probes.
	go h.certificateNotificationLoop(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		h.probeCertificates(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (h *hub) probeCertificates(ctx context.Context) {
	items, err := h.store.Certificates()
	if err != nil {
		return
	}
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, m := range items {
		if ctx.Err() != nil {
			break
		}
		if time.Since(m.Result.AttemptedAt) < certificates.Interval {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(m certificates.Monitor) {
			defer wg.Done()
			defer func() { <-slots }()
			r := certificates.Probe(ctx, m.Config, nil)
			if ctx.Err() == nil {
				_ = h.store.RecordCertificateProbe(m, r)
			}
		}(m)
	}
	wg.Wait()
}

func (h *hub) certificateNotificationLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		items, err := h.store.Certificates()
		if err != nil {
			continue
		}
		for _, m := range items {
			if ctx.Err() != nil {
				return
			}
			h.notifyCertificate(certificates.Evaluate(m, time.Now()))
		}
	}
}

// Configuration identity keeps manual probe generations from bypassing alert deduplication.
func certificateNotifyKind(s certificates.Status) string {
	raw, _ := json.Marshal(struct {
		Endpoint string
		Expected []string
		Days     []int
	}{s.Endpoint, s.Expected, s.Days})
	return fmt.Sprintf("certificate:%s:%x:%s", s.ID, sha256.Sum256(raw), strings.Join(s.Flags, ","))
}
func (h *hub) notifyCertificate(s certificates.Status) {
	if len(s.Flags) == 0 {
		return
	}
	now := time.Now()
	kind := certificateNotifyKind(s)
	stats, err := h.store.NotifyKindStats(kind)
	if err != nil {
		return
	}
	if stats.HasSuccess && now.Sub(stats.LastSuccess) < 24*time.Hour {
		return
	}
	if stats.HasAttempt && now.Sub(stats.LastAttempt) < notifyBackoff(int(stats.ConsecutiveFailures)) {
		return
	}
	// Keep user-entered notes, endpoints and certificate text out of notification logs.
	h.deliver(kind, fmt.Sprintf("TLS certificate monitor needs attention: %s. Review /tenant/certificates for evidence, owner and next action.", strings.Join(s.Flags, ", ")), now)
}
func (h *hub) certificateRoutes(mux *http.ServeMux) []string {
	patterns := []string{"GET /v1/operator/certificates", "GET /v1/operator/certificates/{id}", "POST /v1/operator/certificates", "PUT /v1/operator/certificates/{id}", "DELETE /v1/operator/certificates/{id}", "POST /v1/operator/certificates/{id}/probe"}
	for _, p := range patterns {
		mux.HandleFunc(p, h.handleCertificates)
	}
	return patterns
}
func (h *hub) handleCertificates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeErr(w, 400, "BAD_REQUEST", "queries unsupported")
		return
	}
	id := r.PathValue("id")
	if r.Method == "GET" {
		items, err := h.retentionOperator().CertificateRead()
		if err != nil {
			writeErr(w, 500, "STORE_ERROR", "unable to load monitors")
			return
		}
		out := []certificates.Status{}
		for _, m := range items {
			if id == "" || m.ID == id {
				out = append(out, m)
			}
		}
		certificates.Sort(out)
		if id != "" {
			if len(out) == 0 {
				writeErr(w, 404, "NOT_FOUND", "monitor not found")
				return
			}
			writeJSON(w, 200, out[0])
			return
		}
		writeJSON(w, 200, struct {
			Items []certificates.Status `json:"items"`
		}{out})
		return
	}
	var req struct {
		Revision int64    `json:"revision"`
		Endpoint string   `json:"endpoint"`
		Expected []string `json:"expected"`
		Owner    string   `json:"owner"`
		Notes    string   `json:"notes"`
		Days     []int    `json:"days"`
	}
	if rejection := decodeOperatorBody(w, r, &req); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	c := certificates.Config{Endpoint: req.Endpoint, Expected: req.Expected, Owner: req.Owner, Notes: req.Notes, Days: req.Days}
	var err error
	switch {
	case r.Method == "DELETE":
		err = h.retentionOperator().CertificateDelete(id, req.Revision, operatorActor(r))
	case strings.HasSuffix(r.URL.Path, "/probe"):
		err = h.retentionOperator().CertificateProbe(id, req.Revision, operatorActor(r))
	default:
		if err = c.Validate(); err == nil {
			id, err = h.retentionOperator().CertificateSave(id, req.Revision, c, operatorActor(r))
		}
	}
	if err != nil {
		writeErr(w, 400, "INVALID_MONITOR", "invalid fields or monitor revision; reload before retrying")
		return
	}
	writeJSON(w, 200, map[string]string{"id": id})
}
