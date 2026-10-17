package operatorclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func (c *Client) MachineOOMKills(ctx context.Context, id string) (operator.OOMKillResult, error) {
	var out operator.OOMKillResult
	req, err := c.newMachineOperatorRequest(ctx, http.MethodGet, id, "/oom-kills", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRawWithLimit(req, 64<<10, "64 KiB")
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: OOM read returned HTTP %d", response.status)
	}
	if err = validateMachineReadHeaders(response.header); err != nil {
		return out, err
	}
	if err = decodeStrictJSONDocument(response.body, "OOM kills", &out); err != nil {
		return out, err
	}
	if out.SchemaVersion != 1 || out.EvaluatedAt.IsZero() || out.Items == nil || len(out.Items) > 64 {
		return operator.OOMKillResult{}, fmt.Errorf("operator client: invalid OOM response")
	}
	for _, e := range out.Items {
		if !model.ValidOOM(e, out.EvaluatedAt) {
			return operator.OOMKillResult{}, fmt.Errorf("operator client: invalid OOM event")
		}
	}
	return out, nil
}
