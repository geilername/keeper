package lightroom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// HTTP talks to the Adobe-shaped API of cmd/lightroom-mock (paths as in
// lr.adobe.io/v2). It is safe for concurrent use.
type HTTP struct {
	baseURL   string
	catalogID string
	timeout   time.Duration
	client    *http.Client
}

var _ Client = (*HTTP)(nil)

// NewHTTP returns a client for baseURL (e.g. http://lightroom-mock:8080).
// timeout bounds each call, including reading the response. Every call is
// a CLIENT span (otelhttp) under the caller's span, and the W3C traceparent
// goes out with the request.
func NewHTTP(baseURL, catalogID string, timeout time.Duration) *HTTP {
	return &HTTP{
		baseURL:   strings.TrimRight(baseURL, "/"),
		catalogID: catalogID,
		timeout:   timeout,
		client: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport,
				// Semconv client span name: "{method} {url.template}".
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					if t, ok := r.Context().Value(templateKey{}).(string); ok {
						return r.Method + " " + t
					}
					return r.Method
				})),
		},
	}
}

type templateKey struct{}

const assetTemplate = "/v2/catalogs/{catalog_id}/assets/{asset_id}"

func (c *HTTP) assetURL(assetID, suffix string) string {
	return c.baseURL + "/v2/catalogs/" + url.PathEscape(c.catalogID) +
		"/assets/" + url.PathEscape(assetID) + suffix
}

// assetResource is the JSON shape of a Lightroom asset (the fields we use).
type assetResource struct {
	ID      string `json:"id"`
	Subtype string `json:"subtype,omitempty"`
	Payload struct {
		ImportSource struct {
			FileName string `json:"fileName,omitempty"`
			SHA256   string `json:"sha256,omitempty"`
		} `json:"importSource"`
		Master *struct {
			SHA256 string `json:"sha256"`
		} `json:"master,omitempty"`
	} `json:"payload"`
}

func (r assetResource) asset() Asset {
	a := Asset{ID: r.ID, SHA256: r.Payload.ImportSource.SHA256}
	if r.Payload.Master != nil {
		a.MasterSHA256 = r.Payload.Master.SHA256
	}
	return a
}

func (c *HTTP) CreateAsset(ctx context.Context, a NewAsset) (Asset, bool, error) {
	var body assetResource
	body.Subtype = "image"
	body.Payload.ImportSource.FileName = a.FileName
	body.Payload.ImportSource.SHA256 = a.SHA256
	data, err := json.Marshal(body)
	if err != nil {
		return Asset{}, false, err
	}

	var res assetResource
	status, err := c.do(ctx, "create_asset", http.MethodPut, assetTemplate, c.assetURL(a.ID, ""),
		"application/json", bytes.NewReader(data), int64(len(data)), &res)
	if err != nil {
		return Asset{}, false, err
	}
	return res.asset(), status == http.StatusCreated, nil
}

func (c *HTTP) UploadMaster(ctx context.Context, assetID, contentType string, body io.Reader, size int64) (bool, error) {
	status, err := c.do(ctx, "upload_master", http.MethodPut, assetTemplate+"/master",
		c.assetURL(assetID, "/master"), contentType, body, size, nil)
	return status == http.StatusCreated, err
}

func (c *HTTP) SetRating(ctx context.Context, assetID string, rating int) error {
	data, err := json.Marshal(map[string]int{"rating": rating})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, "set_rating", http.MethodPatch, assetTemplate, c.assetURL(assetID, ""),
		"application/json", bytes.NewReader(data), int64(len(data)), nil)
	return err
}

// do sends one request with the per-call timeout and decodes a 2xx JSON
// body into out (if non-nil). Non-2xx answers become *StatusError.
func (c *HTTP) do(ctx context.Context, op, method, template, u, contentType string,
	body io.Reader, size int64, out any,
) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, templateKey{}, template)

	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("lightroom %s: %w", op, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e struct {
			Description string `json:"description"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		return resp.StatusCode, &StatusError{Op: op, StatusCode: resp.StatusCode, Description: e.Description}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			if ctx.Err() != nil { // timed out (or canceled) while reading
				return resp.StatusCode, fmt.Errorf("lightroom %s: %w", op, ctx.Err())
			}
			// A 2xx with a body we cannot read is the server's fault (or a
			// cut connection); report it like a bad gateway so it is retried.
			return resp.StatusCode, &StatusError{Op: op, StatusCode: http.StatusBadGateway,
				Description: "invalid response body: " + err.Error()}
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body) // let the connection be reused
	return resp.StatusCode, nil
}
