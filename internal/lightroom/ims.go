package lightroom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultIMSURL is Adobe's identity service.
const DefaultIMSURL = "https://ims-na1.adobelogin.com"

// Scopes the Lightroom Partner API needs (quickstart/oauth.md), plus
// offline_access for a refresh token.
var Scopes = []string{"openid", "AdobeID", "lr_partner_apis", "offline_access"}

// IMS is the Adobe IMS OAuth 2.0 authorization-code client
// (authorize/v2, token/v3).
type IMS struct {
	BaseURL      string // DefaultIMSURL in production
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Client       *http.Client // nil: a 10 s client
	Now          func() time.Time
}

// TokenSet is what IMS hands out.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time // of the access token
}

// AuthURL is where the browser goes to sign in; IMS comes back to
// RedirectURI with ?code=…&state=….
func (i *IMS) AuthURL(state string) string {
	q := url.Values{
		"client_id":     {i.ClientID},
		"redirect_uri":  {i.RedirectURI},
		"scope":         {strings.Join(Scopes, ",")},
		"response_type": {"code"},
		"state":         {state},
	}
	return strings.TrimRight(i.BaseURL, "/") + "/ims/authorize/v2?" + q.Encode()
}

// Exchange trades an authorization code for tokens.
func (i *IMS) Exchange(ctx context.Context, code string) (TokenSet, error) {
	return i.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}})
}

// Refresh gets a new access token. IMS may or may not rotate the refresh
// token; when it does not, the old one is kept.
func (i *IMS) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	ts, err := i.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
	if err == nil && ts.RefreshToken == "" {
		ts.RefreshToken = refreshToken
	}
	return ts, err
}

// IMSError is an error answer from the token endpoint.
type IMSError struct {
	StatusCode  int
	Code        string // "invalid_grant", …
	Description string
}

func (e *IMSError) Error() string {
	return fmt.Sprintf("adobe ims: HTTP %d: %s %s", e.StatusCode, e.Code, e.Description)
}

// Revoked says the grant is gone (refresh token expired or revoked): the
// user has to connect again.
func (e *IMSError) Revoked() bool {
	return e.Code == "invalid_grant" || e.Code == "invalid_token" ||
		e.StatusCode == http.StatusBadRequest || e.StatusCode == http.StatusUnauthorized
}

func (i *IMS) token(ctx context.Context, form url.Values) (TokenSet, error) {
	form.Set("client_id", i.ClientID)
	form.Set("client_secret", i.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(i.BaseURL, "/")+"/ims/token/v3", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := i.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("adobe ims: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenSet{}, fmt.Errorf("adobe ims: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &e)
		return TokenSet{}, &IMSError{StatusCode: resp.StatusCode, Code: e.Error, Description: e.Description}
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return TokenSet{}, fmt.Errorf("adobe ims: token response: %w", err)
	}
	if t.AccessToken == "" {
		return TokenSet{}, errors.New("adobe ims: token response without access_token")
	}
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	return TokenSet{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    now().Add(expiresIn(t.ExpiresIn)).UTC(),
	}, nil
}

// expiresIn reads IMS's expires_in: seconds on token/v3, milliseconds on the
// older token/v1. Anything above 30 days in seconds must be milliseconds.
func expiresIn(n int64) time.Duration {
	switch {
	case n <= 0:
		return time.Hour // unknown: assume short, refresh early
	case n > 30*24*3600:
		return time.Duration(n) * time.Millisecond
	default:
		return time.Duration(n) * time.Second
	}
}
