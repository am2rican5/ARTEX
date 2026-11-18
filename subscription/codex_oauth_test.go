package subscription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type codexOAuthRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip codexOAuthRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestCodexOAuthAuthorizationContract(t *testing.T) {
	driver := NewCodexOAuth(nil)
	if driver.Kind() != ProviderCodex {
		t.Fatalf("Kind() = %q, want %q", driver.Kind(), ProviderCodex)
	}
	callback := driver.Callback()
	wantCallback := CallbackConfig{
		RedirectURI: codexOAuthRedirectURI,
		Port:        codexOAuthCallbackPort,
		Path:        codexOAuthCallbackPath,
	}
	if callback != wantCallback {
		t.Fatalf("Callback() = %#v, want %#v", callback, wantCallback)
	}

	authorizationURL, err := url.Parse(driver.AuthorizationURL("synthetic-state", "synthetic-challenge"))
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if authorizationURL.Scheme != "https" || authorizationURL.Host != "auth.openai.com" ||
		authorizationURL.Path != "/oauth/authorize" {
		t.Fatalf("unexpected authorization endpoint: %s://%s%s", authorizationURL.Scheme, authorizationURL.Host, authorizationURL.Path)
	}
	want := map[string]string{
		"response_type":              "code",
		"client_id":                  codexOAuthClientID,
		"redirect_uri":               codexOAuthRedirectURI,
		"scope":                      codexOAuthScope,
		"code_challenge":             "synthetic-challenge",
		"code_challenge_method":      "S256",
		"state":                      "synthetic-state",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 "omp",
	}
	query := authorizationURL.Query()
	if len(query) != len(want) {
		t.Fatalf("authorization parameter count = %d, want %d: %#v", len(query), len(want), query)
	}
	for key, value := range want {
		values := query[key]
		if len(values) != 1 || values[0] != value {
			t.Errorf("authorization parameter %q = %#v, want %q", key, values, value)
		}
	}
}

func TestCodexOAuthExchangeUsesFixedEndpointFormAndPKCE(t *testing.T) {
	accessToken := codexOAuthTestJWT(t, map[string]any{
		"sub": "access-subject",
		codexOAuthAuthClaim: map[string]any{
			"chatgpt_account_id": "workspace-123",
		},
		codexOAuthProfileClaim: map[string]any{
			"email": "  User@Example.COM ",
		},
	})
	idToken := codexOAuthTestJWT(t, map[string]any{
		"iss": codexOAuthIssuer,
		"sub": "identity-subject",
		codexOAuthAuthClaim: map[string]any{
			"chatgpt_plan_type": " Pro ",
		},
	})

	transport := codexOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", request.Method)
		}
		if request.URL.String() != codexOAuthTokenURL {
			t.Errorf("token endpoint = %q, want %q", request.URL.String(), codexOAuthTokenURL)
		}
		if request.URL.Hostname() == "localhost" {
			t.Errorf("token endpoint unexpectedly used callback host")
		}
		if contentType := request.Header.Get("Content-Type"); contentType != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", contentType)
		}
		form := codexOAuthReadForm(t, request)
		codexOAuthAssertForm(t, form, map[string]string{
			"grant_type":    "authorization_code",
			"client_id":     codexOAuthClientID,
			"code":          "synthetic-code",
			"code_verifier": "synthetic-verifier",
			"redirect_uri":  codexOAuthRedirectURI,
		})
		return codexOAuthJSONResponse(t, http.StatusOK, map[string]any{
			"access_token":  accessToken,
			"refresh_token": "refresh-original",
			"id_token":      idToken,
			"expires_in":    3600,
		}), nil
	})
	providedClient := &http.Client{Transport: transport, Timeout: time.Hour}
	driver := NewCodexOAuth(providedClient)
	implementation, ok := driver.(*codexOAuthDriver)
	if !ok {
		t.Fatalf("NewCodexOAuth returned %T", driver)
	}
	if implementation.client == providedClient {
		t.Fatalf("OAuth driver reused the caller's HTTP client")
	}
	if implementation.client.Timeout != codexOAuthRequestTimeout {
		t.Fatalf("OAuth timeout = %v, want %v", implementation.client.Timeout, codexOAuthRequestTimeout)
	}
	if providedClient.Timeout != time.Hour {
		t.Fatalf("constructor mutated caller timeout to %v", providedClient.Timeout)
	}

	before := time.Now()
	tokens, err := driver.Exchange(context.Background(), CodeExchange{
		Code:        "synthetic-code",
		State:       "synthetic-state",
		Verifier:    "synthetic-verifier",
		RedirectURI: codexOAuthRedirectURI,
	})
	after := time.Now()
	if err != nil {
		t.Fatalf("Exchange() error: %v", err)
	}
	if tokens.AccessToken != accessToken || tokens.RefreshToken != "refresh-original" || tokens.IDToken != idToken {
		t.Fatalf("token material was not projected from the response")
	}
	if tokens.AccountID != "workspace-123" || tokens.OrgID != "workspace-123" {
		t.Fatalf("account/workspace = %q/%q", tokens.AccountID, tokens.OrgID)
	}
	if tokens.OrgName != "pro" || tokens.Email != "user@example.com" {
		t.Fatalf("normalized plan/email = %q/%q", tokens.OrgName, tokens.Email)
	}
	if tokens.ClientID != codexOAuthClientID || tokens.Issuer != codexOAuthIssuer || tokens.Subject != "identity-subject" {
		t.Fatalf("identity metadata = client %q, issuer %q, subject %q", tokens.ClientID, tokens.Issuer, tokens.Subject)
	}
	if tokens.ExpiresAt.Before(before.Add(time.Hour)) || tokens.ExpiresAt.After(after.Add(time.Hour)) {
		t.Fatalf("expiry %v is outside exchange window", tokens.ExpiresAt)
	}
	if strings.Join(tokens.Scopes, " ") != strings.Join(codexOAuthScopes, " ") {
		t.Fatalf("scopes = %#v, want %#v", tokens.Scopes, codexOAuthScopes)
	}
}

func TestCodexOAuthExchangeFallsBackToIDTokenProfile(t *testing.T) {
	accessToken := codexOAuthTestJWT(t, map[string]any{"sub": "access-subject"})
	idToken := codexOAuthTestJWT(t, map[string]any{
		"sub": "identity-subject",
		codexOAuthAuthClaim: map[string]any{
			"chatgpt_account_id": "workspace-from-id",
			"chatgpt_plan_type":  " TEAM ",
		},
		codexOAuthProfileClaim: map[string]any{
			"email": " ID@Example.COM ",
		},
	})
	driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return codexOAuthJSONResponse(t, http.StatusOK, map[string]any{
			"access_token":  accessToken,
			"refresh_token": "refresh-token",
			"id_token":      idToken,
			"expires_in":    60,
		}), nil
	})})

	tokens, err := driver.Exchange(context.Background(), CodeExchange{
		Code:        "code",
		State:       "state",
		Verifier:    "verifier",
		RedirectURI: codexOAuthRedirectURI,
	})
	if err != nil {
		t.Fatalf("Exchange() error: %v", err)
	}
	if tokens.AccountID != "workspace-from-id" || tokens.OrgID != "workspace-from-id" ||
		tokens.OrgName != "team" || tokens.Email != "id@example.com" || tokens.Subject != "identity-subject" {
		t.Fatalf("ID-token profile fallback = %#v", tokens)
	}
}

func TestCodexOAuthRefreshRotatesAndMergesReplacementTokens(t *testing.T) {
	old := TokenSet{
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		ExpiresAt:    time.Unix(100, 0),
		AccountID:    "workspace-123",
		OrgID:        "workspace-123",
		OrgName:      "pro",
		Email:        "old@example.com",
		ClientID:     codexOAuthClientID,
		Issuer:       codexOAuthIssuer,
		Subject:      "stable-subject",
		IDToken:      "id-old",
		Scopes:       []string{"old.scope"},
	}

	t.Run("rotated response replaces rotatable fields and keeps workspace metadata", func(t *testing.T) {
		accessToken := codexOAuthTestJWT(t, map[string]any{
			"sub": "access-subject",
			codexOAuthAuthClaim: map[string]any{
				"chatgpt_account_id": "workspace-123",
				"chatgpt_plan_type":  "enterprise",
			},
			codexOAuthProfileClaim: map[string]any{
				"email": " New@Example.COM ",
			},
		})
		idToken := codexOAuthTestJWT(t, map[string]any{"sub": "stable-subject"})
		driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			form := codexOAuthReadForm(t, request)
			codexOAuthAssertForm(t, form, map[string]string{
				"grant_type":    "refresh_token",
				"client_id":     codexOAuthClientID,
				"refresh_token": "refresh-old",
			})
			return codexOAuthJSONResponse(t, http.StatusOK, map[string]any{
				"access_token":  accessToken,
				"refresh_token": "refresh-rotated",
				"id_token":      idToken,
				"expires_in":    1800,
				"scope":         "openid profile",
			}), nil
		})})

		refreshed, err := driver.Refresh(context.Background(), old)
		if err != nil {
			t.Fatalf("Refresh() error: %v", err)
		}
		if refreshed.AccessToken != accessToken || refreshed.RefreshToken != "refresh-rotated" || refreshed.IDToken != idToken {
			t.Fatalf("rotated token fields = %#v", refreshed)
		}
		if refreshed.AccountID != old.AccountID || refreshed.OrgID != old.OrgID || refreshed.OrgName != old.OrgName {
			t.Fatalf("workspace metadata changed: %#v", refreshed)
		}
		if refreshed.Email != "new@example.com" || refreshed.Subject != old.Subject {
			t.Fatalf("refreshed identity profile = %#v", refreshed)
		}
		if strings.Join(refreshed.Scopes, " ") != "openid profile" {
			t.Fatalf("refreshed scopes = %#v", refreshed.Scopes)
		}
	})

	t.Run("omitted replacement fields preserve stored values", func(t *testing.T) {
		accessToken := codexOAuthTestJWT(t, map[string]any{})
		driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return codexOAuthJSONResponse(t, http.StatusOK, map[string]any{
				"access_token": accessToken,
				"expires_in":   1800,
			}), nil
		})})

		refreshed, err := driver.Refresh(context.Background(), old)
		if err != nil {
			t.Fatalf("Refresh() error: %v", err)
		}
		if refreshed.AccessToken != accessToken || refreshed.RefreshToken != old.RefreshToken ||
			refreshed.IDToken != old.IDToken || refreshed.AccountID != old.AccountID ||
			refreshed.OrgID != old.OrgID || refreshed.OrgName != old.OrgName ||
			refreshed.Email != old.Email || refreshed.Subject != old.Subject {
			t.Fatalf("missing replacement fields were not merged: %#v", refreshed)
		}
		if strings.Join(refreshed.Scopes, " ") != strings.Join(old.Scopes, " ") {
			t.Fatalf("missing scopes = %#v, want %#v", refreshed.Scopes, old.Scopes)
		}
	})

	t.Run("stored ID token supplies account metadata on refresh", func(t *testing.T) {
		storedIDToken := codexOAuthTestJWT(t, map[string]any{
			"sub":               "stable-subject",
			codexOAuthAuthClaim: map[string]any{"chatgpt_account_id": "workspace-from-stored-id"},
		})
		oldWithoutProjectedAccount := old
		oldWithoutProjectedAccount.AccountID = ""
		oldWithoutProjectedAccount.OrgID = ""
		oldWithoutProjectedAccount.IDToken = storedIDToken
		accessToken := codexOAuthTestJWT(t, map[string]any{"sub": "access-subject"})
		driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return codexOAuthJSONResponse(t, http.StatusOK, map[string]any{
				"access_token": accessToken,
				"expires_in":   1800,
			}), nil
		})})

		refreshed, err := driver.Refresh(context.Background(), oldWithoutProjectedAccount)
		if err != nil {
			t.Fatalf("Refresh() error: %v", err)
		}
		if refreshed.AccountID != "workspace-from-stored-id" || refreshed.IDToken != storedIDToken ||
			refreshed.Subject != old.Subject {
			t.Fatalf("stored ID-token account fallback = %#v", refreshed)
		}
	})

	t.Run("email-only refresh without stored account is rejected", func(t *testing.T) {
		emailOnlyAccess := codexOAuthTestJWT(t, map[string]any{
			codexOAuthProfileClaim: map[string]any{"email": "user@example.com"},
		})
		driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return codexOAuthJSONResponse(t, http.StatusOK, map[string]any{
				"access_token": emailOnlyAccess,
				"expires_in":   1800,
			}), nil
		})})

		_, err := driver.Refresh(context.Background(), TokenSet{RefreshToken: "refresh-old"})
		var terminal *Error
		if !errors.As(err, &terminal) || !IsTerminal(err) || terminal.Code != CodeRefreshFailed {
			t.Fatalf("email-only refresh error = %#v", err)
		}
	})
}

func TestCodexOAuthRejectsMissingTokenFieldsAndIdentity(t *testing.T) {
	identityToken := codexOAuthTestJWT(t, map[string]any{
		codexOAuthAuthClaim: map[string]any{"chatgpt_account_id": "workspace-123"},
	})
	emailOnlyToken := codexOAuthTestJWT(t, map[string]any{
		codexOAuthProfileClaim: map[string]any{"email": "user@example.com"},
	})
	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "access token",
			body: map[string]any{"refresh_token": "refresh", "expires_in": 3600},
		},
		{
			name: "refresh token",
			body: map[string]any{"access_token": identityToken, "expires_in": 3600},
		},
		{
			name: "expiry",
			body: map[string]any{"access_token": identityToken, "refresh_token": "refresh"},
		},
		{
			name: "account identity",
			body: map[string]any{
				"access_token":  codexOAuthTestJWT(t, map[string]any{}),
				"refresh_token": "refresh",
				"expires_in":    3600,
			},
		},
		{
			name: "email without ChatGPT account",
			body: map[string]any{
				"access_token":  emailOnlyToken,
				"refresh_token": "refresh",
				"expires_in":    3600,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return codexOAuthJSONResponse(t, http.StatusOK, test.body), nil
			})})
			_, err := driver.Exchange(context.Background(), CodeExchange{
				Code:        "code",
				State:       "state",
				Verifier:    "verifier",
				RedirectURI: codexOAuthRedirectURI,
			})
			if err == nil {
				t.Fatalf("Exchange() unexpectedly succeeded")
			}
			var terminal *Error
			if !errors.As(err, &terminal) || !IsTerminal(err) || terminal.Code != CodeAuthorizationFailed {
				t.Fatalf("Exchange() error = %#v, want terminal authorization failure", err)
			}
			if strings.Contains(err.Error(), identityToken) || strings.Contains(err.Error(), emailOnlyToken) ||
				strings.Contains(err.Error(), "refresh") {
				t.Fatalf("Exchange() error leaked token material: %v", err)
			}
		})
	}
}

func TestCodexOAuthErrorsAndCancellationDoNotLeakSecrets(t *testing.T) {
	const secret = "synthetic-secret-token"
	const secretRedirect = "http://localhost:1455/auth/callback?code=synthetic-secret-code"

	t.Run("transport error", func(t *testing.T) {
		driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New(secret + " " + secretRedirect)
		})})
		_, err := driver.Exchange(context.Background(), CodeExchange{
			Code:        "code",
			State:       "state",
			Verifier:    "verifier",
			RedirectURI: codexOAuthRedirectURI,
		})
		codexOAuthAssertSecretFreeTerminalError(t, err, secret, secretRedirect)
	})

	t.Run("endpoint error body", func(t *testing.T) {
		driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"error":{"code":"invalid_request","message":"` + secret + ` ` + secretRedirect + `"}}`,
				)),
			}, nil
		})})
		_, err := driver.Exchange(context.Background(), CodeExchange{
			Code:        "code",
			State:       "state",
			Verifier:    "verifier",
			RedirectURI: codexOAuthRedirectURI,
		})
		codexOAuthAssertSecretFreeTerminalError(t, err, secret, secretRedirect)
		var providerErr *Error
		if !errors.As(err, &providerErr) || providerErr.Status != http.StatusBadRequest {
			t.Fatalf("endpoint error status = %#v", providerErr)
		}
	})

	t.Run("in-flight cancellation cause", func(t *testing.T) {
		started := make(chan struct{})
		driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			close(started)
			<-request.Context().Done()
			return nil, context.Cause(request.Context())
		})})
		ctx, cancel := context.WithCancelCause(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := driver.Exchange(ctx, CodeExchange{
				Code:        "code",
				State:       "state",
				Verifier:    "verifier",
				RedirectURI: codexOAuthRedirectURI,
			})
			result <- err
		}()
		<-started
		cancel(errors.New(secret))
		err := <-result
		codexOAuthAssertSecretFreeTerminalError(t, err, secret, secretRedirect)
	})

	t.Run("redirect mismatch", func(t *testing.T) {
		called := false
		driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("unexpected request")
		})})
		_, err := driver.Exchange(context.Background(), CodeExchange{
			Code:        "code",
			State:       "state",
			Verifier:    "verifier",
			RedirectURI: secretRedirect,
		})
		codexOAuthAssertSecretFreeTerminalError(t, err, secret, secretRedirect)
		if called {
			t.Fatalf("redirect mismatch reached token endpoint")
		}
	})
}

func TestCodexOAuthRefreshAuthRequiredAndRevocationUnsupported(t *testing.T) {
	driver := NewCodexOAuth(nil)
	_, err := driver.Refresh(context.Background(), TokenSet{})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Code != CodeAuthRequired {
		t.Fatalf("missing refresh token error = %T %v", err, err)
	}
	if !IsTerminal(err) {
		t.Fatalf("missing refresh token error is not terminal")
	}
	if err := driver.Revoke(context.Background(), TokenSet{}); !errors.Is(err, ErrRevocationUnsupported) {
		t.Fatalf("Revoke() error = %v, want ErrRevocationUnsupported", err)
	}
}

func TestCodexOAuthRefreshInvalidGrantRequiresAuthorizationWithoutBodyLeak(t *testing.T) {
	const secret = "refresh-body-secret"
	driver := NewCodexOAuth(&http.Client{Transport: codexOAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"error":"invalid_grant","error_description":"` + secret + `"}`,
			)),
		}, nil
	})})
	_, err := driver.Refresh(context.Background(), TokenSet{RefreshToken: "refresh-old"})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Code != CodeAuthRequired || providerErr.Status != http.StatusUnauthorized {
		t.Fatalf("invalid-grant error = %T %#v", err, providerErr)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid-grant error leaked response body: %v", err)
	}
}

func codexOAuthTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims: %v", err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".synthetic-signature"
}

func codexOAuthJSONResponse(t *testing.T, status int, value any) *http.Response {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal HTTP fixture: %v", err)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

func codexOAuthReadForm(t *testing.T, request *http.Request) url.Values {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatalf("parse request form: %v", err)
	}
	return form
}

func codexOAuthAssertForm(t *testing.T, form url.Values, want map[string]string) {
	t.Helper()
	if len(form) != len(want) {
		t.Fatalf("form parameter count = %d, want %d: %#v", len(form), len(want), form)
	}
	for key, value := range want {
		values := form[key]
		if len(values) != 1 || values[0] != value {
			t.Errorf("form parameter %q = %#v, want %q", key, values, value)
		}
	}
}

func codexOAuthAssertSecretFreeTerminalError(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("operation unexpectedly succeeded")
	}
	if !IsTerminal(err) {
		t.Fatalf("error is not terminal: %T %v", err, err)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
}
