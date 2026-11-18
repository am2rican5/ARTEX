package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type claudeOAuthRoundTripFunc func(*http.Request) (*http.Response, error)

func (function claudeOAuthRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func claudeOAuthTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func claudeOAuthTestChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func claudeOAuthAssertTerminalSecretSafe(t *testing.T, err error, secrets ...string) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an OAuth error")
	}
	if !IsTerminal(err) {
		t.Fatalf("error is not terminal: %T %v", err, err)
	}
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	message := err.Error()
	for _, secret := range append(secrets, "api.anthropic.com", "claude.ai/oauth/authorize") {
		if secret != "" && strings.Contains(message, secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
	return typed
}

func TestClaudeOAuthDeclarationAndExchange(t *testing.T) {
	state := "state-synthetic"
	verifier := "verifier-synthetic-with-enough-entropy-for-pkce"
	challenge := claudeOAuthTestChallenge(verifier)
	var observedRequest bool
	driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		observedRequest = true
		if request.Method != http.MethodPost {
			t.Errorf("token request method = %q, want POST", request.Method)
		}
		if request.URL.String() != claudeOAuthTokenURL {
			t.Errorf("token request URL = %q, want fixed token URL", request.URL.String())
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("token request Content-Type = %q", got)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read token request: %v", err)
		}
		var values map[string]string
		if err := json.Unmarshal(body, &values); err != nil {
			t.Errorf("decode token request: %v", err)
		}
		want := map[string]string{
			"grant_type":    "authorization_code",
			"client_id":     claudeOAuthClientID,
			"code":          "code-synthetic",
			"redirect_uri":  claudeOAuthRedirectURI,
			"code_verifier": verifier,
			"state":         state,
		}
		if len(values) != len(want) {
			t.Errorf("token request fields = %#v", values)
		}
		for key, expected := range want {
			if values[key] != expected {
				t.Errorf("token request %s = %q, want %q", key, values[key], expected)
			}
		}
		return claudeOAuthTestResponse(http.StatusOK, `{
			"access_token":" access-synthetic ",
			"refresh_token":" refresh-synthetic ",
			"expires_in":3600,
			"account":{"uuid":" account-1 ","email_address":" user@example.test "},
			"organization":{"uuid":" org-1 ","name":" Example Org "}
		}`), nil
	})})

	if driver.Kind() != ProviderClaude {
		t.Fatalf("Kind() = %q", driver.Kind())
	}
	callback := driver.Callback()
	if callback.RedirectURI != "http://localhost:54545/callback" || callback.Port != 54545 || callback.Path != "/callback" {
		t.Fatalf("Callback() = %#v", callback)
	}
	authorizationURL := driver.AuthorizationURL(state, challenge)
	if err := validateAuthorizationURL(authorizationURL, state); err != nil {
		t.Fatalf("manager authorization validator rejected pinned Claude URL: %v", err)
	}
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "claude.ai" || parsed.Path != "/oauth/authorize" {
		t.Fatalf("authorization origin = %s://%s%s", parsed.Scheme, parsed.Host, parsed.Path)
	}
	query := parsed.Query()
	wantQuery := map[string]string{
		"client_id":             claudeOAuthClientID,
		"response_type":         "code",
		"redirect_uri":          claudeOAuthRedirectURI,
		"scope":                 "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"state":                 state,
		"code":                  "true",
	}
	if len(query) != len(wantQuery) {
		t.Fatalf("authorization query = %#v", query)
	}
	for key, expected := range wantQuery {
		if values := query[key]; len(values) != 1 || values[0] != expected {
			t.Errorf("authorization query %s = %#v, want %q", key, values, expected)
		}
	}

	before := time.Now()
	tokens, err := driver.Exchange(context.Background(), CodeExchange{
		Code:        "code-synthetic",
		State:       state,
		Verifier:    verifier,
		RedirectURI: claudeOAuthRedirectURI,
	})
	after := time.Now()
	if err != nil {
		t.Fatalf("Exchange() error: %v", err)
	}
	if !observedRequest {
		t.Fatal("token request was not observed")
	}
	if tokens.AccessToken != "access-synthetic" || tokens.RefreshToken != "refresh-synthetic" {
		t.Fatalf("token projection = %#v", tokens)
	}
	if tokens.AccountID != "account-1" || tokens.Email != "user@example.test" || tokens.OrgID != "org-1" || tokens.OrgName != "Example Org" {
		t.Fatalf("identity projection = %#v", tokens)
	}
	if tokens.ClientID != claudeOAuthClientID || tokens.Issuer != claudeOAuthIssuer {
		t.Fatalf("client identity = %#v", tokens)
	}
	if tokens.ExpiresAt.Before(before.Add(54*time.Minute+59*time.Second)) || tokens.ExpiresAt.After(after.Add(55*time.Minute+time.Second)) {
		t.Fatalf("ExpiresAt = %v, want expires_in minus five-minute skew", tokens.ExpiresAt)
	}
	if got := strings.Join(tokens.Scopes, " "); got != wantQuery["scope"] {
		t.Fatalf("Scopes = %q", got)
	}
}

func TestClaudeOAuthExchangeRequiresExplicitState(t *testing.T) {
	calls := 0
	driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return claudeOAuthTestResponse(http.StatusOK, `{}`), nil
	})})
	driver.AuthorizationURL("implicit-state", claudeOAuthTestChallenge("implicit-verifier"))
	_, err := driver.Exchange(context.Background(), CodeExchange{
		Code:        "implicit-code-secret",
		Verifier:    "implicit-verifier",
		RedirectURI: claudeOAuthRedirectURI,
	})
	typed := claudeOAuthAssertTerminalSecretSafe(t, err, "implicit-code-secret", "implicit-verifier")
	if typed.Code != CodeAuthorizationFailed {
		t.Fatalf("error code = %q", typed.Code)
	}
	if calls != 0 {
		t.Fatalf("state-less exchange made %d HTTP requests", calls)
	}
}

func TestClaudeOAuthAuthorizationProtocolFlagGuard(t *testing.T) {
	state := "guard-state"
	driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("authorization declaration must not perform HTTP")
		return nil, nil
	})})
	valid := driver.AuthorizationURL(state, "guard-challenge")
	if err := validateAuthorizationURL(valid, state); err != nil {
		t.Fatalf("pinned code=true flag rejected: %v", err)
	}

	tests := map[string]func(*url.URL){
		"credential code": func(candidate *url.URL) {
			query := candidate.Query()
			query.Set("code", "secret-code")
			candidate.RawQuery = query.Encode()
		},
		"multiple code values": func(candidate *url.URL) {
			query := candidate.Query()
			query.Add("code", "secret-code")
			candidate.RawQuery = query.Encode()
		},
		"other origin": func(candidate *url.URL) {
			candidate.Host = "example.invalid"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate, err := url.Parse(valid)
			if err != nil {
				t.Fatalf("parse valid authorization URL: %v", err)
			}
			mutate(candidate)
			if err := validateAuthorizationURL(candidate.String(), state); err == nil {
				t.Fatalf("validateAuthorizationURL accepted %s", name)
			}
		})
	}
}

type claudeOAuthCallbackOverride struct {
	OAuthDriver
	callback CallbackConfig
}

func (driver claudeOAuthCallbackOverride) Callback() CallbackConfig {
	return driver.callback
}

func (driver claudeOAuthCallbackOverride) AuthorizationURL(state, challenge string) string {
	parsed, _ := url.Parse(driver.OAuthDriver.AuthorizationURL(state, challenge))
	query := parsed.Query()
	query.Set("redirect_uri", driver.callback.RedirectURI)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func TestClaudeOAuthManagerBeginAcceptsProtocolFlag(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate dynamic callback port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatalf("release dynamic callback port: %v", err)
	}
	callback := CallbackConfig{
		RedirectURI: "http://localhost:" + strconv.Itoa(port) + claudeOAuthCallbackPath,
		Port:        port,
		Path:        claudeOAuthCallbackPath,
	}
	driver := claudeOAuthCallbackOverride{
		OAuthDriver: NewClaudeOAuth(nil),
		callback:    callback,
	}
	manager := NewManager(newTestStore(), Options{
		Drivers:            []OAuthDriver{driver},
		CallbackListenHost: "127.0.0.1",
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("Close() error: %v", err)
		}
	}()
	attempt, err := manager.Begin(context.Background(), "owner", ProviderClaude, 0)
	if err != nil {
		t.Fatalf("Begin() rejected pinned code=true flag: %v", err)
	}
	parsed, err := url.Parse(attempt.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse attempt authorization URL: %v", err)
	}
	if values := parsed.Query()["code"]; len(values) != 1 || values[0] != "true" {
		t.Fatalf("attempt code flag = %#v", values)
	}
	if attempt.CallbackURL != callback.RedirectURI {
		t.Fatalf("attempt callback = %q, want dynamic fixture %q", attempt.CallbackURL, callback.RedirectURI)
	}
}

func TestClaudeOAuthExchangeBootstrapsIdentity(t *testing.T) {
	state := "bootstrap-state"
	verifier := "bootstrap-verifier"
	var bootstrapSeen bool
	driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case claudeOAuthTokenURL:
			return claudeOAuthTestResponse(http.StatusOK, `{
				"access_token":"bootstrap-access",
				"refresh_token":"bootstrap-refresh",
				"expires_in":3600
			}`), nil
		case claudeOAuthBootstrapURL:
			bootstrapSeen = true
			if request.Method != http.MethodGet {
				t.Errorf("bootstrap method = %q", request.Method)
			}
			if request.Header.Get("Authorization") != "Bearer bootstrap-access" {
				t.Errorf("bootstrap authorization header was not derived from exchanged token")
			}
			if request.Header.Get("anthropic-beta") != claudeOAuthBeta {
				t.Errorf("bootstrap beta header = %q", request.Header.Get("anthropic-beta"))
			}
			if request.Header.Get("User-Agent") != "claude-code/2.1.321" {
				t.Errorf("bootstrap User-Agent = %q", request.Header.Get("User-Agent"))
			}
			return claudeOAuthTestResponse(http.StatusOK, `{
				"oauth_account":{
					"account_uuid":"boot-account",
					"account_email":"boot@example.test",
					"organization_uuid":"boot-org",
					"organization_name":"Boot Org"
				}
			}`), nil
		default:
			t.Errorf("unexpected OAuth request URL %q", request.URL.String())
			return claudeOAuthTestResponse(http.StatusNotFound, `{}`), nil
		}
	})})
	driver.AuthorizationURL(state, claudeOAuthTestChallenge(verifier))
	tokens, err := driver.Exchange(context.Background(), CodeExchange{
		Code:        "bootstrap-code",
		State:       state,
		Verifier:    verifier,
		RedirectURI: claudeOAuthRedirectURI,
	})
	if err != nil {
		t.Fatalf("Exchange() error: %v", err)
	}
	if !bootstrapSeen {
		t.Fatal("identity bootstrap request was not observed")
	}
	if tokens.AccountID != "boot-account" || tokens.Email != "boot@example.test" || tokens.OrgID != "boot-org" || tokens.OrgName != "Boot Org" {
		t.Fatalf("bootstrapped identity = %#v", tokens)
	}
}

func TestClaudeOAuthExchangeRejectsMissingIdentity(t *testing.T) {
	state := "missing-state"
	verifier := "missing-verifier"
	driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == claudeOAuthTokenURL {
			return claudeOAuthTestResponse(http.StatusOK, `{
				"access_token":"missing-identity-access",
				"refresh_token":"missing-identity-refresh",
				"expires_in":3600
			}`), nil
		}
		return claudeOAuthTestResponse(http.StatusForbidden, `{"error":"bootstrap-body-secret"}`), nil
	})})
	driver.AuthorizationURL(state, claudeOAuthTestChallenge(verifier))
	_, err := driver.Exchange(context.Background(), CodeExchange{
		Code:        "missing-code-secret",
		State:       state,
		Verifier:    verifier,
		RedirectURI: claudeOAuthRedirectURI,
	})
	typed := claudeOAuthAssertTerminalSecretSafe(
		t,
		err,
		"missing-identity-access",
		"missing-identity-refresh",
		"missing-code-secret",
		"bootstrap-body-secret",
	)
	if typed.Code != CodeAuthorizationFailed {
		t.Fatalf("error code = %q", typed.Code)
	}
}

func TestClaudeOAuthRefreshRotationAndMetadataRetention(t *testing.T) {
	old := TokenSet{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute),
		AccountID:    "account-1",
		OrgID:        "org-1",
		OrgName:      "Original Org",
		Email:        "old@example.test",
		ClientID:     claudeOAuthClientID,
		Issuer:       claudeOAuthIssuer,
		Subject:      "subject-1",
		IDToken:      "old-id-token",
		Scopes:       []string{"scope:old"},
	}
	tests := []struct {
		name             string
		refreshField     string
		wantRefreshToken string
	}{
		{name: "rotated refresh token", refreshField: `,"refresh_token":"rotated-refresh"`, wantRefreshToken: "rotated-refresh"},
		{name: "omitted refresh token", wantRefreshToken: "old-refresh"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var tokenSeen bool
			var bootstrapSeen bool
			driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.String() {
				case claudeOAuthTokenURL:
					tokenSeen = true
					if request.Header.Get("anthropic-beta") != claudeOAuthBeta {
						t.Errorf("refresh beta header = %q", request.Header.Get("anthropic-beta"))
					}
					if request.Header.Get("User-Agent") != "anthropic-sdk-typescript/0.128.0 userOAuthProvider" {
						t.Errorf("refresh User-Agent = %q", request.Header.Get("User-Agent"))
					}
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Errorf("read refresh request: %v", err)
					}
					var values map[string]string
					if err := json.Unmarshal(body, &values); err != nil {
						t.Errorf("decode refresh request: %v", err)
					}
					if len(values) != 3 || values["grant_type"] != "refresh_token" || values["client_id"] != claudeOAuthClientID || values["refresh_token"] != "old-refresh" {
						t.Errorf("refresh request = %#v", values)
					}
					return claudeOAuthTestResponse(http.StatusOK, `{"access_token":"new-access","expires_in":3600`+test.refreshField+`}`), nil
				case claudeOAuthBootstrapURL:
					bootstrapSeen = true
					return claudeOAuthTestResponse(http.StatusForbidden, `{"error":"unavailable"}`), nil
				default:
					t.Errorf("unexpected OAuth request URL %q", request.URL.String())
					return claudeOAuthTestResponse(http.StatusNotFound, `{}`), nil
				}
			})})
			tokens, err := driver.Refresh(context.Background(), old)
			if err != nil {
				t.Fatalf("Refresh() error: %v", err)
			}
			if !tokenSeen || !bootstrapSeen {
				t.Fatalf("requests observed: token=%v bootstrap=%v", tokenSeen, bootstrapSeen)
			}
			if tokens.AccessToken != "new-access" || tokens.RefreshToken != test.wantRefreshToken {
				t.Fatalf("refreshed secrets = %#v", tokens)
			}
			if tokens.AccountID != old.AccountID || tokens.OrgID != old.OrgID || tokens.OrgName != old.OrgName || tokens.Email != old.Email {
				t.Fatalf("refreshed account metadata = %#v", tokens)
			}
			if tokens.ClientID != old.ClientID || tokens.Issuer != old.Issuer || tokens.Subject != old.Subject || tokens.IDToken != old.IDToken {
				t.Fatalf("refreshed immutable metadata = %#v", tokens)
			}
			if len(tokens.Scopes) != 1 || tokens.Scopes[0] != "scope:old" {
				t.Fatalf("refreshed scopes = %#v", tokens.Scopes)
			}
		})
	}
}

func TestClaudeOAuthSanitizedFailuresAndCancellation(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		bodySecret string
		wantStatus int
	}{
		{
			name:       "malformed success",
			status:     http.StatusOK,
			body:       `{"access_token":"malformed-body-secret"`,
			bodySecret: "malformed-body-secret",
			wantStatus: http.StatusOK,
		},
		{
			name:       "unauthorized",
			status:     http.StatusUnauthorized,
			body:       `{"error":"invalid_grant","error_description":"unauthorized-body-secret"}`,
			bodySecret: "unauthorized-body-secret",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "forbidden",
			status:     http.StatusForbidden,
			body:       `{"error":"forbidden-body-secret"}`,
			bodySecret: "forbidden-body-secret",
			wantStatus: http.StatusForbidden,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := "failure-state"
			verifier := "failure-verifier"
			driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() != claudeOAuthTokenURL {
					t.Errorf("unexpected request URL %q", request.URL.String())
				}
				return claudeOAuthTestResponse(test.status, test.body), nil
			})})
			driver.AuthorizationURL(state, claudeOAuthTestChallenge(verifier))
			_, err := driver.Exchange(context.Background(), CodeExchange{
				Code:        "failure-code-secret",
				State:       state,
				Verifier:    verifier,
				RedirectURI: claudeOAuthRedirectURI,
			})
			typed := claudeOAuthAssertTerminalSecretSafe(t, err, test.bodySecret, "failure-code-secret", verifier)
			if typed.Code != CodeAuthorizationFailed || typed.Status != test.wantStatus {
				t.Fatalf("error = %#v", typed)
			}
		})
	}

	t.Run("canceled", func(t *testing.T) {
		calls := 0
		state := "cancel-state"
		verifier := "cancel-verifier-secret"
		driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return claudeOAuthTestResponse(http.StatusOK, `{}`), nil
		})})
		driver.AuthorizationURL(state, claudeOAuthTestChallenge(verifier))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := driver.Exchange(ctx, CodeExchange{
			Code:        "cancel-code-secret",
			State:       state,
			Verifier:    verifier,
			RedirectURI: claudeOAuthRedirectURI,
		})
		typed := claudeOAuthAssertTerminalSecretSafe(t, err, "cancel-code-secret", verifier)
		if typed.Code != CodeAuthorizationFailed {
			t.Fatalf("error code = %q", typed.Code)
		}
		if calls != 0 {
			t.Fatalf("canceled exchange made %d HTTP requests", calls)
		}
	})

	t.Run("in-flight canceled", func(t *testing.T) {
		started := make(chan struct{})
		result := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		state := "in-flight-cancel-state"
		verifier := "in-flight-cancel-verifier-secret"
		driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})})
		driver.AuthorizationURL(state, claudeOAuthTestChallenge(verifier))
		go func() {
			_, err := driver.Exchange(ctx, CodeExchange{
				Code:        "in-flight-cancel-code-secret",
				State:       state,
				Verifier:    verifier,
				RedirectURI: claudeOAuthRedirectURI,
			})
			result <- err
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("token request did not start")
		}
		cancel()
		select {
		case err := <-result:
			typed := claudeOAuthAssertTerminalSecretSafe(
				t,
				err,
				"in-flight-cancel-code-secret",
				"in-flight-cancel-verifier-secret",
			)
			if typed.Code != CodeAuthorizationFailed {
				t.Fatalf("error code = %q", typed.Code)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled token request did not return")
		}
	})
}

func TestClaudeOAuthRefreshAuthorizationFailureIsTerminal(t *testing.T) {
	old := TokenSet{
		RefreshToken: "refresh-secret",
		AccountID:    "account-1",
		OrgID:        "org-1",
		ClientID:     claudeOAuthClientID,
		Issuer:       claudeOAuthIssuer,
	}
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"grant-body-secret"}`},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":"forbidden-body-secret"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return claudeOAuthTestResponse(test.status, test.body), nil
			})})
			_, err := driver.Refresh(context.Background(), old)
			typed := claudeOAuthAssertTerminalSecretSafe(t, err, "refresh-secret", "grant-body-secret", "forbidden-body-secret")
			if typed.Code != CodeAuthRequired || typed.Status != test.status {
				t.Fatalf("refresh error = %#v", typed)
			}
		})
	}
}

func TestClaudeOAuthDoesNotFollowRedirects(t *testing.T) {
	state := "redirect-state"
	verifier := "redirect-verifier"
	calls := 0
	driver := NewClaudeOAuth(&http.Client{Transport: claudeOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		response := claudeOAuthTestResponse(http.StatusFound, `redirect-body-secret`)
		response.Header.Set("Location", "https://example.invalid/capture")
		return response, nil
	})})
	driver.AuthorizationURL(state, claudeOAuthTestChallenge(verifier))
	_, err := driver.Exchange(context.Background(), CodeExchange{
		Code:        "redirect-code-secret",
		State:       state,
		Verifier:    verifier,
		RedirectURI: claudeOAuthRedirectURI,
	})
	typed := claudeOAuthAssertTerminalSecretSafe(t, err, "redirect-body-secret", "redirect-code-secret", "example.invalid")
	if typed.Status != http.StatusFound {
		t.Fatalf("redirect error status = %d", typed.Status)
	}
	if calls != 1 {
		t.Fatalf("redirect attempt made %d transport calls", calls)
	}
}

func TestClaudeOAuthRevocationUnsupported(t *testing.T) {
	driver := NewClaudeOAuth(nil)
	err := driver.Revoke(context.Background(), TokenSet{AccessToken: "revocation-secret"})
	if !errors.Is(err, ErrRevocationUnsupported) {
		t.Fatalf("Revoke() error = %v", err)
	}
	if !IsTerminal(err) {
		t.Fatalf("Revoke() error is not terminal: %v", err)
	}
	if strings.Contains(err.Error(), "revocation-secret") {
		t.Fatalf("Revoke() leaked token: %v", err)
	}
}
