package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// handleGetOperatorDataDisclosure says what this Hub keeps about a machine,
// where each kind came from, how long it stays, and what survives retirement.
//
// 它讀的是這個 Hub 現行的保留期，不是預設值：一份印著預設保留期的揭露面，會在
// 有人把保留期調短之後繼續承諾一段它其實已經刪掉的歷史。
func (h *hub) handleGetOperatorDataDisclosure(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "data-disclosure 不接受 query parameters")
		return
	}
	disclosure, err := operator.DataDisclosureFor(h.retention, time.Now().UTC())
	if err != nil {
		log.Printf("failed to read operator data disclosure: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取資料揭露面失敗")
		return
	}
	writeJSON(w, http.StatusOK, disclosure)
}

// handleGetOperatorMachineData measures what this Hub currently holds about one
// machine, category by category.
func (h *hub) handleGetOperatorMachineData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine data 不接受 query parameters")
		return
	}
	machineID := r.PathValue("id")
	result, err := h.operatorReportService().MachineData(machineID, h.retention, time.Now().UTC())
	if err != nil {
		writeOperatorMachineDataError(w, err, machineID)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeOperatorMachineDataError(w http.ResponseWriter, err error, machineID string) {
	var rejection *store.OperatorRequestError
	switch {
	case errors.Is(err, operator.ErrInvalidDataDisclosure):
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine data 請求不合法")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
	case errors.As(err, &rejection) && rejection.Code == store.OperatorCodeMachineNotFound:
		writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
	default:
		log.Printf("failed to read operator machine data machine=%q: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取單機資料揭露面失敗")
	}
}
