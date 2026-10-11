package operatorclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func (c *Client) supervisionRead(ctx context.Context, path string, result any) error {
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return err
	}
	if response.status != http.StatusOK {
		return fmt.Errorf("operator client: supervision returned HTTP %d", response.status)
	}
	if err := validateJobReadHeaders(response.header); err != nil {
		return err
	}
	return decodeStrictJSONDocument(response.body, "supervision", result)
}
func (c *Client) JobsSummary(ctx context.Context, f operator.SupervisionFilter) (operator.JobsSummary, error) {
	r := operator.JobsSummary{}
	if err := operator.ValidateSupervisionFilter(f); err != nil {
		return r, err
	}
	q := url.Values{}
	if f.Kind != "" {
		q.Set("kind", f.Kind)
	}
	if f.MachineID != "" {
		q.Set("machine_id", f.MachineID)
	}
	if f.Window != "" {
		q.Set("window", f.Window)
	}
	if f.PerMachine {
		q.Set("per_machine", "true")
	}
	path := "/v1/operator/jobs-summary"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	err := c.supervisionRead(ctx, path, &r)
	return r, err
}
func (c *Client) ApprovalsSummary(ctx context.Context, window string) (operator.ApprovalsSummary, error) {
	r := operator.ApprovalsSummary{}
	if err := operator.ValidateSupervisionFilter(operator.SupervisionFilter{Window: window}); err != nil {
		return r, err
	}
	path := "/v1/operator/approvals-summary"
	if window != "" {
		path += "?window=" + url.QueryEscape(window)
	}
	err := c.supervisionRead(ctx, path, &r)
	return r, err
}
func (c *Client) HubStatus(ctx context.Context) (operator.HubStatus, error) {
	r := operator.HubStatus{}
	err := c.supervisionRead(ctx, "/v1/operator/hub-status", &r)
	return r, err
}
