package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/subscription"
)

// credentialOwner binds an authorization attempt to the exact validated ARTEX
// session without retaining the bearer token itself.
func (s *Server) credentialOwner(r *http.Request) (string, bool) {
	token := extractToken(r)
	if token == "" || !s.sessionOK(token) {
		return "", false
	}
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:]), true
}

func (s *Server) subscriptionRequest(w http.ResponseWriter, r *http.Request) (*subscription.Manager, string, bool) {
	w.Header().Set("Cache-Control", "no-store")
	owner, ok := s.credentialOwner(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Unauthorized")
		return nil, "", false
	}
	if s.m == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "native subscription authentication is unavailable",
			"code":  subscription.CodeManagerClosed,
		})
		return nil, "", false
	}
	manager := s.m.Subscriptions()
	if manager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "native subscription authentication is unavailable",
			"code":  subscription.CodeManagerClosed,
		})
		return nil, "", false
	}
	return manager, owner, true
}

func (s *Server) listLLMCredentials(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.subscriptionRequest(w, r); !ok {
		return
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	credentials, err := pg.ListLLMCredentials(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list LLM credentials")
		return
	}
	out := make([]CredentialDTO, 0, len(credentials))
	for _, credential := range credentials {
		out = append(out, credentialDTO(credential))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) loginLLMCredential(w http.ResponseWriter, r *http.Request) {
	manager, owner, ok := s.subscriptionRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Provider     string `json:"provider"`
		CredentialID *int64 `json:"credential_id"`
	}
	if err := decode(w, r, &body); err != nil {
		writeSubscriptionError(w, &subscription.Error{Code: subscription.CodeInvalidRequest, Message: "invalid credential login request"})
		return
	}
	credentialID := int64(0)
	if body.CredentialID != nil {
		credentialID = *body.CredentialID
	}
	attempt, err := manager.Begin(r.Context(), owner, strings.TrimSpace(body.Provider), credentialID)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attemptDTO(attempt))
}

func (s *Server) getLLMCredentialAttempt(w http.ResponseWriter, r *http.Request) {
	manager, owner, ok := s.subscriptionRequest(w, r)
	if !ok {
		return
	}
	attempt, err := manager.Attempt(r.Context(), owner, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attemptDTO(attempt))
}

func (s *Server) submitLLMCredentialAttempt(w http.ResponseWriter, r *http.Request) {
	manager, owner, ok := s.subscriptionRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := decode(w, r, &body); err != nil {
		writeSubscriptionError(w, &subscription.Error{Code: subscription.CodeInvalidRequest, Message: "invalid authorization redirect submission"})
		return
	}
	attempt, err := manager.Submit(r.Context(), owner, strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(body.RedirectURL))
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attemptDTO(attempt))
}

func (s *Server) disconnectLLMCredential(w http.ResponseWriter, r *http.Request) {
	manager, _, ok := s.subscriptionRequest(w, r)
	if !ok {
		return
	}
	credentialID, valid := pathInt(r, "id")
	if !valid || credentialID <= 0 {
		writeSubscriptionError(w, &subscription.Error{Code: subscription.CodeInvalidRequest, Message: "credential ID is invalid"})
		return
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	credential, err := pg.LLMCredential(r.Context(), credentialID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read LLM credential")
		return
	}
	if credential == nil {
		writeSubscriptionError(w, &subscription.Error{Code: subscription.CodeCredentialNotFound, Message: "credential was not found"})
		return
	}
	if credential.Status == subscription.CredentialRevoked {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":                      subscription.CredentialRevoked,
			"remote_revocation_confirmed": false,
			"warning":                     "credential was already revoked locally; this request did not confirm remote revocation",
		})
		return
	}
	if err := manager.Disconnect(r.Context(), credentialID); err != nil {
		var subscriptionErr *subscription.Error
		if errors.As(err, &subscriptionErr) && (subscriptionErr.Code == subscription.CodeRevocationUnsupported || subscriptionErr.Code == subscription.CodeRevocationNotConfirmed) {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":                      subscription.CredentialRevoked,
				"remote_revocation_confirmed": false,
				"warning":                     subscriptionErr.Message,
			})
			return
		}
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                      subscription.CredentialRevoked,
		"remote_revocation_confirmed": true,
	})
}

func writeSubscriptionError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	code := "subscription_operation_failed"
	message := "subscription operation failed"
	var subscriptionErr *subscription.Error
	if errors.As(err, &subscriptionErr) {
		code = subscriptionErr.Code
		message = subscriptionErr.Message
		switch subscriptionErr.Code {
		case subscription.CodeInvalidRequest,
			subscription.CodeProviderUnsupported,
			subscription.CodeRedirectMismatch,
			subscription.CodeStateMismatch:
			status = http.StatusBadRequest
		case subscription.CodeAttemptNotFound,
			subscription.CodeAttemptOwnerMismatch,
			subscription.CodeCredentialNotFound:
			status = http.StatusNotFound
		case subscription.CodeAttemptExpired,
			subscription.CodeCredentialInvalid,
			subscription.CodeAuthRequired,
			subscription.CodeConsentRequired,
			subscription.CodeSubscriptionLimit,
			subscription.CodeRevoked,
			subscription.CodeBindingChanged,
			subscription.CodeIdentityChanged,
			subscription.CodeCredentialChanged:
			status = http.StatusConflict
		case subscription.CodeManagerClosed:
			status = http.StatusServiceUnavailable
		case subscription.CodeCredentialPersistence:
			status = http.StatusInternalServerError
		default:
			// Provider authorization, callback transport, refresh, and remote
			// revocation failures are upstream failures. In particular, an
			// upstream 401 must never become an ARTEX HTTP 401.
			status = http.StatusBadGateway
		}
	}
	if strings.TrimSpace(message) == "" {
		message = "subscription operation failed"
	}
	body := map[string]any{"error": message}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}
