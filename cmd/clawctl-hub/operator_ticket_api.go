package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func (h *hub) handleListOperatorTickets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorTicketReadRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "票證使用量 query 不合法")
		return
	}
	service := h.operatorService
	if service == nil {
		service = operator.New(h.store)
	}
	result, err := service.ListTicketsContext(r.Context(), request, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, operator.ErrInvalidTicketRead), errors.Is(err, store.ErrInvalidTicketRead):
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "票證使用量篩選或時間範圍不合法")
		case errors.Is(err, store.ErrTicketReadTooBroad):
			writeErr(w, http.StatusUnprocessableEntity, "TICKET_READ_TOO_BROAD", "票證使用量查詢超過安全成本上限；請縮短天數")
		default:
			log.Printf("failed to read operator tickets: %v", err)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取票證使用量失敗")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseOperatorTicketReadRequest(r *http.Request) (operator.TicketReadRequest, error) {
	if r == nil || r.URL == nil || r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.TicketReadRequest{}, errors.New("票證使用量 request 不完整")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.TicketReadRequest{}, errors.New("票證使用量 query 編碼不合法")
	}
	for key := range values {
		if key != "days" && key != "provider_ref" {
			return operator.TicketReadRequest{}, fmt.Errorf("票證使用量不接受 query parameter %q", key)
		}
	}
	request := operator.TicketReadRequest{}
	if raw, present := values["days"]; present {
		if len(raw) != 1 || !validOperatorChangeQueryText(raw[0], 2) {
			return operator.TicketReadRequest{}, errors.New("days 必須只出現一次")
		}
		request.Days, err = strconv.Atoi(raw[0])
		if err != nil || strconv.Itoa(request.Days) != raw[0] || request.Days < 1 {
			return operator.TicketReadRequest{}, errors.New("days 必須是 canonical 整數")
		}
	}
	if raw, present := values["provider_ref"]; present {
		if len(raw) != 1 || !validOperatorChangeQueryText(raw[0], 71) {
			return operator.TicketReadRequest{}, errors.New("provider_ref 必須只出現一次")
		}
		request.ProviderRef = raw[0]
	}
	if err := operator.ValidateTicketReadRequest(request); err != nil {
		return operator.TicketReadRequest{}, err
	}
	return request, nil
}
