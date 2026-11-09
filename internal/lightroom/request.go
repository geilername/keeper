package lightroom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// caller is the request plumbing HTTP and Adobe share: a per-call timeout,
// a CLIENT span per call named after the URL template, optional auth
// headers, the while(1){} prefix stripped, and non-2xx answers turned into
// *StatusError.
type caller struct {
	client  *http.Client
	timeout time.Duration
	// uploadTimeout, if set, bounds upload_master instead of timeout: a
	// 60 MB raw over a home uplink takes minutes.
	uploadTimeout time.Duration
	// auth sets per-request headers (Adobe: X-API-Key and the bearer
	// token). An error ends the call before anything is sent.
	auth func(ctx context.Context, h http.Header) error
}

type templateKey struct{}

// tracedClient makes every call a CLIENT span (otelhttp) under the caller's
// span; the W3C traceparent goes out with the request.
func tracedClient() *http.Client {
	return &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport,
			// Semconv client span name: "{method} {url.template}".
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if t, ok := r.Context().Value(templateKey{}).(string); ok {
					return r.Method + " " + t
				}
				return r.Method
			})),
	}
}

// maxJSON bounds a JSON response body; album and asset pages are far smaller.
const maxJSON = 16 << 20

// while1 is the anti-JSON-hijacking prefix Adobe puts before every JSON
// body (usage/calling.md); the same pattern Adobe's sample code strips.
var while1 = regexp.MustCompile(`^\s*while\s*\(\s*1\s*\)\s*\{\s*\}\s*`)

// stripPrefix removes Adobe's while(1){} prefix, if present.
func stripPrefix(b []byte) []byte {
	if loc := while1.FindIndex(b); loc != nil {
		return b[loc[1]:]
	}
	return b
}

// do sends one request with the per-call timeout and decodes a 2xx JSON
// body into out (if non-nil; an empty body leaves out untouched). Non-2xx
// answers become *StatusError.
func (c *caller) do(ctx context.Context, op, method, template, u, contentType string,
	body io.Reader, size int64, out any,
) (int, error) {
	timeout := c.timeout
	if op == "upload_master" && c.uploadTimeout > 0 {
		timeout = c.uploadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx = context.WithValue(ctx, templateKey{}, template)

	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.ContentLength = size
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if c.auth != nil {
		if err := c.auth(ctx, req.Header); err != nil {
			return 0, fmt.Errorf("lightroom %s: %w", op, err)
		}
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("lightroom %s: %w", op, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, statusError(op, resp.StatusCode, stripPrefix(raw))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body) // let the connection be reused
		return resp.StatusCode, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON))
	if err == nil {
		raw = bytes.TrimSpace(stripPrefix(raw))
		if len(raw) > 0 {
			err = json.Unmarshal(raw, out)
		}
	}
	if err != nil {
		if ctx.Err() != nil { // timed out (or canceled) while reading
			return resp.StatusCode, fmt.Errorf("lightroom %s: %w", op, ctx.Err())
		}
		// A 2xx with a body we cannot read is the server's fault (or a cut
		// connection); report it like a bad gateway so it is retried.
		return resp.StatusCode, &StatusError{Op: op, StatusCode: http.StatusBadGateway,
			Description: "invalid response body: " + err.Error()}
	}
	return resp.StatusCode, nil
}

// statusError reads both error shapes: the mock's and Adobe's
// {"code":4300,"description":…}, Adobe's {"error_code":"403003","message":…}
// and per-field {"errors":{"asset":["already exists"]}}.
func statusError(op string, status int, body []byte) *StatusError {
	se := &StatusError{Op: op, StatusCode: status, body: body}
	var e struct {
		Code        json.RawMessage `json:"code"`
		ErrorCode   json.RawMessage `json:"error_code"`
		Description string          `json:"description"`
		Message     string          `json:"message"`
		Errors      json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(body, &e) != nil {
		return se
	}
	se.Description = e.Description
	if se.Description == "" {
		se.Description = e.Message
	}
	se.Code = rawString(e.Code)
	if se.Code == "" {
		se.Code = rawString(e.ErrorCode)
	}
	var fields map[string][]string
	if json.Unmarshal(e.Errors, &fields) == nil {
		se.Errors = fields
	}
	return se
}

// rawString turns 4300 or "4300" into "4300".
func rawString(r json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(r)), `"`)
}
