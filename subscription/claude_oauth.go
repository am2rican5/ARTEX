// OAuth protocol constants and identity projection in this file are adapted
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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	claudeOAuthClientID         = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthIssuer           = "https://claude.ai"
	claudeOAuthAuthorizeURL     = claudeOAuthIssuer + "/oauth/authorize"
	claudeOAuthTokenURL         = "https://api.anthropic.com/v1/oauth/token"
	claudeOAuthBootstrapURL     = "https://api.anthropic.com/api/claude_cli/bootstrap?entrypoint=cli&model=claude-opus-4-8"
	claudeOAuthRedirectURI      = "http://localhost:54545/callback"
	claudeOAuthCallbackPort     = 54545
	claudeOAuthCallbackPath     = "/callback"
	claudeOAuthRequestTimeout   = 30 * time.Second
	claudeOAuthExpirySkew       = 5 * time.Minute
	claudeOAuthTokenBodyLimit   = 1 << 20
	claudeOAuthMaxExpirySeconds = float64((1<<63 - 1) / int64(time.Second))
	claudeOAuthCLIVersion       = "2.1.321"
	claudeOAuthSDKVersion       = "0.128.0"
	claudeOAuthBeta             = "oauth-2025-04-20"
)

var claudeOAuthScopes = []string{
	"org:create_api_key",
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

type claudeOAuthDriver struct {
	client *http.Client
}

var _ OAuthDriver = (*claudeOAuthDriver)(nil)

// NewClaudeOAuth returns the fixed Anthropic browser OAuth driver. The
// supplied client contributes only its transport, allowing deliberate test or
// proxy interception without sharing cookies, redirect policy, or an unbounded
// timeout with model traffic.
func NewClaudeOAuth(client *http.Client) OAuthDriver {
	transport := http.DefaultTransport
	if client != nil && client.Transport != nil {
		transport = client.Transport
	}
	return &claudeOAuthDriver{client: &http.Client{
		Transport:     transport,
		CheckRedirect: claudeOAuthRejectRedirect,
		Timeout:       claudeOAuthRequestTimeout,
	}}
}

func (*claudeOAuthDriver) Kind() string { return ProviderClaude }

func (*claudeOAuthDriver) Callback() CallbackConfig {
	return CallbackConfig{
		RedirectURI: claudeOAuthRedirectURI,
		Port:        claudeOAuthCallbackPort,
		Path:        claudeOAuthCallbackPath,
	}
}

func (*claudeOAuthDriver) AuthorizationURL(state, challenge string) string {
	values := url.Values{
		"client_id":             {claudeOAuthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {claudeOAuthRedirectURI},
		"scope":                 {strings.Join(claudeOAuthScopes, " ")},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"code":                  {"true"},
	}
	return claudeOAuthAuthorizeURL + "?" + values.Encode()
}

func (driver *claudeOAuthDriver) Exchange(ctx context.Context, input CodeExchange) (TokenSet, error) {
	if ctx == nil || ctx.Err() != nil {
		return TokenSet{}, claudeOAuthFailure(false, 0, "Claude authorization was canceled")
	}
	if input.Code == "" || input.State == "" || input.Verifier == "" || input.RedirectURI != claudeOAuthRedirectURI {
		return TokenSet{}, claudeOAuthFailure(false, 0, "Claude authorization response was invalid")
	}
	requestBody := claudeOAuthExchangeRequest{
		GrantType:    "authorization_code",
		ClientID:     claudeOAuthClientID,
		Code:         input.Code,
		RedirectURI:  claudeOAuthRedirectURI,
		CodeVerifier: input.Verifier,
		State:        input.State,
	}
	response, err := driver.claudeOAuthTokenRequest(ctx, requestBody, false)
	if err != nil {
		return TokenSet{}, err
	}
	tokens, err := claudeOAuthProjectToken(response, TokenSet{}, false)
	if err != nil {
		return TokenSet{}, err
	}
	if err := driver.claudeOAuthEnrichIdentity(ctx, &tokens, true, false); err != nil {
		return TokenSet{}, err
	}
	if tokens.AccountID == "" || tokens.OrgID == "" {
		return TokenSet{}, claudeOAuthFailure(false, 0, "Claude did not confirm account and organization identity")
	}
	return tokens, nil
}

func (driver *claudeOAuthDriver) Refresh(ctx context.Context, old TokenSet) (TokenSet, error) {
	if ctx == nil || ctx.Err() != nil {
		return TokenSet{}, claudeOAuthFailure(true, 0, "Claude credential refresh was canceled")
	}
	if old.RefreshToken == "" {
		return TokenSet{}, newError(CodeAuthRequired, "Claude credential requires authorization")
	}
	if old.AccountID == "" || old.OrgID == "" {
		return TokenSet{}, newError(CodeCredentialInvalid, "Claude credential identity is incomplete")
	}
	if (old.ClientID != "" && !constantTimeEqual(old.ClientID, claudeOAuthClientID)) ||
		(old.Issuer != "" && !constantTimeEqual(old.Issuer, claudeOAuthIssuer)) {
		return TokenSet{}, newError(CodeIdentityChanged, "Claude credential client identity changed")
	}
	requestBody := claudeOAuthRefreshRequest{
		GrantType:    "refresh_token",
		ClientID:     claudeOAuthClientID,
		RefreshToken: old.RefreshToken,
	}
	response, err := driver.claudeOAuthTokenRequest(ctx, requestBody, true)
	if err != nil {
		return TokenSet{}, err
	}
	tokens, err := claudeOAuthProjectToken(response, old, true)
	if err != nil {
		return TokenSet{}, err
	}
	if err := driver.claudeOAuthEnrichIdentity(ctx, &tokens, false, true); err != nil {
		return TokenSet{}, err
	}
	if tokens.AccountID == "" {
		tokens.AccountID = old.AccountID
	}
	if !constantTimeEqual(tokens.AccountID, old.AccountID) {
		return TokenSet{}, newError(CodeIdentityChanged, "Claude account identity changed during refresh")
	}
	if tokens.Email == "" {
		tokens.Email = old.Email
	}
	tokens.OrgID = old.OrgID
	tokens.OrgName = old.OrgName
	return tokens, nil
}

func (*claudeOAuthDriver) Revoke(context.Context, TokenSet) error {
	return ErrRevocationUnsupported
}

func claudeOAuthRejectRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type claudeOAuthExchangeRequest struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier"`
	State        string `json:"state"`
}

type claudeOAuthRefreshRequest struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

type claudeOAuthTokenResponse struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresIn    *float64        `json:"expires_in"`
	Error        json.RawMessage `json:"error"`
	Account      struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

func (driver *claudeOAuthDriver) claudeOAuthTokenRequest(
	ctx context.Context,
	payload any,
	refresh bool,
) (claudeOAuthTokenResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return claudeOAuthTokenResponse{}, claudeOAuthFailure(refresh, 0, "Claude token request could not be created")
	}
	boundedCtx, cancel := context.WithTimeout(ctx, claudeOAuthRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		boundedCtx,
		http.MethodPost,
		claudeOAuthTokenURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return claudeOAuthTokenResponse{}, claudeOAuthFailure(refresh, 0, "Claude token request could not be created")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if refresh {
		request.Header.Set("anthropic-beta", claudeOAuthBeta)
		request.Header.Set("User-Agent", "anthropic-sdk-typescript/"+claudeOAuthSDKVersion+" userOAuthProvider")
	}
	response, err := driver.client.Do(request)
	if err != nil {
		if boundedCtx.Err() != nil {
			return claudeOAuthTokenResponse{}, claudeOAuthFailure(refresh, 0, "Claude token request was canceled or timed out")
		}
		return claudeOAuthTokenResponse{}, claudeOAuthFailure(refresh, 0, "Claude token endpoint was unavailable")
	}
	defer response.Body.Close()

	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, claudeOAuthTokenBodyLimit+1))
	if readErr != nil || len(responseBody) > claudeOAuthTokenBodyLimit {
		return claudeOAuthTokenResponse{}, claudeOAuthFailure(refresh, response.StatusCode, "Claude token response could not be read")
	}
	var tokenResponse claudeOAuthTokenResponse
	decodeErr := json.Unmarshal(responseBody, &tokenResponse)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if refresh && claudeOAuthRequiresAuthorization(response.StatusCode, tokenResponse.Error) {
			return claudeOAuthTokenResponse{}, &Error{
				Code:    CodeAuthRequired,
				Message: "Claude credential requires authorization",
				Status:  response.StatusCode,
			}
		}
		return claudeOAuthTokenResponse{}, claudeOAuthFailure(refresh, response.StatusCode, "Claude token endpoint rejected the request")
	}
	if decodeErr != nil {
		return claudeOAuthTokenResponse{}, claudeOAuthFailure(refresh, response.StatusCode, "Claude token response was invalid")
	}
	return tokenResponse, nil
}

func claudeOAuthRequiresAuthorization(status int, raw json.RawMessage) bool {
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

func claudeOAuthFailure(refresh bool, status int, message string) *Error {
	code := CodeAuthorizationFailed
	if refresh {
		code = CodeRefreshFailed
	}
	return &Error{Code: code, Message: message, Status: status}
}

func claudeOAuthProjectToken(
	response claudeOAuthTokenResponse,
	old TokenSet,
	refresh bool,
) (TokenSet, error) {
	accessToken := strings.TrimSpace(response.AccessToken)
	refreshToken := strings.TrimSpace(response.RefreshToken)
	if accessToken == "" || response.ExpiresIn == nil ||
		math.IsNaN(*response.ExpiresIn) || math.IsInf(*response.ExpiresIn, 0) ||
		*response.ExpiresIn <= 0 || *response.ExpiresIn > claudeOAuthMaxExpirySeconds {
		return TokenSet{}, claudeOAuthFailure(refresh, 0, "Claude token response was missing required fields")
	}
	expiresAfter := time.Duration(*response.ExpiresIn*float64(time.Second)) - claudeOAuthExpirySkew
	if expiresAfter <= 0 {
		return TokenSet{}, claudeOAuthFailure(refresh, 0, "Claude token response contained an invalid expiry")
	}
	tokens := TokenSet{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Add(expiresAfter),
		AccountID:    strings.TrimSpace(response.Account.UUID),
		OrgID:        strings.TrimSpace(response.Organization.UUID),
		OrgName:      strings.TrimSpace(response.Organization.Name),
		Email:        strings.TrimSpace(response.Account.EmailAddress),
		ClientID:     claudeOAuthClientID,
		Issuer:       claudeOAuthIssuer,
		Scopes:       append([]string(nil), claudeOAuthScopes...),
	}
	if !refresh {
		return tokens, nil
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = old.RefreshToken
	}
	tokens.OrgID = old.OrgID
	tokens.OrgName = old.OrgName
	tokens.Subject = old.Subject
	tokens.IDToken = old.IDToken
	if len(old.Scopes) != 0 {
		tokens.Scopes = append([]string(nil), old.Scopes...)
	}
	return tokens, nil
}

type claudeOAuthBootstrapResponse struct {
	OAuthAccount struct {
		AccountUUID      string `json:"account_uuid"`
		AccountEmail     string `json:"account_email"`
		OrganizationUUID string `json:"organization_uuid"`
		OrganizationName string `json:"organization_name"`
	} `json:"oauth_account"`
}

func (driver *claudeOAuthDriver) claudeOAuthEnrichIdentity(
	ctx context.Context,
	tokens *TokenSet,
	includeOrg bool,
	refresh bool,
) error {
	if tokens.AccountID != "" && tokens.Email != "" && (!includeOrg || tokens.OrgID != "") {
		return nil
	}
	identity, err := driver.claudeOAuthBootstrapIdentity(ctx, tokens.AccessToken, refresh)
	if err != nil {
		if ctx.Err() != nil {
			return claudeOAuthFailure(refresh, 0, "Claude identity request was canceled")
		}
		return nil
	}
	if tokens.AccountID == "" {
		tokens.AccountID = strings.TrimSpace(identity.OAuthAccount.AccountUUID)
	}
	if tokens.Email == "" {
		tokens.Email = strings.TrimSpace(identity.OAuthAccount.AccountEmail)
	}
	if includeOrg {
		if tokens.OrgID == "" {
			tokens.OrgID = strings.TrimSpace(identity.OAuthAccount.OrganizationUUID)
		}
		if tokens.OrgName == "" {
			tokens.OrgName = strings.TrimSpace(identity.OAuthAccount.OrganizationName)
		}
	}
	return nil
}

func (driver *claudeOAuthDriver) claudeOAuthBootstrapIdentity(
	ctx context.Context,
	accessToken string,
	refresh bool,
) (claudeOAuthBootstrapResponse, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, claudeOAuthRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(boundedCtx, http.MethodGet, claudeOAuthBootstrapURL, nil)
	if err != nil {
		return claudeOAuthBootstrapResponse{}, claudeOAuthFailure(refresh, 0, "Claude identity request could not be created")
	}
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "claude-code/"+claudeOAuthCLIVersion)
	request.Header.Set("anthropic-beta", claudeOAuthBeta)
	response, err := driver.client.Do(request)
	if err != nil {
		if boundedCtx.Err() != nil {
			return claudeOAuthBootstrapResponse{}, claudeOAuthFailure(refresh, 0, "Claude identity request was canceled or timed out")
		}
		return claudeOAuthBootstrapResponse{}, claudeOAuthFailure(refresh, 0, "Claude identity endpoint was unavailable")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, claudeOAuthTokenBodyLimit+1))
	if readErr != nil || len(body) > claudeOAuthTokenBodyLimit {
		return claudeOAuthBootstrapResponse{}, claudeOAuthFailure(refresh, response.StatusCode, "Claude identity response could not be read")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return claudeOAuthBootstrapResponse{}, claudeOAuthFailure(refresh, response.StatusCode, "Claude identity endpoint rejected the request")
	}
	var identity claudeOAuthBootstrapResponse
	if json.Unmarshal(body, &identity) != nil {
		return claudeOAuthBootstrapResponse{}, claudeOAuthFailure(refresh, response.StatusCode, "Claude identity response was invalid")
	}
	return identity, nil
}
