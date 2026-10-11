package operatorclient

import (
	"context"
	"errors"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"github.com/teddashh/AI-Intune/internal/store"
	"net/http"
	"slices"
	"sort"
)

func (c *Client) ScriptCatalog(ctx context.Context) ([]scriptcatalog.Entry, error) {
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/script-catalog", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.doRaw(req)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, errors.New("script catalog request failed")
	}
	if err = validateSettingReadHeaders(resp.header); err != nil {
		return nil, err
	}
	return decodeScriptCatalog(resp.body)
}
func (c *Client) ScriptRun(ctx context.Context, body store.ScriptRunRequest, apply bool, key string) (store.ScriptRunResult, error) {
	var out store.ScriptRunResult
	path := "/v1/operator/script-runs/preview"
	if apply {
		path = "/v1/operator/script-runs"
		if !validSettingIdempotencyKey(key) || !validSHA256Digest(body.PreviewDigest) {
			return out, errors.New("invalid script apply coordinates")
		}
	}
	resp, err := c.postSettingJSON(ctx, path, key, body)
	if err != nil {
		return out, err
	}
	replayed := false
	if apply {
		replayed, err = validateSettingWriteHeaders(resp)
	} else {
		if resp.status != 200 {
			return out, errors.New("script preview request failed")
		}
		err = validateSettingReadHeaders(resp.header)
	}
	if err != nil {
		return out, err
	}
	err = decodeStrictJSONDocument(resp.body, "script run", &out)
	if err != nil {
		return out, err
	}
	targets := append([]string(nil), body.Targets...)
	sort.Strings(targets)
	if !validSHA256Digest(out.PreviewDigest) || !slices.Equal(out.Targets, targets) || out.Replayed != replayed {
		return out, errors.New("script response coordinates mismatch")
	}
	if apply {
		if out.PreviewDigest != body.PreviewDigest || len(out.JobIDs) != len(out.Targets) || (resp.status == http.StatusCreated) == out.Replayed {
			return out, errors.New("script apply receipt mismatch")
		}
	} else if len(out.JobIDs) != 0 || out.Replayed {
		return out, errors.New("script preview must not create jobs")
	}
	return out, nil
}

func decodeScriptCatalog(raw []byte) ([]scriptcatalog.Entry, error) {
	var out scriptcatalog.CatalogResponse
	if err := decodeStrictJSONDocument(raw, "script catalog", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
