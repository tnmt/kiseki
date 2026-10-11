// Package withings fetches body measurements from the Withings API and stores
// them as raw records inside kiseki serve.
package withings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultAPIBase = "https://wbsapi.withings.net"
	authorizeURL   = "https://account.withings.com/oauth2_user/authorize2"
	scope          = "user.metrics"
)

// Withings answers HTTP 200 for most failures and puts the outcome in the
// body's status field.
const (
	statusOK          = 0
	statusRateLimited = 601
	// The token endpoint answers an invalid code or refresh token with 503.
	statusInvalidParams = 503
)

// invalidTokenStatuses mean the access token is no longer accepted.
var invalidTokenStatuses = []int{401, 293}

var (
	// ErrRateLimited means Withings refused the call with status 601. The next
	// scheduled run tries again.
	ErrRateLimited = errors.New("withings: rate limited (status 601)")
	// ErrAuthRequired means the refresh token was rejected and only a new
	// authorization with `kiseki withings auth` can recover.
	ErrAuthRequired        = errors.New("withings: refresh token rejected; run `kiseki withings auth` again")
	errAccessTokenRejected = errors.New("withings: access token rejected")
)

// APIError is a non-zero status in a Withings response body.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("withings: status %d: %s", e.Status, e.Message)
}

// App identifies the Withings developer application.
type App struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	APIBase      string
	HTTP         *http.Client
}

func (a *App) base() string {
	if a.APIBase != "" {
		return strings.TrimRight(a.APIBase, "/")
	}
	return DefaultAPIBase
}

func (a *App) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return http.DefaultClient
}

// AuthorizeURL is the page where the user grants access. Withings redirects
// to RedirectURI with a code that must be exchanged within 30 seconds.
func (a *App) AuthorizeURL(state string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {a.ClientID},
		"scope":         {scope},
		"redirect_uri":  {a.RedirectURI},
		"state":         {state},
	}
	return authorizeURL + "?" + q.Encode()
}

// Token is the OAuth state persisted between runs. Withings rotates the
// refresh token on every refresh, so the file must be rewritten each time.
type Token struct {
	UserID       string `json:"userid"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // Unix seconds
	Scope        string `json:"scope"`
}

func LoadToken(path string) (*Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("%s: no refresh token", path)
	}
	return &t, nil
}

// SaveToken replaces the file atomically. A torn write would lose the only
// valid refresh token and force a new authorization.
func SaveToken(path string, t *Token) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Exchange trades an authorization code for tokens.
func (a *App) Exchange(ctx context.Context, code string, now time.Time) (*Token, error) {
	return a.requestToken(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {a.RedirectURI},
	}, now)
}

// Refresh obtains a new access token. The returned token carries a new
// refresh token and the old one stops working.
func (a *App) Refresh(ctx context.Context, refreshToken string, now time.Time) (*Token, error) {
	return a.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}, now)
}

func (a *App) requestToken(ctx context.Context, form url.Values, now time.Time) (*Token, error) {
	form.Set("action", "requesttoken")
	form.Set("client_id", a.ClientID)
	form.Set("client_secret", a.ClientSecret)
	var body struct {
		UserID       json.Number `json:"userid"`
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    int64       `json:"expires_in"`
		Scope        string      `json:"scope"`
	}
	if err := a.call(ctx, "/v2/oauth2", "", form, &body); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == statusInvalidParams {
			return nil, ErrAuthRequired
		}
		return nil, err
	}
	if body.AccessToken == "" || body.RefreshToken == "" {
		return nil, errors.New("withings: token response without tokens")
	}
	return &Token{
		UserID:       body.UserID.String(),
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresAt:    now.Unix() + body.ExpiresIn,
		Scope:        body.Scope,
	}, nil
}

// MeasureGroup keeps the group as Withings sent it, plus the fields the
// collector needs to build a record.
type MeasureGroup struct {
	Raw      json.RawMessage
	GrpID    int64
	Date     int64 // Unix seconds
	Timezone string
}

func (g *MeasureGroup) UnmarshalJSON(b []byte) error {
	var fields struct {
		GrpID    int64  `json:"grpid"`
		Date     int64  `json:"date"`
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	g.Raw = append(json.RawMessage(nil), b...)
	g.GrpID, g.Date, g.Timezone = fields.GrpID, fields.Date, fields.Timezone
	return nil
}

// MeasurePage is one getmeas response.
type MeasurePage struct {
	Timezone string         `json:"timezone"`
	Groups   []MeasureGroup `json:"measuregrps"`
	More     int            `json:"more"`
	Offset   int            `json:"offset"`
}

// GetMeas returns the groups modified at or after lastUpdate (all groups when
// lastUpdate is zero), starting at offset.
func (a *App) GetMeas(ctx context.Context, accessToken string, lastUpdate int64, offset int) (*MeasurePage, error) {
	form := url.Values{
		"action":   {"getmeas"},
		"category": {"1"},
	}
	if lastUpdate > 0 {
		form.Set("lastupdate", strconv.FormatInt(lastUpdate, 10))
	}
	if offset > 0 {
		form.Set("offset", strconv.Itoa(offset))
	}
	var page MeasurePage
	if err := a.call(ctx, "/measure", accessToken, form, &page); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && slices.Contains(invalidTokenStatuses, apiErr.Status) {
			return nil, errAccessTokenRejected
		}
		return nil, err
	}
	return &page, nil
}

func (a *App) call(ctx context.Context, path, accessToken string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("withings: %s: HTTP %d", path, resp.StatusCode)
	}
	var env struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("withings: %s: %w", path, err)
	}
	switch env.Status {
	case statusOK:
		return json.Unmarshal(env.Body, out)
	case statusRateLimited:
		return ErrRateLimited
	default:
		return &APIError{Status: env.Status, Message: env.Error}
	}
}
