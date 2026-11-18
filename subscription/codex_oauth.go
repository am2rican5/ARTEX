// OAuth protocol constants and profile projection in this file are adapted
// from oh-my-pi commit 1bdbf6d242d99ca62bdf2feeff4519ff992f647d.
//
// MIT License
//
// Copyright (c) 2025 Mario Zechner
// Copyright (c) 2025-2026 Can Bölük
// Copyright (c) 2026 Stencil Labs, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package subscription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	codexOAuthClientID         = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthIssuer           = "https://auth.openai.com"
	codexOAuthAuthorizeURL     = codexOAuthIssuer + "/oauth/authorize"
	codexOAuthTokenURL         = codexOAuthIssuer + "/oauth/token"
	codexOAuthRedirectURI      = "http://localhost:1455/auth/callback"
	codexOAuthCallbackPort     = 1455
	codexOAuthCallbackPath     = "/auth/callback"
	codexOAuthScope            = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	codexOAuthRequestTimeout   = 15 * time.Second
	codexOAuthTokenBodyLimit   = 1 << 20
	codexOAuthMaxExpirySeconds = float64((1<<63 - 1) / int64(time.Second))

	codexOAuthAuthClaim    = "https://api.openai.com/auth"
	codexOAuthProfileClaim = "https://api.openai.com/profile"
)

var codexOAuthScopes = []string{
	"openid",
	"profile",
	"email",
	"offline_access",
	"api.connectors.read",
	"api.connectors.invoke",
}

type codexOAuthDriver struct {
	client *http.Client
}

var _ OAuthDriver = (*codexOAuthDriver)(nil)

// NewCodexOAuth returns the fixed legacy Codex browser OAuth driver. The
// supplied client contributes only its transport, which permits controlled
// proxying and test interception without sharing cookies, redirect policy, or
// an unbounded timeout with model traffic.
func NewCodexOAuth(client *http.Client) OAuthDriver {
	transport := http.DefaultTransport
	if client != nil && client.Transport != nil {
		transport = client.Transport
	}
	return &codexOAuthDriver{client: &http.Client{
		Transport:     transport,
		CheckRedirect: codexOAuthRejectRedirect,
		Timeout:       codexOAuthRequestTimeout,
	}}
}

func (*codexOAuthDriver) Kind() string { return ProviderCodex }

func (*codexOAuthDriver) Callback() CallbackConfig {
	return CallbackConfig{
		RedirectURI: codexOAuthRedirectURI,
		Port:        codexOAuthCallbackPort,
		Path:        codexOAuthCallbackPath,
	}
}

func (*codexOAuthDriver) AuthorizationURL(state, challenge string) string {
	values := url.Values{
		"response_type":              {"code"},
		"client_id":                  {codexOAuthClientID},
		"redirect_uri":               {codexOAuthRedirectURI},
		"scope":                      {codexOAuthScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"omp"},
	}
	return codexOAuthAuthorizeURL + "?" + values.Encode()
}

func (driver *codexOAuthDriver) Exchange(ctx context.Context, input CodeExchange) (TokenSet, error) {
	if ctx == nil || ctx.Err() != nil {
		return TokenSet{}, codexOAuthFailure(false, 0, "Codex authorization was canceled")
	}
	if input.Code == "" || input.Verifier == "" || input.RedirectURI != codexOAuthRedirectURI {
		return TokenSet{}, codexOAuthFailure(false, 0, "Codex authorization response was invalid")
	}
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexOAuthClientID},
		"code":          {input.Code},
		"code_verifier": {input.Verifier},
		"redirect_uri":  {codexOAuthRedirectURI},
	}
	response, err := driver.codexOAuthTokenRequest(ctx, values, false)
	if err != nil {
		return TokenSet{}, err
	}
	return codexOAuthProjectToken(response, TokenSet{}, false)
}

func (driver *codexOAuthDriver) Refresh(ctx context.Context, old TokenSet) (TokenSet, error) {
	if ctx.Err() != nil {
		return TokenSet{}, codexOAuthFailure(true, 0, "Codex credential refresh was canceled")
	}
	if old.RefreshToken == "" {
		return TokenSet{}, newError(CodeAuthRequired, "Codex credential requires authorization")
	}
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {codexOAuthClientID},
		"refresh_token": {old.RefreshToken},
	}
	response, err := driver.codexOAuthTokenRequest(ctx, values, true)
	if err != nil {
		return TokenSet{}, err
	}
	return codexOAuthProjectToken(response, old, true)
}

func (*codexOAuthDriver) Revoke(context.Context, TokenSet) error {
	return ErrRevocationUnsupported
}

func codexOAuthRejectRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type codexOAuthTokenResponse struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	IDToken      string          `json:"id_token"`
	ExpiresIn    *float64        `json:"expires_in"`
	Scope        string          `json:"scope"`
	Error        json.RawMessage `json:"error"`
}

func (driver *codexOAuthDriver) codexOAuthTokenRequest(
	ctx context.Context,
	values url.Values,
	refresh bool,
) (codexOAuthTokenResponse, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		codexOAuthTokenURL,
		strings.NewReader(values.Encode()),
	)
	if err != nil {
		return codexOAuthTokenResponse{}, codexOAuthFailure(refresh, 0, "Codex token request could not be created")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := driver.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return codexOAuthTokenResponse{}, codexOAuthFailure(refresh, 0, "Codex token request was canceled")
		}
		return codexOAuthTokenResponse{}, codexOAuthFailure(refresh, 0, "Codex token endpoint was unavailable")
	}
	defer response.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(response.Body, codexOAuthTokenBodyLimit+1))
	if readErr != nil || len(body) > codexOAuthTokenBodyLimit {
		return codexOAuthTokenResponse{}, codexOAuthFailure(refresh, response.StatusCode, "Codex token response could not be read")
	}
	var tokenResponse codexOAuthTokenResponse
	decodeErr := json.Unmarshal(body, &tokenResponse)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if refresh && codexOAuthRequiresAuthorization(response.StatusCode, tokenResponse.Error) {
			return codexOAuthTokenResponse{}, &Error{
				Code:    CodeAuthRequired,
				Message: "Codex credential requires authorization",
				Status:  response.StatusCode,
			}
		}
		return codexOAuthTokenResponse{}, codexOAuthFailure(refresh, response.StatusCode, "Codex token endpoint rejected the request")
	}
	if decodeErr != nil {
		return codexOAuthTokenResponse{}, codexOAuthFailure(refresh, response.StatusCode, "Codex token response was invalid")
	}
	return tokenResponse, nil
}

func codexOAuthRequiresAuthorization(status int, raw json.RawMessage) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	var code string
	if len(raw) == 0 || json.Unmarshal(raw, &code) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_grant", "invalid_token", "unauthorized_client":
		return true
	default:
		return false
	}
}

func codexOAuthFailure(refresh bool, status int, message string) *Error {
	code := CodeAuthorizationFailed
	if refresh {
		code = CodeRefreshFailed
	}
	return &Error{Code: code, Message: message, Status: status}
}

type codexOAuthJWTClaims struct {
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	Auth    struct {
		AccountID string `json:"chatgpt_account_id"`
		PlanType  string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
	Profile struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
}

type codexOAuthProfile struct {
	accountID string
	email     string
	planType  string
	subject   string
}

// codexOAuthDecodeClaims decodes legacy JWT payload metadata only. It does not
// verify a signature and must not be used as an OIDC authorization decision.
func codexOAuthDecodeClaims(token string) (codexOAuthJWTClaims, bool) {
	var claims codexOAuthJWTClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return claims, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return codexOAuthJWTClaims{}, false
	}
	return claims, true
}

func codexOAuthTokenProfile(accessToken, idToken string) codexOAuthProfile {
	access, _ := codexOAuthDecodeClaims(accessToken)
	identity, _ := codexOAuthDecodeClaims(idToken)

	accountID := strings.TrimSpace(access.Auth.AccountID)
	if accountID == "" {
		accountID = strings.TrimSpace(identity.Auth.AccountID)
	}
	email := access.Profile.Email
	if email == "" {
		email = identity.Profile.Email
	}
	planType := access.Auth.PlanType
	if planType == "" {
		planType = identity.Auth.PlanType
	}
	subject := strings.TrimSpace(identity.Subject)
	if subject == "" {
		subject = strings.TrimSpace(access.Subject)
	}

	return codexOAuthProfile{
		accountID: accountID,
		email:     strings.ToLower(strings.TrimSpace(email)),
		planType:  strings.ToLower(strings.TrimSpace(planType)),
		subject:   subject,
	}
}

func codexOAuthProjectToken(
	response codexOAuthTokenResponse,
	old TokenSet,
	refresh bool,
) (TokenSet, error) {
	accessToken := strings.TrimSpace(response.AccessToken)
	refreshToken := strings.TrimSpace(response.RefreshToken)
	idToken := strings.TrimSpace(response.IDToken)
	if accessToken == "" || response.ExpiresIn == nil ||
		math.IsNaN(*response.ExpiresIn) || math.IsInf(*response.ExpiresIn, 0) ||
		*response.ExpiresIn <= 0 || *response.ExpiresIn > codexOAuthMaxExpirySeconds {
		return TokenSet{}, codexOAuthFailure(refresh, 0, "Codex token response was missing required fields")
	}
	if !refresh && refreshToken == "" {
		return TokenSet{}, codexOAuthFailure(false, 0, "Codex token response was missing required fields")
	}

	expiresAfter := time.Duration(*response.ExpiresIn * float64(time.Second))
	if expiresAfter <= 0 {
		return TokenSet{}, codexOAuthFailure(refresh, 0, "Codex token response contained an invalid expiry")
	}
	profileToken := idToken
	if refresh && profileToken == "" {
		profileToken = old.IDToken
	}
	profile := codexOAuthTokenProfile(accessToken, profileToken)
	if !refresh && profile.accountID == "" {
		return TokenSet{}, codexOAuthFailure(false, 0, "Codex token response did not identify a ChatGPT account")
	}

	scopes := codexOAuthResponseScopes(response.Scope)
	if !refresh && len(scopes) == 0 {
		scopes = append([]string(nil), codexOAuthScopes...)
	}
	tokens := TokenSet{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Add(expiresAfter),
		AccountID:    profile.accountID,
		Email:        profile.email,
		ClientID:     codexOAuthClientID,
		Issuer:       codexOAuthIssuer,
		Subject:      profile.subject,
		IDToken:      idToken,
		Scopes:       scopes,
	}
	if !refresh {
		tokens.OrgID = profile.accountID
		tokens.OrgName = profile.planType
		return tokens, nil
	}

	if tokens.RefreshToken == "" {
		tokens.RefreshToken = old.RefreshToken
	}
	if tokens.IDToken == "" {
		tokens.IDToken = old.IDToken
	}
	if tokens.AccountID == "" {
		tokens.AccountID = old.AccountID
	}
	if tokens.Email == "" {
		tokens.Email = old.Email
	}
	if tokens.Subject == "" {
		tokens.Subject = old.Subject
	}
	tokens.OrgID = old.OrgID
	tokens.OrgName = old.OrgName
	tokens.AccountID = strings.TrimSpace(tokens.AccountID)
	if tokens.AccountID == "" {
		return TokenSet{}, codexOAuthFailure(true, 0, "Codex token response did not identify a ChatGPT account")
	}
	if len(tokens.Scopes) == 0 {
		if len(old.Scopes) > 0 {
			tokens.Scopes = append([]string(nil), old.Scopes...)
		} else {
			tokens.Scopes = append([]string(nil), codexOAuthScopes...)
		}
	}
	return tokens, nil
}

func codexOAuthResponseScopes(raw string) []string {
	return strings.Fields(raw)
}
