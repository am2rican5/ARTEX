package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/subscription"
	"github.com/golang-jwt/jwt/v5"
)

type credentialAPIDriver struct {
	kind     string
	callback subscription.CallbackConfig
	label    string

	mu          sync.Mutex
	exchanges   int
	revocations int
}

func (d *credentialAPIDriver) Kind() string { return d.kind }

func (d *credentialAPIDriver) Callback() subscription.CallbackConfig { return d.callback }

func (d *credentialAPIDriver) AuthorizationURL(state, challenge string) string {
	values := url.Values{
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return "https://auth.fixture.invalid/authorize?" + values.Encode()
}

func (d *credentialAPIDriver) Exchange(_ context.Context, input subscription.CodeExchange) (subscription.TokenSet, error) {
	d.mu.Lock()
	d.exchanges++
	d.mu.Unlock()
	if input.Code == "provider-401-secret-code" {
		return subscription.TokenSet{}, &subscription.Error{
			Code:    subscription.CodeAuthorizationFailed,
			Message: "provider authorization failed",
			Status:  http.StatusUnauthorized,
		}
	}
	return subscription.TokenSet{
		AccessToken:  "fixture-access-token-must-not-leak",
		RefreshToken: "fixture-refresh-token-must-not-leak",
		ExpiresAt:    time.Now().Add(time.Hour),
		AccountID:    "fixture-account",
		OrgID:        "fixture-org",
		OrgName:      "Fixture Org",
		Email:        d.label,
		Issuer:       "https://issuer.fixture.invalid",
		Subject:      "fixture-subject",
		IDToken:      "fixture-id-token-must-not-leak",
		Scopes:       []string{"fixture-secret-scope"},
	}, nil
}

func (d *credentialAPIDriver) Refresh(context.Context, subscription.TokenSet) (subscription.TokenSet, error) {
	return subscription.TokenSet{}, errors.New("fixture refresh is not used")
}

func (d *credentialAPIDriver) Revoke(context.Context, subscription.TokenSet) error {
	d.mu.Lock()
	d.revocations++
	d.mu.Unlock()
	return subscription.ErrRevocationUnsupported
}

type nativeHTTPFixture struct {
	t                    *testing.T
	server               *Server
	h                    http.Handler
	m                    *Manager
	token                string
	driver               *credentialAPIDriver
	label                string
	cleanupProfileIDs    []int64
	cleanupCredentialIDs []int64
}

func newNativeHTTPFixture(t *testing.T) *nativeHTTPFixture {
	t.Helper()
	if _, _, err := db.DSN(); err != nil {
		t.Skipf("PostgreSQL is not configured: %v", err)
	}
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("configured PostgreSQL initialization failed: %v", err)
	}
	port := reserveCredentialAPIPort(t)
	stamp := fmt.Sprintf("native-api-%d-%d@fixture.invalid", os.Getpid(), time.Now().UnixNano())
	driver := &credentialAPIDriver{
		kind: subscription.ProviderClaude,
		callback: subscription.CallbackConfig{
			RedirectURI: fmt.Sprintf("http://127.0.0.1:%d/callback", port),
			Port:        port,
			Path:        "/callback",
		},
		label: stamp,
	}
	if existing := m.Subscriptions(); existing != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		if err := existing.Close(closeCtx); err != nil {
			closeCancel()
			_ = m.Close()
			t.Fatalf("close production subscription manager for fixture: %v", err)
		}
		closeCancel()
	}
	m.mu.Lock()
	m.subscriptions = subscription.NewManager(m.pg, subscription.Options{
		Drivers:            []subscription.OAuthDriver{driver},
		CallbackListenHost: "127.0.0.1",
	})
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	token, err := signJWT(s.jwtKey)
	if err != nil {
		cancel()
		m.Close()
		t.Fatalf("sign ARTEX JWT: %v", err)
	}
	fixture := &nativeHTTPFixture{t: t, server: s, h: s.Handler(), m: m, token: token, driver: driver, label: stamp}
	t.Cleanup(func() {
		stopTestServer(t, s, cancel)
		for _, profileID := range fixture.cleanupProfileIDs {
			_, _ = m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID)
		}
		for _, credentialID := range fixture.cleanupCredentialIDs {
			_, _ = m.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credentialID)
		}
		_, _ = m.pg.Exec(`DELETE FROM llm_credentials WHERE label=$1`, stamp)
		m.Close()
	})
	return fixture
}

func reserveCredentialAPIPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve callback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release callback port: %v", err)
	}
	return port
}

func (f *nativeHTTPFixture) request(method, path string, body any, token string) *httptestResponse {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, "http://artex.invalid"+path, reader)
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := newHTTPTestResponse()
	f.h.ServeHTTP(response, request)
	return response
}

// httptestResponse is the small ResponseRecorder surface these API tests need.
// Keeping the parsed helpers here makes accidental secret assertions explicit.
type httptestResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newHTTPTestResponse() *httptestResponse {
	return &httptestResponse{header: make(http.Header), status: http.StatusOK}
}

func (r *httptestResponse) Header() http.Header { return r.header }

func (r *httptestResponse) Write(payload []byte) (int, error) { return r.body.Write(payload) }

func (r *httptestResponse) WriteHeader(status int) { r.status = status }

func (r *httptestResponse) mapBody(t *testing.T) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(r.body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %d %q: %v", r.status, r.body.String(), err)
	}
	return value
}

func (r *httptestResponse) arrayBody(t *testing.T) []map[string]any {
	t.Helper()
	var value []map[string]any
	if err := json.Unmarshal(r.body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %d %q: %v", r.status, r.body.String(), err)
	}
	return value
}

func signedFixtureJWT(t *testing.T, key []byte, id string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "ARTEX",
		ID:        id,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}).SignedString(key)
	if err != nil {
		t.Fatalf("sign alternate ARTEX JWT: %v", err)
	}
	return token
}

func attemptState(t *testing.T, attempt map[string]any) string {
	t.Helper()
	authorizationURL, _ := attempt["authorization_url"].(string)
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("authorization URL omitted state")
	}
	return state
}

func TestLLMCredentialProtectedMuxLifecycleAndSecretBoundary(t *testing.T) {
	f := newNativeHTTPFixture(t)

	if response := f.request(http.MethodGet, "/api/llm/credentials", nil, ""); response.status != http.StatusUnauthorized {
		t.Fatalf("unprotected credential list: HTTP %d %s", response.status, response.body.String())
	}
	login := f.request(http.MethodPost, "/api/llm/credentials/login", map[string]any{"provider": subscription.ProviderClaude}, f.token)
	if login.status != http.StatusOK {
		t.Fatalf("begin login: HTTP %d %s", login.status, login.body.String())
	}
	attempt := login.mapBody(t)
	if attempt["status"] != subscription.AttemptPending || attempt["authorization_url"] == "" || attempt["callback_url"] == "" {
		t.Fatalf("pending attempt = %#v", attempt)
	}
	attemptID := attempt["id"].(string)
	state := attemptState(t, attempt)
	callbackURL := attempt["callback_url"].(string)

	otherToken := signedFixtureJWT(t, f.server.jwtKey, "different-session")
	ownerMismatch := f.request(http.MethodGet, "/api/llm/credentials/attempt/"+attemptID, nil, otherToken)
	if ownerMismatch.status != http.StatusNotFound || ownerMismatch.mapBody(t)["code"] != subscription.CodeAttemptOwnerMismatch {
		t.Fatalf("owner mismatch: HTTP %d %s", ownerMismatch.status, ownerMismatch.body.String())
	}

	wrongState := f.request(http.MethodPost, "/api/llm/credentials/attempt/"+attemptID+"/submit", map[string]any{
		"redirect_url": callbackURL + "?code=wrong-state-secret-code&state=wrong",
	}, f.token)
	if wrongState.status != http.StatusBadRequest || wrongState.mapBody(t)["code"] != subscription.CodeStateMismatch {
		t.Fatalf("wrong state: HTTP %d %s", wrongState.status, wrongState.body.String())
	}
	if strings.Contains(wrongState.body.String(), "wrong-state-secret-code") {
		t.Fatalf("redirect code leaked in error: %s", wrongState.body.String())
	}

	complete := f.request(http.MethodPost, "/api/llm/credentials/attempt/"+attemptID+"/submit", map[string]any{
		"redirect_url": callbackURL + "?code=manual-success-secret-code&state=" + url.QueryEscape(state),
	}, f.token)
	if complete.status != http.StatusOK {
		t.Fatalf("manual submit: HTTP %d %s", complete.status, complete.body.String())
	}
	completedAttempt := complete.mapBody(t)
	if completedAttempt["status"] != subscription.AttemptComplete || completedAttempt["credential_id"] == nil {
		t.Fatalf("completed attempt = %#v", completedAttempt)
	}
	if _, present := completedAttempt["authorization_url"]; present {
		t.Fatalf("terminal attempt retained authorization URL: %#v", completedAttempt)
	}
	if _, present := completedAttempt["callback_url"]; present {
		t.Fatalf("terminal attempt retained callback URL: %#v", completedAttempt)
	}
	credentialID := int64(completedAttempt["credential_id"].(float64))

	poll := f.request(http.MethodGet, "/api/llm/credentials/attempt/"+attemptID, nil, f.token)
	if poll.status != http.StatusOK || poll.mapBody(t)["status"] != subscription.AttemptComplete {
		t.Fatalf("poll completed attempt: HTTP %d %s", poll.status, poll.body.String())
	}
	list := f.request(http.MethodGet, "/api/llm/credentials", nil, f.token)
	if list.status != http.StatusOK {
		t.Fatalf("list credentials: HTTP %d %s", list.status, list.body.String())
	}
	credentials := list.arrayBody(t)
	if len(credentials) == 0 {
		t.Fatal("completed login did not create a credential")
	}
	for _, forbidden := range []string{"fixture-access-token", "fixture-refresh-token", "fixture-id-token", "fixture-secret-scope", "manual-success-secret-code", "payload", "scopes"} {
		if strings.Contains(list.body.String(), forbidden) || strings.Contains(complete.body.String(), forbidden) {
			t.Fatalf("credential HTTP artifact leaked %q: list=%s complete=%s", forbidden, list.body.String(), complete.body.String())
		}
	}

	disconnect := f.request(http.MethodPost, fmt.Sprintf("/api/llm/credentials/%d/disconnect", credentialID), map[string]any{}, f.token)
	if disconnect.status != http.StatusOK {
		t.Fatalf("disconnect: HTTP %d %s", disconnect.status, disconnect.body.String())
	}
	disconnected := disconnect.mapBody(t)
	if disconnected["status"] != subscription.CredentialRevoked || disconnected["remote_revocation_confirmed"] != false || disconnected["warning"] == "" {
		t.Fatalf("unsupported remote revoke was reported as success: %#v", disconnected)
	}
	stored, err := f.m.pg.LLMCredential(t.Context(), credentialID)
	if err != nil || stored == nil || stored.Status != subscription.CredentialRevoked {
		t.Fatalf("local revocation was not durable: credential=%#v err=%v", stored, err)
	}
}

func TestLLMCredentialCallbackAndProvider401StaySeparateFromARTEXAuth(t *testing.T) {
	f := newNativeHTTPFixture(t)

	beginCallback := f.request(http.MethodPost, "/api/llm/credentials/login", map[string]any{"provider": subscription.ProviderClaude}, f.token)
	attempt := beginCallback.mapBody(t)
	state := attemptState(t, attempt)
	callbackURL := attempt["callback_url"].(string) + "?code=callback-success-secret-code&state=" + url.QueryEscape(state)
	client := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	callbackResponse, err := client.Get(callbackURL)
	if err != nil {
		t.Fatalf("provider callback: %v", err)
	}
	callbackBody, _ := io.ReadAll(callbackResponse.Body)
	_ = callbackResponse.Body.Close()
	if callbackResponse.StatusCode != http.StatusOK || strings.Contains(string(callbackBody), "callback-success-secret-code") {
		t.Fatalf("callback response: HTTP %d %q", callbackResponse.StatusCode, callbackBody)
	}
	poll := f.request(http.MethodGet, "/api/llm/credentials/attempt/"+attempt["id"].(string), nil, f.token)
	if poll.status != http.StatusOK || poll.mapBody(t)["status"] != subscription.AttemptComplete {
		t.Fatalf("callback attempt did not complete: HTTP %d %s", poll.status, poll.body.String())
	}

	beginFailure := f.request(http.MethodPost, "/api/llm/credentials/login", map[string]any{"provider": subscription.ProviderClaude}, f.token)
	failedAttempt := beginFailure.mapBody(t)
	failureState := attemptState(t, failedAttempt)
	failure := f.request(http.MethodPost, "/api/llm/credentials/attempt/"+failedAttempt["id"].(string)+"/submit", map[string]any{
		"redirect_url": failedAttempt["callback_url"].(string) + "?code=provider-401-secret-code&state=" + url.QueryEscape(failureState),
	}, f.token)
	if failure.status != http.StatusBadGateway || failure.mapBody(t)["code"] != subscription.CodeAuthorizationFailed {
		t.Fatalf("provider 401 crossed ARTEX auth boundary: HTTP %d %s", failure.status, failure.body.String())
	}
	if strings.Contains(failure.body.String(), "provider-401-secret-code") {
		t.Fatalf("provider failure leaked redirect code: %s", failure.body.String())
	}
	stillAuthenticated := f.request(http.MethodGet, "/api/llm/credentials", nil, f.token)
	if stillAuthenticated.status != http.StatusOK {
		t.Fatalf("provider 401 invalidated ARTEX login: HTTP %d %s", stillAuthenticated.status, stillAuthenticated.body.String())
	}
}
