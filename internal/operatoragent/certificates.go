package operatoragent

import (
	"context"
	"encoding/json"
	"strings"
)

func certificateTools() []Tool {
	return []Tool{
		{Name: "certificate_list", Description: "List TLS expiry, chain, SAN-gap and stale-probe evidence. Includes operator free text; not secret-scanned. Configure monitors in the Hub Certificates page.", InputSchema: obj(map[string]any{}), Annotations: readAnn("certificate_list")},
		{Name: "certificate_get", Description: "Read a TLS monitor and next action. Includes operator free text; not secret-scanned.", InputSchema: obj(map[string]any{"id": strField(1, 128, idPattern, "Monitor ID")}, "id"), Annotations: readAnn("certificate_get")},
	}
}
func (s *Service) certificateCall(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	var req struct {
		ID string `json:"id"`
	}
	if name == "certificate_list" {
		if err := decodeEmpty(raw); err != nil {
			return nil, err
		}
	} else {
		if err := decodeArgs(raw, &req); err != nil {
			return nil, err
		}
		if req.ID == "" || len(req.ID) > 128 || strings.ContainsAny(req.ID, "/\\ \t\r\n") || req.ID == "." || req.ID == ".." {
			return nil, &CallError{Code: "invalid_arguments", Message: "valid monitor ID required"}
		}
	}
	h, ok := s.Hub.(interface {
		CertificateRead(context.Context, string) (any, error)
	})
	if !ok {
		return nil, &CallError{Code: "not_configured", Message: "certificate client unavailable"}
	}
	v, err := h.CertificateRead(ctx, req.ID)
	return v, hubErr(err)
}
