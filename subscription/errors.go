package subscription

import (
	"errors"
	"fmt"
	"time"
)

const (
	CodeInvalidRequest         = "subscription_invalid_request"
	CodeProviderUnsupported    = "subscription_provider_unsupported"
	CodeManagerClosed          = "subscription_manager_closed"
	CodeAttemptNotFound        = "subscription_attempt_not_found"
	CodeAttemptOwnerMismatch   = "subscription_attempt_owner_mismatch"
	CodeAttemptExpired         = "subscription_attempt_expired"
	CodeRedirectMismatch       = "subscription_redirect_mismatch"
	CodeStateMismatch          = "subscription_state_mismatch"
	CodeAuthorizationFailed    = "subscription_authorization_failed"
	CodeCredentialNotFound     = "subscription_credential_not_found"
	CodeCredentialInvalid      = "subscription_credential_invalid"
	CodeAuthRequired           = "subscription_auth_required"
	CodeConsentRequired        = "subscription_consent_required"
	CodeSubscriptionLimit      = "subscription_limit"
	CodeRevoked                = "subscription_revoked"
	CodeBindingChanged         = "subscription_binding_changed"
	CodeIdentityChanged        = "subscription_identity_changed"
	CodeRefreshFailed          = "subscription_refresh_failed"
	CodeCredentialChanged      = "subscription_credential_changed"
	CodeRevocationUnsupported  = "subscription_revocation_unsupported"
	CodeRevocationNotConfirmed = "subscription_revocation_not_confirmed"
	CodeCallbackUnavailable    = "subscription_callback_unavailable"
	CodeCredentialPersistence  = "subscription_credential_persistence_failed"
	CodeTaskRecoveryRequired   = "subscription_task_recovery_required"
)

// Error is a sanitized, terminal subscription failure. Message must be safe
// for authenticated API responses and logs: provider response bodies, URLs,
// codes, verifiers, and tokens must never be copied into it.
type Error struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Status    int       `json:"status,omitempty"`
	ResetAt   time.Time `json:"reset_at,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "subscription error"
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NoAutoRetry prevents native provider failures from being replayed after an
// upstream request may already have consumed subscription capacity.
func (*Error) NoAutoRetry() bool { return true }

// NoAutoFallback prevents a selected subscription from silently falling back
// to a paid API key or another account.
func (*Error) NoAutoFallback() bool { return true }

var ErrRevocationUnsupported = &Error{
	Code:    CodeRevocationUnsupported,
	Message: "the provider does not support remote revocation",
}

type terminalError interface {
	error
	NoAutoRetry() bool
	NoAutoFallback() bool
}

// IsTerminal reports whether err explicitly disables both automatic replay
// and fallback. Wrapped subscription errors retain this property.
func IsTerminal(err error) bool {
	var terminal terminalError
	return errors.As(err, &terminal) && terminal.NoAutoRetry() && terminal.NoAutoFallback()
}

func newError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}
