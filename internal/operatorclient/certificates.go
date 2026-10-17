package operatorclient

import (
	"context"
	"errors"
	"github.com/teddashh/AI-Intune/internal/certificates"
	"net/http"
	"net/url"
)

func (c *Client) CertificateRead(ctx context.Context, id string) (any, error) {
	path := "/v1/operator/certificates"
	if id != "" {
		path += "/" + url.PathEscape(id)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	r, err := c.doRaw(req)
	if err != nil {
		return nil, err
	}
	if r.status != 200 {
		return nil, errors.New("certificate read failed")
	}
	if err = validateSettingReadHeaders(r.header); err != nil {
		return nil, err
	}
	if id != "" {
		var out certificates.Status
		err = decodeStrictJSONDocument(r.body, "certificate", &out)
		return out, err
	}
	var out struct {
		Items []certificates.Status `json:"items"`
	}
	err = decodeStrictJSONDocument(r.body, "certificates", &out)
	return out.Items, err
}
