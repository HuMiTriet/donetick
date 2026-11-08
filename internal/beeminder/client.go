package beeminder

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

	"donetick.com/core/config"
)

// ErrInvalidToken means Beeminder rejected the auth token (HTTP 401).
var ErrInvalidToken = errors.New("beeminder rejected the auth token")

// APIError is a non-2xx response from Beeminder other than 401.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("beeminder returned HTTP %d: %s", e.Status, e.Body)
}

// User is the subset of Beeminder's user resource Donetick needs.
type User struct {
	Username string `json:"username"`
	Deadbeat bool   `json:"deadbeat"`
}

// Client talks to the Beeminder API. The auth token is sent as the
// auth_token parameter, as Beeminder's docs require, and is never written
// into an error or log line.
type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(cfg *config.Config) *Client {
	timeout := time.Duration(cfg.Beeminder.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	baseURL := cfg.Beeminder.BaseURL
	if baseURL == "" {
		baseURL = "https://www.beeminder.com/api/v1"
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Me returns the Beeminder user that owns token.
func (c *Client) GetBeeminderUserInfo(ctx context.Context, token string) (*User, error) {
	query := url.Values{"auth_token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/users/me.json?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var user User
	if err := c.do(req, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// Charge charges beeminderUser amountCents through Beeminder and returns the
// charge id, or "" when Beeminder doesn't send a readable one. With dryRun the
// charge is simulated: Beeminder returns the charge JSON without creating it.
func (c *Client) Charge(ctx context.Context, token, beeminderUser string, amountCents int, note string, dryRun bool) (string, error) {
	form := url.Values{
		"auth_token": {token},
		"user_id":    {beeminderUser},
		"amount":     {FormatDollars(amountCents)},
		"note":       {note},
	}
	if dryRun {
		form.Set("dryrun", "true")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/charges.json", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var body map[string]any
	if err := c.do(req, &body); err != nil {
		var decodeErr *decodeError
		if !errors.As(err, &decodeErr) {
			return "", err
		}
		// Beeminder answered 2xx, so the money moved even though the body
		// was unreadable. Report success so the charge is recorded and not
		// retried (which would charge twice).
		return "", nil
	}
	// Any 2xx is a success, so a missing or non-string id must not turn into
	// an error either.
	id, _ := body["id"].(string)
	return id, nil
}

// decodeError means a 2xx response body could not be decoded.
type decodeError struct{ err error }

func (e *decodeError) Error() string { return "decoding beeminder response: " + e.err.Error() }
func (e *decodeError) Unwrap() error { return e.err }

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error repeats the request URL, which carries the auth token
		// for GET requests. Keep only the underlying cause.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("beeminder request failed: %w", urlErr.Err)
		}
		return fmt.Errorf("beeminder request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("reading beeminder response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrInvalidToken
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text := strings.TrimSpace(string(body))
		if len(text) > 300 {
			text = text[:300]
		}
		return &APIError{Status: resp.StatusCode, Body: text}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &decodeError{err: err}
	}
	return nil
}

// FormatDollars renders cents as a dollar amount with two decimals, e.g.
// 550 -> "5.50".
func FormatDollars(cents int) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
