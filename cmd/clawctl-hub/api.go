package main

import (
	"encoding/json"
	"errors"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Agent 面的 API。
//
// ⚠ 這裡沒有任何一支是 Hub 主動打進機器的。全部都是 Agent 向外連線。
// 機器上不開任何 inbound 控制 port —— 一旦開了，這東西就是一個批量後門。

// resolveAgentSettings answers what one machine must run under, and says so
// with the digest that machine will echo back on its next heartbeat.
//
// ⚠ 解析失敗時回產品預設值，不是回錯誤。心跳是判生死用的；因為一個設定問題
// 而拒收心跳，會把「設定讀不到」誤報成「機器死了」。
func (h *hub) resolveAgentSettings(machineID string) (settingpolicy.Settings, string) {
	effective, err := h.store.ResolveMachineSettings(machineID)
	if err != nil {
		log.Printf("解析設定失敗 machine=%s: %v", machineID, err)
		effective = settingpolicy.Effective{Settings: settingpolicy.Defaults(),
			Source: settingpolicy.SourceDefault}
	}
	digest := effective.Digest
	if digest == "" {
		digest = settingpolicy.MustDigest(effective.Settings)
	}
	return effective.Settings, digest
}

// machineAndPublicRoutes registers the machine plane and the public liveness
// probe. /metrics is an operator view route; exposition unit tests mount
// handleMetrics on their own mux.
func (h *hub) machineAndPublicRoutes(mux *http.ServeMux) []string {
	patterns := h.agentRoutes(mux)
	patterns = append(patterns, "GET /healthz")
	mux.HandleFunc(patterns[len(patterns)-1], h.handleHealthz)
	return patterns
}

// agentRoutes is the machine-to-Hub plane. Every route after enrollment is
// guarded by the machine bearer middleware. Do not add operator actions here:
// a machine token must never become operator app-cap authority.
func (h *hub) agentRoutes(mux *http.ServeMux) []string {
	patterns := []string{
		"POST /v1/enrollments",
		"POST /v1/checkins",
		"POST /v1/observations:batch",
		"GET /v1/jobs/next",
		"GET /v1/agent/readiness",
		"POST /v1/jobs/{id}/claims",
		"POST /v1/jobs/{id}/lease:renew",
		"POST /v1/jobs/{id}/events",
		"POST /v1/jobs/{id}/verifications",
		"POST /v1/jobs/{id}/complete",
		"POST /v1/jobs/{id}/reject",
		"GET /v1/capabilities",
		"GET /v1/artifacts/{sha256}",
		"HEAD /v1/artifacts/{sha256}",
		"POST /v1/verifications",
		"GET /v1/verification-assignments",
	}
	mux.HandleFunc(patterns[0], h.handleEnroll)
	mux.HandleFunc(patterns[1], h.authed(h.handleCheckin))
	mux.HandleFunc(patterns[2], h.authed(h.handleObservations))
	mux.HandleFunc(patterns[3], h.authed(h.handleNextJob))
	mux.HandleFunc(patterns[4], h.authed(h.handleAgentReadiness))
	mux.HandleFunc(patterns[5], h.authed(h.handleClaimJob))
	mux.HandleFunc(patterns[6], h.authed(h.handleRenewJobLease))
	mux.HandleFunc(patterns[7], h.authed(h.handleJobEvent))
	mux.HandleFunc(patterns[8], h.authed(h.handleJobVerification))
	mux.HandleFunc(patterns[9], h.authed(h.handleCompleteJob))
	mux.HandleFunc(patterns[10], h.authed(h.handleRejectJob))
	mux.HandleFunc(patterns[11], h.authed(h.handleCapabilities))
	mux.HandleFunc(patterns[12], h.authed(h.handleGetArtifact))
	mux.HandleFunc(patterns[13], h.authed(h.handleGetArtifact))
	mux.HandleFunc(patterns[14], h.verifierAuthed(h.handleIndependentVerification))
	mux.HandleFunc(patterns[15], h.verifierAuthed(h.handleVerificationAssignments))
	return patterns
}

// authed 把 bearer token 換成 machine_id。
//
// ⚠ Phase 1 用 bearer token 而不是 mTLS，這是對 SPEC §5.1 的一個
// 有意識的偏離，理由與償還條件寫在 docs/PHASE1.md。簡單說：Phase 1 沒有
// 任何寫入路徑，被盜用的 token 最多能塞假的觀測進來 —— 那很糟，但不是
// 「有人拿它去改 30 台機器」。
//
// ⚠ 2026-09-06 Phase 4 開工時重新開過這個決定：**不做 mTLS**，改成把它
// 要保護的三個性質寫成強制不變量 —— 領工作單只領得到自己 machine_id 的、
// artifact digest 在 activation 之前驗、只綁 tailnet。理由與**它們擋不住
// 什麼**（被偷的 token 仍能塞假觀測；digest 擋不住 Hub 本身被拿下）
// 寫在 docs/PHASE1.md §4，連同重新開這個決定的三個觸發條件。
func (h *hub) authed(next func(w http.ResponseWriter, r *http.Request, machineID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, model.ErrUnauthorized, "缺少 bearer token")
			return
		}
		machineID, err := h.store.AuthenticateAgent(tok)
		if err != nil {
			// ⚠ 不要在錯誤訊息裡透露 token 是「不存在」還是「不對」。
			// 鎖競爭不是憑證錯誤：回 401 會讓 agent 丟掉 token。
			if errors.Is(err, store.ErrUnauthorized) {
				writeErr(w, http.StatusUnauthorized, model.ErrUnauthorized, "token 無效")
				return
			}
			h.finishStoreError(w, "authenticate agent", "", "internal error", err)
			return
		}
		next(w, r, machineID)
	}
}

func (h *hub) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req model.EnrollRequest
	if !decode(w, r, &req) {
		return
	}
	if req.SchemaVersion != model.SchemaVersion {
		// ⚠ 不認得的版本就拒絕並說清楚，不要盡力解析。
		// 盡力解析是 adapter 讀到屍體檔的同一類錯誤。
		writeErr(w, http.StatusBadRequest, model.ErrBadSchemaVersion,
			"這個 Hub 只認得 schema_version=1，收到的是不同版本；請升級 agent")
		return
	}

	machineID, token, err := h.store.RedeemEnrollToken(req.EnrollToken, req, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrEnrollToken) {
			writeErr(w, http.StatusForbidden, model.ErrEnrollTokenBad,
				"報到 token 無效、已使用或已過期")
			return
		}
		h.writeStoreError(w, "報到", "", err)
		return
	}

	log.Printf("已報到 machine=%s host=%s user=%s", machineID, req.Hostname, req.UnixUser)
	settings, settingsDigest := h.resolveAgentSettings(machineID)
	writeJSON(w, http.StatusOK, model.EnrollResponse{
		SchemaVersion:              model.SchemaVersion,
		MachineID:                  machineID,
		AgentToken:                 token,
		CheckinIntervalSeconds:     settings.CheckinIntervalSeconds,
		ObservationIntervalSeconds: settings.ObservationIntervalSeconds,
		SettingsDigest:             settingsDigest,
	})
}

func (h *hub) handleCheckin(w http.ResponseWriter, r *http.Request, machineID string) {
	var c model.Checkin
	if !decode(w, r, &c) {
		return
	}
	// ⚠ received_at 由 Hub 決定，不接受 Agent 給的時間。
	// 生死判斷一律用這個 —— 機器時鐘快兩天不能讓它看起來永遠新鮮。
	received := time.Now().UTC()

	if err := h.store.RecordCheckin(machineID, c, received); err != nil {
		h.writeStoreError(w, "寫入", machineID, err)
		return
	}
	// ⚠ 期望是按**顯示名稱**宣告的（人寫設定檔時想的是 "sampleagent2"，
	// 不是 machine_id 那串十六進位）。查不到名字就不下發 —— 寧可什麼都不量，
	// 也不要因為名字對不上而把 A 機器的期望套到 B 機器上。
	var exps []model.Expectation
	name := h.store.DisplayName(machineID)
	if name != "" {
		exps = h.store.Expectations().For(name)
	}
	policyToken, err := h.store.CurrentWorkloadPolicyToken(name)
	if err != nil {
		h.writeStoreError(w, "下發期望", machineID, err)
		return
	}
	settings, settingsDigest := h.resolveAgentSettings(machineID)
	writeJSON(w, http.StatusOK, model.CheckinResponse{
		ReceivedAt:                 received,
		CheckinIntervalSeconds:     settings.CheckinIntervalSeconds,
		ObservationIntervalSeconds: settings.ObservationIntervalSeconds,
		SettingsDigest:             settingsDigest,
		Expectations:               exps,
		WorkloadPolicyToken:        policyToken,
	})
}

func (h *hub) handleAgentReadiness(w http.ResponseWriter, r *http.Request, machineID string) {
	result, err := h.store.AgentReadiness(machineID)
	if err != nil {
		h.finishStoreError(w, "讀取 agent readiness", machineID, "讀取 agent readiness 失敗", err)
		return
	}
	writeJSON(w, http.StatusOK, model.AgentReadinessResponse{
		MachineID: result.MachineID, LastCheckinReceivedAt: result.LastCheckinReceivedAt,
		AgentStartedAt: result.AgentStartedAt, AgentVersion: result.AgentVersion, JobsEnabled: result.JobsEnabled,
		DeviceSyncV1: result.DeviceSyncV1, IdentityReceivedAt: result.IdentityReceivedAt,
		IdentityMeasuredAt: result.IdentityMeasuredAt, IdentityOS: result.IdentityOS, IdentityArch: result.IdentityArch,
	})
}

func (h *hub) handleObservations(w http.ResponseWriter, r *http.Request, machineID string) {
	var b model.ObservationBatch
	if !decode(w, r, &b) {
		return
	}
	if b.SchemaVersion != model.SchemaVersion {
		writeErr(w, http.StatusBadRequest, model.ErrBadSchemaVersion,
			"這個 Hub 只認得 schema_version=1；請升級 agent")
		return
	}
	if err := h.store.RecordObservation(machineID, b, time.Now().UTC()); err != nil {
		h.writeStoreError(w, "寫入", machineID, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHealthz 只說「這個 process 還在」。
//
// ⚠ 它刻意不檢查資料庫、不檢查機隊狀態，也刻意不叫 /health ——
// 因為 Hub 說自己健康不算數。真正的 Hub 健康判定在外部死人之鐘那裡：
// 早報有沒有準時送到。這支只是給反向代理用的存活探針。
func (h *hub) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("alive\n"))
}

// ---------------------------------------------------------------- helpers

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "JSON 解析失敗："+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	// ⚠ 不做 HTML 轉義：工作單的 spec 是 json.RawMessage，預設轉義會把 `<`、`>`、`&`
	// 改寫成 \u003c…，agent 對線上位元組算 sha256 就跟 Hub 開單時釘的對不上。
	// 這裡的 JSON 只給 agent 與 CLI 讀，不進 HTML。
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("回應寫入失敗：%v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, apiCode, msg string) {
	if apiCode == model.ErrHubBusy {
		w.Header().Set("Retry-After", strconv.Itoa(hubBusyRetryAfter()))
	}
	writeJSON(w, code, model.APIError{Code: apiCode, Message: msg})
}

// hubBusyRetryAfter is a uniform integer in [5, 15] seconds. Agents that
// honor Retry-After should spread their retries across that window.
func hubBusyRetryAfter() int {
	return 5 + rand.IntN(11)
}

// writeStoreError is the machine-plane mapping for a store failure. The
// public message keeps the caller's existing text; lock contention is the
// one case that changes status.
func (h *hub) writeStoreError(w http.ResponseWriter, action, machineID string, err error) {
	h.finishStoreError(w, action, machineID, action+"失敗", err)
}

func (h *hub) finishStoreError(w http.ResponseWriter, action, machineID, publicMessage string, err error) {
	if machineID != "" {
		log.Printf("action=%s machine=%s err=%v", action, machineID, err)
	} else {
		log.Printf("action=%s err=%v", action, err)
	}
	if store.IsBusy(err) {
		if h != nil && h.store != nil {
			h.store.NoteBusy(err)
		}
		writeErr(w, http.StatusServiceUnavailable, model.ErrHubBusy, "the hub is busy; retry shortly")
		return
	}
	writeErr(w, http.StatusInternalServerError, "INTERNAL", publicMessage)
}
