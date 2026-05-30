// Package weftclient — device.go implements the OAuth 2.0 Device
// Authorization Grant (RFC 8628) used by `weft login` to obtain a
// token from dex without a callback URL. The flow is:
//
//   1. POST <issuer>/device/code with client_id + scope
//      → returns device_code, user_code, verification_uri,
//        verification_uri_complete, expires_in, interval.
//   2. Display user_code + verification_uri to the operator.
//      They open the URL in a browser and authenticate.
//   3. Poll <issuer>/token with grant_type=urn:ietf:params:
//      oauth:grant-type:device_code + device_code, every
//      `interval` seconds. dex returns the access_token (plus
//      id_token if openid was in scope) once authorisation lands.
//
// We keep this dependency-light: stdlib net/http for the requests,
// stdlib encoding/json for the OIDC responses (the dex wire format
// is JSON regardless of our HCL preference; HCL is for config we
// own).
package weftclient

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

	"golang.org/x/oauth2"
)

// DeviceAuthResponse mirrors the RFC 8628 server response.
type DeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceAuth starts the device flow. Returns the server response;
// the caller is expected to display it to the user and then call
// PollDeviceToken.
//
// Scopes default to `openid profile email groups` — matches dex's
// canonical claim set for our use case.
func DeviceAuth(ctx context.Context, issuer, clientID string, scopes []string) (*DeviceAuthResponse, error) {
	if issuer == "" || clientID == "" {
		return nil, errors.New("device auth: issuer and client_id are required")
	}
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "groups", "offline_access"}
	}
	form := url.Values{
		"client_id": {clientID},
		"scope":     {strings.Join(scopes, " ")},
	}
	endpoint := strings.TrimRight(issuer, "/") + "/device/code"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("device auth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device auth: %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device auth: %s: %s — %s", endpoint, resp.Status, string(body))
	}
	var out DeviceAuthResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("device auth: decode response: %w (body=%s)", err, string(body))
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return &out, nil
}

// PollDeviceToken polls the token endpoint until the user
// authorises (or the device code expires / ctx is cancelled).
// Returns the oauth2.Token plus the raw `id_token` string (if
// dex issued one — depends on whether `openid` was in scope).
//
// Retryable RFC 8628 errors (`authorization_pending`, `slow_down`)
// are handled inline; everything else surfaces immediately.
func PollDeviceToken(ctx context.Context, issuer, clientID string, da *DeviceAuthResponse) (*oauth2.Token, string, error) {
	endpoint := strings.TrimRight(issuer, "/") + "/token"
	interval := time.Duration(da.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	if da.ExpiresIn == 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for {
		if time.Now().After(deadline) {
			return nil, "", errors.New("device flow: code expired (operator did not complete login in time)")
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(interval):
		}
		form := url.Values{
			"client_id":   {clientID},
			"device_code": {da.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, "", fmt.Errorf("poll token: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("poll token: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var raw struct {
				AccessToken  string `json:"access_token"`
				TokenType    string `json:"token_type"`
				RefreshToken string `json:"refresh_token"`
				IDToken      string `json:"id_token"`
				ExpiresIn    int    `json:"expires_in"`
			}
			if err := json.Unmarshal(body, &raw); err != nil {
				return nil, "", fmt.Errorf("poll token: decode success: %w", err)
			}
			tok := &oauth2.Token{
				AccessToken:  raw.AccessToken,
				TokenType:    raw.TokenType,
				RefreshToken: raw.RefreshToken,
				Expiry:       time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second),
			}
			return tok, raw.IDToken, nil
		}
		// RFC 8628 declares specific retryable errors via the body.
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errBody)
		switch errBody.Error {
		case "authorization_pending":
			// Keep polling at the current interval.
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "expired_token":
			return nil, "", errors.New("device flow: code expired before authorisation")
		case "access_denied":
			return nil, "", errors.New("device flow: user denied the request")
		default:
			return nil, "", fmt.Errorf("poll token: %s — %s", resp.Status, string(body))
		}
	}
}
