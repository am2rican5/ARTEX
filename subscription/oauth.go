package subscription

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const credentialPayloadVersion = 1

type credentialPayload struct {
	Version   int      `json:"version"`
	Provider  string   `json:"provider"`
	AuthScope string   `json:"auth_scope"`
	Tokens    TokenSet `json:"tokens"`
}

func newPKCE() (state, verifier, challenge string, err error) {
	state, err = randomURLToken(32)
	if err != nil {
		return "", "", "", err
	}
	verifier, err = randomURLToken(48)
	if err != nil {
		return "", "", "", err
	}
	digest := sha256.Sum256([]byte(verifier))
	return state, verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func randomURLToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func validateCallbackConfig(config CallbackConfig) (*url.URL, error) {
	if config.Port < 1 || config.Port > 65535 || config.Path == "" || !strings.HasPrefix(config.Path, "/") {
		return nil, newError(CodeInvalidRequest, "provider callback configuration is invalid")
	}
	parsed, err := url.Parse(config.RedirectURI)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, newError(CodeInvalidRequest, "provider callback configuration is invalid")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port != config.Port || parsed.EscapedPath() != config.Path {
		return nil, newError(CodeInvalidRequest, "provider callback configuration is inconsistent")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, newError(CodeInvalidRequest, "provider callback must use a loopback redirect")
	}
	return parsed, nil
}

func validateAuthorizationURL(rawURL, state string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() || parsed.User != nil || parsed.Fragment != "" {
		return newError(CodeAuthorizationFailed, "provider authorization URL is invalid")
	}
	values := parsed.Query()
	states := values["state"]
	if len(states) != 1 || !constantTimeEqual(states[0], state) {
		return newError(CodeAuthorizationFailed, "provider authorization URL is not bound to the attempt")
	}
	// Claude's authorization endpoint uses code=true as a public protocol flag,
	// not an issued authorization code. No other code value or origin is safe.
	if codes, present := values["code"]; present {
		if parsed.Scheme != "https" || parsed.Host != "claude.ai" || parsed.Path != "/oauth/authorize" || len(codes) != 1 || codes[0] != "true" {
			return newError(CodeAuthorizationFailed, "provider authorization URL contains credential material")
		}
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if _, present := values[key]; present {
			return newError(CodeAuthorizationFailed, "provider authorization URL contains credential material")
		}
	}
	return nil
}

func validateRedirect(rawURL string, expected *url.URL) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() || parsed.User != nil || parsed.Fragment != "" {
		return nil, newError(CodeRedirectMismatch, "redirect URL does not match the authorization attempt")
	}
	if parsed.Scheme != expected.Scheme || parsed.Host != expected.Host || parsed.EscapedPath() != expected.EscapedPath() {
		return nil, newError(CodeRedirectMismatch, "redirect URL does not match the authorization attempt")
	}
	return parsed, nil
}

func validateIssuedTokens(tokens TokenSet, now time.Time) error {
	if tokens.AccessToken == "" || tokens.ExpiresAt.IsZero() || !tokens.ExpiresAt.After(now) {
		return newError(CodeAuthorizationFailed, "provider returned unusable credentials")
	}
	if tokens.AccountID == "" && tokens.Subject == "" {
		return newError(CodeAuthorizationFailed, "provider did not return a stable account identity")
	}
	return nil
}

func mergeTokenSets(old, next TokenSet, now time.Time) (TokenSet, error) {
	if err := validateIssuedTokens(next, now); err != nil {
		return TokenSet{}, err
	}
	if identityFieldChanged(old.AccountID, next.AccountID) ||
		identityFieldChanged(old.OrgID, next.OrgID) ||
		identityFieldChanged(old.ClientID, next.ClientID) ||
		identityFieldChanged(old.Issuer, next.Issuer) ||
		identityFieldChanged(old.Subject, next.Subject) {
		return TokenSet{}, newError(CodeIdentityChanged, "provider account identity changed during credential update")
	}
	if old.AccountID != "" || old.Subject != "" {
		accountMatches := old.AccountID != "" && next.AccountID != "" && constantTimeEqual(old.AccountID, next.AccountID)
		subjectMatches := old.Subject != "" && next.Subject != "" && constantTimeEqual(old.Subject, next.Subject)
		if !accountMatches && !subjectMatches {
			return TokenSet{}, newError(CodeIdentityChanged, "provider did not confirm the existing account identity")
		}
	}

	merged := next
	if merged.RefreshToken == "" {
		merged.RefreshToken = old.RefreshToken
	}
	if merged.IDToken == "" {
		merged.IDToken = old.IDToken
	}
	if merged.AccountID == "" {
		merged.AccountID = old.AccountID
	}
	if merged.OrgID == "" {
		merged.OrgID = old.OrgID
	}
	if merged.OrgName == "" {
		merged.OrgName = old.OrgName
	}
	if merged.Email == "" {
		merged.Email = old.Email
	}
	if merged.ClientID == "" {
		merged.ClientID = old.ClientID
	}
	if merged.Issuer == "" {
		merged.Issuer = old.Issuer
	}
	if merged.Subject == "" {
		merged.Subject = old.Subject
	}
	if len(merged.Scopes) == 0 {
		merged.Scopes = append([]string(nil), old.Scopes...)
	} else {
		merged.Scopes = append([]string(nil), merged.Scopes...)
	}
	if merged.AccountID == "" && merged.Subject == "" {
		return TokenSet{}, newError(CodeIdentityChanged, "credential has no stable account identity")
	}
	return merged, nil
}

func identityFieldChanged(oldValue, newValue string) bool {
	return oldValue != "" && newValue != "" && !constantTimeEqual(oldValue, newValue)
}

func authScope(provider string, tokens TokenSet) string {
	identity := struct {
		Provider  string `json:"provider"`
		Issuer    string `json:"issuer"`
		ClientID  string `json:"client_id"`
		AccountID string `json:"account_id"`
		OrgID     string `json:"org_id"`
		Subject   string `json:"subject"`
	}{provider, tokens.Issuer, tokens.ClientID, tokens.AccountID, tokens.OrgID, tokens.Subject}
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func redactTokens(tokens TokenSet) TokenSet {
	return TokenSet{
		AccountID: tokens.AccountID,
		OrgID:     tokens.OrgID,
		OrgName:   tokens.OrgName,
		Email:     tokens.Email,
		ClientID:  tokens.ClientID,
		Issuer:    tokens.Issuer,
		Subject:   tokens.Subject,
		Scopes:    append([]string(nil), tokens.Scopes...),
	}
}
