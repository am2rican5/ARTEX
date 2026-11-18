package subscription

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Autumn-27/artex/db"
)

const (
	ProviderClaude = "anthropic-oauth"
	ProviderCodex  = "openai-codex-oauth"

	CredentialReady          = "ready"
	CredentialReauthRequired = "reauth_required"
	CredentialRevoked        = "revoked"

	AttemptPending  = "pending"
	AttemptComplete = "complete"
	AttemptFailed   = "failed"
)

// TokenSet is the provider credential material encrypted in the credential
// store. Callers must never log or expose a TokenSet.
type TokenSet struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
	OrgID        string    `json:"org_id,omitempty"`
	OrgName      string    `json:"org_name,omitempty"`
	Email        string    `json:"email,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	Issuer       string    `json:"issuer,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
}

// Binding identifies the exact profile-to-credential generation used to build
// a provider. Old provider instances are rejected after the binding changes.
type Binding struct {
	ProfileID    int64
	CredentialID int64
	Generation   int64
}

// Snapshot is a short-lived credential view for a single model request.
// Tokens must not be retained by a long-lived provider instance.
type Snapshot struct {
	Provider  string
	Tokens    TokenSet
	AuthScope string
}

// CallbackConfig is the provider's fixed loopback redirect contract.
type CallbackConfig struct {
	RedirectURI string
	Port        int
	Path        string
}

// CodeExchange is the manager-owned, immutable input for one authorization
// code exchange. Code and Verifier are secret material; drivers must consume
// the input only during Exchange and must never retain, log, or expose it.
type CodeExchange struct {
	Code        string
	State       string
	Verifier    string
	RedirectURI string
}

// OAuthDriver implements one provider's fixed OAuth protocol. Implementations
// are responsible for using only their provider's fixed authorization, token,
// and revocation endpoints.
type OAuthDriver interface {
	Kind() string
	Callback() CallbackConfig
	AuthorizationURL(state, challenge string) string
	Exchange(ctx context.Context, input CodeExchange) (TokenSet, error)
	Refresh(ctx context.Context, old TokenSet) (TokenSet, error)
	Revoke(ctx context.Context, old TokenSet) error
}

// Store is the narrow encrypted credential persistence contract used by the
// manager. *db.DB implements this interface.
type Store interface {
	CreateLLMCredential(ctx context.Context, provider, label string, payload json.RawMessage) (*db.LLMCredential, error)
	LLMCredential(ctx context.Context, id int64) (*db.LLMCredential, error)
	ListLLMCredentials(ctx context.Context) ([]db.LLMCredential, error)
	UpdateLLMCredential(ctx context.Context, credential *db.LLMCredential, expectedRevision int64) error
	LockLLMCredential(ctx context.Context, id int64) (unlock func(), err error)
	ProfileCredentialBinding(ctx context.Context, profileID int64) (credentialID, generation int64, err error)
}

var _ Store = (*db.DB)(nil)

// Options configures manager-owned OAuth drivers and callback listeners.
type Options struct {
	Drivers            []OAuthDriver
	CallbackListenHost string
	Now                func() time.Time
}

// Attempt is the secret-free authorization state exposed to authenticated
// management callers. AuthorizationURL intentionally contains OAuth state, but
// never a code, verifier, or token.
type Attempt struct {
	ID               string    `json:"id"`
	Provider         string    `json:"provider"`
	Status           string    `json:"status"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	CallbackURL      string    `json:"callback_url"`
	CredentialID     int64     `json:"credential_id,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
	Error            string    `json:"error,omitempty"`
}
