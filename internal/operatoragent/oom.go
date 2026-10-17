package operatoragent

import (
	"context"
	"encoding/json"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func (s *Service) machineOOMKills(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MachineID string `json:"machine_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireID("machine_id", args.MachineID); err != nil {
		return nil, err
	}
	h, ok := s.Hub.(interface {
		MachineOOMKills(context.Context, string) (operator.OOMKillResult, error)
	})
	if !ok {
		return nil, &CallError{Code: "not_configured", Message: "OOM client unavailable"}
	}
	result, err := h.MachineOOMKills(ctx, args.MachineID)
	return result, hubErr(err)
}
