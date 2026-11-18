package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

const (
	authorizationTTL         = 10 * time.Minute
	maxAuthorizationAttempts = 1024
	refreshSkew              = 30 * time.Second
	operationTimeout         = 30 * time.Second
	listenerStopLimit        = 5 * time.Second
)

type Manager struct {
	store          Store
	drivers        map[string]OAuthDriver
	listenHost     string
	now            func() time.Time
	scheduleExpiry func(time.Duration, func()) authorizationTimer
	workCtx        context.Context
	workCancel     context.CancelFunc

	mu              sync.Mutex
	closed          bool
	initErr         *Error
	closeDone       chan struct{}
	attempts        map[string]*authorizationAttempt
	attemptStates   map[string]*authorizationAttempt
	listeners       map[int]*callbackListener
	refreshes       map[int64]*refreshCall
	disconnects     map[int64]*disconnectCall
	gates           map[int64]*credentialGate
	requests        map[int64]map[uint64]context.CancelFunc
	nextRequestID   uint64
	backgroundWorks sync.WaitGroup
}

type authorizationAttempt struct {
	public          Attempt
	owner           string
	state           string
	verifier        string
	redirect        *url.URL
	driver          OAuthDriver
	listener        *callbackListener
	expiryTimer     authorizationTimer
	baseRevision    int64
	baseStatus      string
	exchangeStarted bool
	terminalErr     error
	done            chan struct{}
}

type authorizationTimer interface {
	Stop() bool
}

type authorizationExchange struct {
	attempt      *authorizationAttempt
	driver       OAuthDriver
	input        CodeExchange
	provider     string
	credentialID int64
	baseRevision int64
	baseStatus   string
}

type callbackListener struct {
	server   *http.Server
	listener net.Listener
	port     int
	pending  int
	stopOnce sync.Once
}

type refreshCall struct {
	done     chan struct{}
	snapshot Snapshot
	err      error
}

type disconnectCall struct {
	done chan struct{}
	err  error
}

type credentialGate struct {
	permit chan struct{}
}

func newCredentialGate() *credentialGate {
	gate := &credentialGate{permit: make(chan struct{}, 1)}
	gate.permit <- struct{}{}
	return gate
}

func (g *credentialGate) acquire(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.permit:
		var once sync.Once
		return func() { once.Do(func() { g.permit <- struct{}{} }) }, nil
	}
}

// NewManager constructs a shared credential manager. It does not bind callback
// ports; the exact provider listener is started only by Begin.
func NewManager(store Store, opts Options) *Manager {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	listenHost := opts.CallbackListenHost
	if listenHost == "" {
		listenHost = "127.0.0.1"
	}
	workCtx, workCancel := context.WithCancel(context.Background())
	manager := &Manager{
		store:      store,
		drivers:    make(map[string]OAuthDriver, len(opts.Drivers)),
		listenHost: listenHost,
		now:        now,
		scheduleExpiry: func(delay time.Duration, expire func()) authorizationTimer {
			return time.AfterFunc(delay, expire)
		},
		workCtx:       workCtx,
		workCancel:    workCancel,
		closeDone:     make(chan struct{}),
		attempts:      make(map[string]*authorizationAttempt),
		attemptStates: make(map[string]*authorizationAttempt),
		listeners:     make(map[int]*callbackListener),
		refreshes:     make(map[int64]*refreshCall),
		disconnects:   make(map[int64]*disconnectCall),
		gates:         make(map[int64]*credentialGate),
		requests:      make(map[int64]map[uint64]context.CancelFunc),
	}
	if store == nil {
		manager.initErr = newError(CodeInvalidRequest, "credential store is required")
	}
	for _, driver := range opts.Drivers {
		if driver == nil {
			manager.initErr = newError(CodeInvalidRequest, "OAuth driver is required")
			continue
		}
		kind := driver.Kind()
		if kind != ProviderClaude && kind != ProviderCodex {
			manager.initErr = newError(CodeProviderUnsupported, "OAuth driver kind is unsupported")
			continue
		}
		if _, exists := manager.drivers[kind]; exists {
			manager.initErr = newError(CodeInvalidRequest, "OAuth driver kind is duplicated")
			continue
		}
		manager.drivers[kind] = driver
	}
	return manager
}

// Begin creates a browser authorization attempt owned by the authenticated
// caller. credentialID zero creates a new identity; a positive ID reauthenticates
// that exact credential and may not replace it with a different account.
func (m *Manager) Begin(ctx context.Context, owner, provider string, credentialID int64) (Attempt, error) {
	if err := ctx.Err(); err != nil {
		return Attempt{}, err
	}
	if owner == "" || credentialID < 0 {
		return Attempt{}, newError(CodeInvalidRequest, "authorization owner and credential ID are invalid")
	}

	m.mu.Lock()
	if err := m.openErrorLocked(); err != nil {
		m.mu.Unlock()
		return Attempt{}, err
	}
	driver := m.drivers[provider]
	m.mu.Unlock()
	if driver == nil {
		return Attempt{}, newError(CodeProviderUnsupported, "subscription provider is unsupported")
	}

	var baseRevision int64
	var baseStatus string
	if credentialID != 0 {
		record, err := m.store.LLMCredential(ctx, credentialID)
		if err != nil {
			return Attempt{}, persistenceError(ctx, err)
		}
		if record == nil {
			return Attempt{}, newError(CodeCredentialNotFound, "credential was not found")
		}
		if record.Provider != provider {
			return Attempt{}, newError(CodeIdentityChanged, "credential belongs to a different provider")
		}
		if _, err := decodeCredential(record, provider); err != nil {
			return Attempt{}, err
		}
		baseRevision = record.Revision
		baseStatus = record.Status
	}

	callback := driver.Callback()
	redirect, err := validateCallbackConfig(callback)
	if err != nil {
		return Attempt{}, err
	}
	state, verifier, challenge, err := newPKCE()
	if err != nil {
		return Attempt{}, newError(CodeAuthorizationFailed, "could not create a secure authorization attempt")
	}
	attemptID, err := randomURLToken(24)
	if err != nil {
		return Attempt{}, newError(CodeAuthorizationFailed, "could not create a secure authorization attempt")
	}
	authorizationURL := driver.AuthorizationURL(state, challenge)
	if err := validateAuthorizationURL(authorizationURL, state); err != nil {
		return Attempt{}, err
	}

	attempt := &authorizationAttempt{
		public: Attempt{
			ID:               attemptID,
			Provider:         provider,
			Status:           AttemptPending,
			AuthorizationURL: authorizationURL,
			CallbackURL:      callback.RedirectURI,
			CredentialID:     credentialID,
			ExpiresAt:        m.now().Add(authorizationTTL),
		},
		owner:        owner,
		state:        state,
		verifier:     verifier,
		redirect:     redirect,
		driver:       driver,
		baseRevision: baseRevision,
		baseStatus:   baseStatus,
		done:         make(chan struct{}),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.openErrorLocked(); err != nil {
		return Attempt{}, err
	}
	m.pruneAttemptsLocked(m.now())
	if len(m.attempts) >= maxAuthorizationAttempts {
		return Attempt{}, newError(CodeInvalidRequest, "too many authorization attempts are active")
	}
	listener, err := m.startListenerLocked(callback.Port)
	if err != nil {
		return Attempt{}, err
	}
	attempt.listener = listener
	m.attempts[attemptID] = attempt
	m.attemptStates[state] = attempt
	m.scheduleAttemptExpiryLocked(attempt)
	return attempt.public, nil
}

// Attempt returns an authorization attempt only to its authenticated owner.
func (m *Manager) Attempt(ctx context.Context, owner, id string) (Attempt, error) {
	if err := ctx.Err(); err != nil {
		return Attempt{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.openErrorLocked(); err != nil {
		return Attempt{}, err
	}
	attempt, err := m.ownedAttemptLocked(owner, id)
	if err != nil {
		return Attempt{}, err
	}
	m.expireAttemptLocked(attempt, m.now())
	return attempt.public, nil
}

// Submit accepts an authenticated manual final redirect URL. A bare code is
// deliberately unsupported because it is not bound to callback, state, PKCE,
// expiry, and owner.
func (m *Manager) Submit(ctx context.Context, owner, attemptID, redirectURL string) (Attempt, error) {
	return m.submit(ctx, owner, attemptID, redirectURL)
}

func (m *Manager) submit(ctx context.Context, owner, attemptID, redirectURL string) (Attempt, error) {
	if err := ctx.Err(); err != nil {
		return Attempt{}, err
	}

	m.mu.Lock()
	if err := m.openErrorLocked(); err != nil {
		m.mu.Unlock()
		return Attempt{}, err
	}
	attempt, err := m.ownedAttemptLocked(owner, attemptID)
	if err != nil {
		m.mu.Unlock()
		return Attempt{}, err
	}
	if attempt.public.Status != AttemptPending {
		result, terminalErr := attempt.public, attempt.terminalErr
		m.mu.Unlock()
		return result, terminalErr
	}
	parsed, err := validateRedirect(redirectURL, attempt.redirect)
	if err != nil {
		result := attempt.public
		m.mu.Unlock()
		return result, err
	}
	values := parsed.Query()
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if _, present := values[key]; present {
			result := attempt.public
			m.mu.Unlock()
			return result, newError(CodeInvalidRequest, "redirect URL contains unexpected credential material")
		}
	}
	states := values["state"]
	if len(states) != 1 || !constantTimeEqual(states[0], attempt.state) {
		result := attempt.public
		m.mu.Unlock()
		return result, newError(CodeStateMismatch, "redirect state does not match the authorization attempt")
	}
	if !attempt.exchangeStarted {
		if !m.now().Before(attempt.public.ExpiresAt) {
			terminalErr := newError(CodeAttemptExpired, "authorization attempt expired")
			m.failAttemptLocked(attempt, terminalErr)
			result := attempt.public
			m.mu.Unlock()
			return result, terminalErr
		}
		if providerErrors := values["error"]; len(providerErrors) != 0 {
			terminalErr := newError(CodeAuthorizationFailed, "provider denied the authorization request")
			m.failAttemptLocked(attempt, terminalErr)
			result := attempt.public
			m.mu.Unlock()
			return result, terminalErr
		}
		codes := values["code"]
		if len(codes) != 1 || codes[0] == "" {
			result := attempt.public
			m.mu.Unlock()
			return result, newError(CodeInvalidRequest, "redirect URL has no authorization code")
		}
		exchange := authorizationExchange{
			attempt: attempt,
			driver:  attempt.driver,
			input: CodeExchange{
				Code:        codes[0],
				State:       attempt.state,
				Verifier:    attempt.verifier,
				RedirectURI: attempt.public.CallbackURL,
			},
			provider:     attempt.public.Provider,
			credentialID: attempt.public.CredentialID,
			baseRevision: attempt.baseRevision,
			baseStatus:   attempt.baseStatus,
		}
		attempt.exchangeStarted = true
		attempt.verifier = ""
		m.stopAttemptExpiryLocked(attempt)
		m.backgroundWorks.Add(1)
		go m.completeAuthorization(exchange)
	}
	done := attempt.done
	pending := attempt.public
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		return pending, ctx.Err()
	case <-done:
		m.mu.Lock()
		result, terminalErr := attempt.public, attempt.terminalErr
		m.mu.Unlock()
		return result, terminalErr
	}
}

// Credential resolves a fresh token snapshot for the exact current profile
// binding. Concurrent refreshes for one credential share one manager-owned
// exchange and persistence operation.
func (m *Manager) Credential(ctx context.Context, binding Binding) (Snapshot, error) {
	if err := m.checkOpen(); err != nil {
		return Snapshot{}, err
	}
	record, payload, err := m.boundCredential(ctx, binding)
	if err != nil {
		return Snapshot{}, err
	}
	if !needsRefresh(payload.Tokens, m.now()) {
		return snapshotFromPayload(record.Provider, payload), nil
	}
	call, err := m.startRefresh(binding.CredentialID)
	if err != nil {
		return Snapshot{}, err
	}
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-call.done:
		return call.snapshot, call.err
	}
}

// RegisterRequest validates the binding immediately before an actual model
// call and returns a context canceled only when that credential disconnects,
// the caller cancels, or the manager closes.
func (m *Manager) RegisterRequest(ctx context.Context, binding Binding) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateBinding(binding); err != nil {
		return nil, nil, err
	}
	if err := m.checkOpen(); err != nil {
		return nil, nil, err
	}
	gate := m.gate(binding.CredentialID)
	releaseGate, err := gate.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer releaseGate()

	credentialID, generation, err := m.store.ProfileCredentialBinding(ctx, binding.ProfileID)
	if err != nil {
		return nil, nil, persistenceError(ctx, err)
	}
	if credentialID != binding.CredentialID || generation != binding.Generation {
		return nil, nil, newError(CodeBindingChanged, "profile credential binding changed")
	}
	record, err := m.store.LLMCredential(ctx, binding.CredentialID)
	if err != nil {
		return nil, nil, persistenceError(ctx, err)
	}
	if record == nil {
		return nil, nil, newError(CodeCredentialNotFound, "credential was not found")
	}
	if err := credentialStatusError(record.Status); err != nil {
		return nil, nil, err
	}
	if err := recordTaskUsage(ctx, m.store); err != nil {
		return nil, nil, err
	}

	requestCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if err := m.openErrorLocked(); err != nil {
		m.mu.Unlock()
		cancel()
		return nil, nil, err
	}
	m.nextRequestID++
	requestID := m.nextRequestID
	if m.requests[binding.CredentialID] == nil {
		m.requests[binding.CredentialID] = make(map[uint64]context.CancelFunc)
	}
	m.requests[binding.CredentialID][requestID] = cancel
	m.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			m.mu.Lock()
			delete(m.requests[binding.CredentialID], requestID)
			if len(m.requests[binding.CredentialID]) == 0 {
				delete(m.requests, binding.CredentialID)
			}
			m.mu.Unlock()
		})
	}
	return requestCtx, release, nil
}

// Disconnect durably closes the local credential gate, clears token material,
// cancels only that credential's model calls, then attempts remote revocation.
// A remote failure is reported distinctly; it never resurrects local access.
func (m *Manager) Disconnect(ctx context.Context, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id <= 0 {
		return newError(CodeInvalidRequest, "credential ID is invalid")
	}
	m.mu.Lock()
	if err := m.openErrorLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	call := m.disconnects[id]
	if call == nil {
		call = &disconnectCall{done: make(chan struct{})}
		m.disconnects[id] = call
		m.backgroundWorks.Add(1)
		go m.runDisconnect(id, call)
	}
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-call.done:
		return call.err
	}
}

// Close rejects new work, stops callback listeners, cancels active model calls,
// and drains manager-owned token exchange/persistence before returning.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		for _, attempt := range m.attempts {
			if attempt.public.Status == AttemptPending && !attempt.exchangeStarted {
				m.failAttemptLocked(attempt, newError(CodeManagerClosed, "subscription manager is closed"))
			}
		}
		listeners := make([]*callbackListener, 0, len(m.listeners))
		for _, listener := range m.listeners {
			listeners = append(listeners, listener)
		}
		cancels := make([]context.CancelFunc, 0)
		for _, credentialRequests := range m.requests {
			for _, cancel := range credentialRequests {
				cancels = append(cancels, cancel)
			}
		}
		go m.finishClose(listeners, cancels)
	}
	done := m.closeDone
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (m *Manager) completeAuthorization(exchange authorizationExchange) {
	defer m.backgroundWorks.Done()
	ctx, cancel := context.WithTimeout(m.workCtx, operationTimeout)
	defer cancel()

	tokens, err := exchange.driver.Exchange(ctx, exchange.input)
	if err != nil {
		m.finishAttempt(exchange.attempt, 0, sanitizedProviderError(ctx, CodeAuthorizationFailed, "provider authorization failed", err))
		return
	}
	merged, err := mergeTokenSets(TokenSet{}, tokens, m.now())
	if err != nil {
		m.finishAttempt(exchange.attempt, 0, asTerminalError(err))
		return
	}

	var credentialID int64
	if exchange.credentialID == 0 {
		payload := credentialPayload{
			Version:   credentialPayloadVersion,
			Provider:  exchange.provider,
			AuthScope: authScope(exchange.provider, merged),
			Tokens:    merged,
		}
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			m.finishAttempt(exchange.attempt, 0, newError(CodeCredentialPersistence, "credential could not be persisted"))
			return
		}
		record, createErr := m.store.CreateLLMCredential(ctx, exchange.provider, credentialLabel(exchange.provider, merged), encoded)
		if createErr != nil {
			m.finishAttempt(exchange.attempt, 0, asTerminalError(persistenceError(ctx, createErr)))
			return
		}
		if record == nil || record.ID <= 0 {
			m.finishAttempt(exchange.attempt, 0, newError(CodeCredentialPersistence, "credential could not be persisted"))
			return
		}
		credentialID = record.ID
	} else {
		credentialID = exchange.credentialID
		if persistErr := m.persistReauthorization(ctx, exchange, merged); persistErr != nil {
			m.finishAttempt(exchange.attempt, 0, asTerminalError(persistErr))
			return
		}
	}
	m.finishAttempt(exchange.attempt, credentialID, nil)
}

func (m *Manager) persistReauthorization(ctx context.Context, exchange authorizationExchange, tokens TokenSet) error {
	gate := m.gate(exchange.credentialID)
	releaseGate, err := gate.acquire(ctx)
	if err != nil {
		return err
	}
	defer releaseGate()
	unlock, err := m.store.LockLLMCredential(ctx, exchange.credentialID)
	if err != nil {
		return persistenceError(ctx, err)
	}
	defer unlock()

	record, err := m.store.LLMCredential(ctx, exchange.credentialID)
	if err != nil {
		return persistenceError(ctx, err)
	}
	if record == nil {
		return newError(CodeCredentialNotFound, "credential was not found")
	}
	if record.Provider != exchange.provider {
		return newError(CodeIdentityChanged, "credential belongs to a different provider")
	}
	if record.Revision != exchange.baseRevision || record.Status != exchange.baseStatus {
		return newError(CodeCredentialChanged, "credential changed after authorization began")
	}
	oldPayload, err := decodeCredential(record, exchange.provider)
	if err != nil {
		return err
	}
	merged, err := mergeTokenSets(oldPayload.Tokens, tokens, m.now())
	if err != nil {
		return err
	}
	updated := credentialPayload{
		Version:   credentialPayloadVersion,
		Provider:  record.Provider,
		AuthScope: authScope(record.Provider, merged),
		Tokens:    merged,
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return newError(CodeCredentialPersistence, "credential could not be persisted")
	}
	expectedRevision := record.Revision
	record.Payload = encoded
	record.Status = CredentialReady
	if err := m.store.UpdateLLMCredential(ctx, record, expectedRevision); err != nil {
		return changedOrPersistenceError(ctx, err)
	}
	return nil
}

func (m *Manager) startRefresh(id int64) (*refreshCall, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.openErrorLocked(); err != nil {
		return nil, err
	}
	if call := m.refreshes[id]; call != nil {
		return call, nil
	}
	call := &refreshCall{done: make(chan struct{})}
	m.refreshes[id] = call
	m.backgroundWorks.Add(1)
	go m.runRefresh(id, call)
	return call, nil
}

func (m *Manager) runRefresh(id int64, call *refreshCall) {
	defer m.backgroundWorks.Done()
	ctx, cancel := context.WithTimeout(m.workCtx, operationTimeout)
	defer cancel()

	call.snapshot, call.err = m.refreshCredential(ctx, id)
	m.mu.Lock()
	if m.refreshes[id] == call {
		delete(m.refreshes, id)
	}
	close(call.done)
	m.mu.Unlock()
}

func (m *Manager) refreshCredential(ctx context.Context, id int64) (Snapshot, error) {
	gate := m.gate(id)
	releaseGate, err := gate.acquire(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer releaseGate()
	unlock, err := m.store.LockLLMCredential(ctx, id)
	if err != nil {
		return Snapshot{}, persistenceError(ctx, err)
	}
	defer unlock()

	record, err := m.store.LLMCredential(ctx, id)
	if err != nil {
		return Snapshot{}, persistenceError(ctx, err)
	}
	if record == nil {
		return Snapshot{}, newError(CodeCredentialNotFound, "credential was not found")
	}
	if err := credentialStatusError(record.Status); err != nil {
		return Snapshot{}, err
	}
	payload, err := decodeCredential(record, record.Provider)
	if err != nil {
		return Snapshot{}, err
	}
	if !needsRefresh(payload.Tokens, m.now()) {
		return snapshotFromPayload(record.Provider, payload), nil
	}
	if payload.Tokens.RefreshToken == "" {
		if err := m.persistStatus(ctx, record, CredentialReauthRequired, payload); err != nil {
			return Snapshot{}, err
		}
		return Snapshot{}, newError(CodeAuthRequired, "credential requires authorization")
	}
	driver := m.drivers[record.Provider]
	if driver == nil {
		return Snapshot{}, newError(CodeProviderUnsupported, "subscription provider is unsupported")
	}
	next, err := driver.Refresh(ctx, payload.Tokens)
	if err != nil {
		providerErr := sanitizedProviderError(ctx, CodeRefreshFailed, "credential refresh failed", err)
		var terminal *Error
		if errors.As(err, &terminal) && terminal.Code == CodeAuthRequired {
			if persistErr := m.persistStatus(ctx, record, CredentialReauthRequired, payload); persistErr != nil {
				return Snapshot{}, persistErr
			}
		}
		return Snapshot{}, providerErr
	}
	merged, err := mergeTokenSets(payload.Tokens, next, m.now())
	if err != nil {
		return Snapshot{}, err
	}
	updated := credentialPayload{
		Version:   credentialPayloadVersion,
		Provider:  record.Provider,
		AuthScope: authScope(record.Provider, merged),
		Tokens:    merged,
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return Snapshot{}, newError(CodeCredentialPersistence, "credential could not be persisted")
	}
	expectedRevision := record.Revision
	record.Payload = encoded
	record.Status = CredentialReady
	if err := m.store.UpdateLLMCredential(ctx, record, expectedRevision); err != nil {
		return Snapshot{}, changedOrPersistenceError(ctx, err)
	}
	return snapshotFromPayload(record.Provider, updated), nil
}

func (m *Manager) runDisconnect(id int64, call *disconnectCall) {
	defer m.backgroundWorks.Done()
	ctx, cancel := context.WithTimeout(m.workCtx, operationTimeout)
	defer cancel()
	call.err = m.disconnectCredential(ctx, id)
	m.mu.Lock()
	if m.disconnects[id] == call {
		delete(m.disconnects, id)
	}
	close(call.done)
	m.mu.Unlock()
}

func (m *Manager) disconnectCredential(ctx context.Context, id int64) error {
	gate := m.gate(id)
	releaseGate, err := gate.acquire(ctx)
	if err != nil {
		return err
	}
	defer releaseGate()
	unlock, err := m.store.LockLLMCredential(ctx, id)
	if err != nil {
		return persistenceError(ctx, err)
	}
	defer unlock()

	record, err := m.store.LLMCredential(ctx, id)
	if err != nil {
		return persistenceError(ctx, err)
	}
	if record == nil {
		return newError(CodeCredentialNotFound, "credential was not found")
	}
	if record.Status == CredentialRevoked {
		m.cancelCredentialRequests(id)
		return nil
	}

	payload, decodeErr := decodeCredential(record, record.Provider)
	if decodeErr != nil {
		payload = credentialPayload{Version: credentialPayloadVersion, Provider: record.Provider}
	}
	oldTokens := payload.Tokens
	payload.Tokens = redactTokens(payload.Tokens)
	if payload.AuthScope == "" && (payload.Tokens.AccountID != "" || payload.Tokens.Subject != "") {
		payload.AuthScope = authScope(record.Provider, payload.Tokens)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return newError(CodeCredentialPersistence, "credential could not be revoked locally")
	}
	expectedRevision := record.Revision
	record.Payload = encoded
	record.Status = CredentialRevoked
	if err := m.store.UpdateLLMCredential(ctx, record, expectedRevision); err != nil {
		return changedOrPersistenceError(ctx, err)
	}
	m.cancelCredentialRequests(id)

	if decodeErr != nil {
		return newError(CodeRevocationNotConfirmed, "credential was revoked locally; remote revocation was not confirmed")
	}
	driver := m.drivers[record.Provider]
	if driver == nil {
		return newError(CodeRevocationNotConfirmed, "credential was revoked locally; remote revocation was not confirmed")
	}
	if err := driver.Revoke(ctx, oldTokens); err != nil {
		if errors.Is(err, ErrRevocationUnsupported) {
			return ErrRevocationUnsupported
		}
		return newError(CodeRevocationNotConfirmed, "credential was revoked locally; remote revocation was not confirmed")
	}
	return nil
}

func (m *Manager) boundCredential(ctx context.Context, binding Binding) (*db.LLMCredential, credentialPayload, error) {
	if err := validateBinding(binding); err != nil {
		return nil, credentialPayload{}, err
	}
	credentialID, generation, err := m.store.ProfileCredentialBinding(ctx, binding.ProfileID)
	if err != nil {
		return nil, credentialPayload{}, persistenceError(ctx, err)
	}
	if credentialID != binding.CredentialID || generation != binding.Generation {
		return nil, credentialPayload{}, newError(CodeBindingChanged, "profile credential binding changed")
	}
	record, err := m.store.LLMCredential(ctx, binding.CredentialID)
	if err != nil {
		return nil, credentialPayload{}, persistenceError(ctx, err)
	}
	if record == nil {
		return nil, credentialPayload{}, newError(CodeCredentialNotFound, "credential was not found")
	}
	if err := credentialStatusError(record.Status); err != nil {
		return nil, credentialPayload{}, err
	}
	payload, err := decodeCredential(record, record.Provider)
	if err != nil {
		return nil, credentialPayload{}, err
	}
	if m.drivers[record.Provider] == nil {
		return nil, credentialPayload{}, newError(CodeProviderUnsupported, "subscription provider is unsupported")
	}
	return record, payload, nil
}

func decodeCredential(record *db.LLMCredential, expectedProvider string) (credentialPayload, error) {
	if record.Provider == "" || record.Provider != expectedProvider {
		return credentialPayload{}, newError(CodeCredentialInvalid, "credential provider binding is invalid")
	}
	var payload credentialPayload
	if len(record.Payload) == 0 || json.Unmarshal(record.Payload, &payload) != nil {
		return credentialPayload{}, newError(CodeCredentialInvalid, "credential payload is invalid")
	}
	if payload.Version != credentialPayloadVersion || payload.Provider != record.Provider {
		return credentialPayload{}, newError(CodeCredentialInvalid, "credential payload binding is invalid")
	}
	if payload.AuthScope == "" || (payload.Tokens.AccountID == "" && payload.Tokens.Subject == "") {
		return credentialPayload{}, newError(CodeCredentialInvalid, "credential account binding is missing")
	}
	expectedScope := authScope(record.Provider, payload.Tokens)
	if !constantTimeEqual(payload.AuthScope, expectedScope) {
		return credentialPayload{}, newError(CodeCredentialInvalid, "credential account binding is invalid")
	}
	return payload, nil
}

func snapshotFromPayload(provider string, payload credentialPayload) Snapshot {
	snapshot := Snapshot{Provider: provider, Tokens: payload.Tokens, AuthScope: payload.AuthScope}
	snapshot.Tokens.Scopes = append([]string(nil), payload.Tokens.Scopes...)
	return snapshot
}

func needsRefresh(tokens TokenSet, now time.Time) bool {
	return tokens.AccessToken == "" || tokens.ExpiresAt.IsZero() || !now.Add(refreshSkew).Before(tokens.ExpiresAt)
}

func validateBinding(binding Binding) error {
	if binding.ProfileID <= 0 || binding.CredentialID <= 0 || binding.Generation < 0 {
		return newError(CodeInvalidRequest, "profile credential binding is invalid")
	}
	return nil
}

func credentialStatusError(status string) error {
	switch status {
	case CredentialReady:
		return nil
	case CredentialRevoked:
		return newError(CodeRevoked, "credential is disconnected")
	case CredentialReauthRequired:
		return newError(CodeAuthRequired, "credential requires authorization")
	default:
		return newError(CodeCredentialInvalid, "credential status is invalid")
	}
}

func (m *Manager) persistStatus(ctx context.Context, record *db.LLMCredential, status string, payload credentialPayload) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return newError(CodeCredentialPersistence, "credential status could not be persisted")
	}
	expectedRevision := record.Revision
	record.Payload = encoded
	record.Status = status
	if err := m.store.UpdateLLMCredential(ctx, record, expectedRevision); err != nil {
		return changedOrPersistenceError(ctx, err)
	}
	return nil
}

func (m *Manager) startListenerLocked(port int) (*callbackListener, error) {
	if entry := m.listeners[port]; entry != nil {
		entry.pending++
		return entry, nil
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(m.listenHost, strconv.Itoa(port)))
	if err != nil {
		return nil, newError(CodeCallbackUnavailable, "provider callback listener is unavailable")
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(m.handleCallback),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	entry := &callbackListener{
		server:   server,
		listener: listener,
		port:     port,
		pending:  1,
	}
	m.listeners[port] = entry
	m.backgroundWorks.Add(1)
	go func() {
		defer m.backgroundWorks.Done()
		_ = server.Serve(listener)
		m.mu.Lock()
		if m.listeners[port] == entry {
			delete(m.listeners, port)
		}
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), listenerStopLimit)
		_ = server.Shutdown(ctx)
		cancel()
	}()
	return entry, nil
}

func (listener *callbackListener) stop() {
	listener.stopOnce.Do(func() {
		_ = listener.listener.Close()
	})
}

func (m *Manager) releaseAttemptListenerLocked(attempt *authorizationAttempt) {
	listener := attempt.listener
	if listener == nil {
		return
	}
	attempt.listener = nil
	listener.pending--
	if listener.pending != 0 {
		return
	}
	if m.listeners[listener.port] == listener {
		delete(m.listeners, listener.port)
	}
	listener.stop()
}

func (m *Manager) handleCallback(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = response.Write([]byte("Authorization callback rejected."))
		return
	}
	values := request.URL.Query()
	states := values["state"]
	if len(states) != 1 {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte("Authorization callback rejected."))
		return
	}
	localAddress, _ := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	localPort := ""
	if localAddress != nil {
		_, localPort, _ = net.SplitHostPort(localAddress.String())
	}
	m.mu.Lock()
	attempt := m.attemptStates[states[0]]
	if attempt == nil ||
		request.Host != attempt.redirect.Host ||
		request.URL.EscapedPath() != attempt.redirect.EscapedPath() ||
		localPort != attempt.redirect.Port() {
		m.mu.Unlock()
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte("Authorization callback rejected."))
		return
	}
	owner, attemptID := attempt.owner, attempt.public.ID
	expected := *attempt.redirect
	expected.RawQuery = request.URL.RawQuery
	redirectURL := expected.String()
	m.mu.Unlock()

	result, err := m.submit(request.Context(), owner, attemptID, redirectURL)
	if err != nil || result.Status != AttemptComplete {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte("Authorization was not completed."))
		return
	}
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte("Authorization completed. You may close this window."))
}

func (m *Manager) finishAttempt(attempt *authorizationAttempt, credentialID int64, terminalErr *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt.public.Status != AttemptPending {
		return
	}
	attempt.public.AuthorizationURL = ""
	attempt.verifier = ""
	delete(m.attemptStates, attempt.state)
	attempt.state = ""
	m.stopAttemptExpiryLocked(attempt)
	m.releaseAttemptListenerLocked(attempt)
	if terminalErr != nil {
		attempt.public.Status = AttemptFailed
		attempt.public.Error = terminalErr.Message
		attempt.terminalErr = terminalErr
	} else {
		attempt.public.Status = AttemptComplete
		attempt.public.CredentialID = credentialID
	}
	close(attempt.done)
}

func (m *Manager) failAttemptLocked(attempt *authorizationAttempt, terminalErr *Error) {
	if attempt.public.Status != AttemptPending {
		return
	}
	attempt.public.Status = AttemptFailed
	attempt.public.AuthorizationURL = ""
	attempt.public.Error = terminalErr.Message
	attempt.verifier = ""
	delete(m.attemptStates, attempt.state)
	attempt.state = ""
	m.stopAttemptExpiryLocked(attempt)
	m.releaseAttemptListenerLocked(attempt)
	attempt.terminalErr = terminalErr
	close(attempt.done)
}

func (m *Manager) expireAttemptLocked(attempt *authorizationAttempt, now time.Time) {
	if attempt.public.Status == AttemptPending && !attempt.exchangeStarted && !now.Before(attempt.public.ExpiresAt) {
		m.failAttemptLocked(attempt, newError(CodeAttemptExpired, "authorization attempt expired"))
	}
}

func (m *Manager) scheduleAttemptExpiryLocked(attempt *authorizationAttempt) {
	delay := attempt.public.ExpiresAt.Sub(m.now())
	if delay < 0 {
		delay = 0
	} else if delay > authorizationTTL {
		delay = authorizationTTL
	}
	attempt.expiryTimer = m.scheduleExpiry(delay, func() {
		m.expireScheduledAttempt(attempt)
	})
}

func (m *Manager) stopAttemptExpiryLocked(attempt *authorizationAttempt) {
	if attempt.expiryTimer == nil {
		return
	}
	attempt.expiryTimer.Stop()
	attempt.expiryTimer = nil
}

func (m *Manager) expireScheduledAttempt(attempt *authorizationAttempt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt.public.Status != AttemptPending || attempt.exchangeStarted {
		attempt.expiryTimer = nil
		return
	}
	now := m.now()
	if now.Before(attempt.public.ExpiresAt) {
		m.scheduleAttemptExpiryLocked(attempt)
		return
	}
	m.failAttemptLocked(attempt, newError(CodeAttemptExpired, "authorization attempt expired"))
}

func (m *Manager) ownedAttemptLocked(owner, id string) (*authorizationAttempt, error) {
	if owner == "" || id == "" {
		return nil, newError(CodeInvalidRequest, "authorization owner and attempt ID are required")
	}
	attempt := m.attempts[id]
	if attempt == nil {
		return nil, newError(CodeAttemptNotFound, "authorization attempt was not found")
	}
	if !constantTimeEqual(attempt.owner, owner) {
		return nil, newError(CodeAttemptOwnerMismatch, "authorization attempt belongs to a different owner")
	}
	return attempt, nil
}

func (m *Manager) pruneAttemptsLocked(now time.Time) {
	for id, attempt := range m.attempts {
		m.expireAttemptLocked(attempt, now)
		if attempt.public.Status != AttemptPending && now.After(attempt.public.ExpiresAt.Add(authorizationTTL)) {
			delete(m.attempts, id)
			delete(m.attemptStates, attempt.state)
		}
	}
}

func (m *Manager) gate(id int64) *credentialGate {
	m.mu.Lock()
	defer m.mu.Unlock()
	gate := m.gates[id]
	if gate == nil {
		gate = newCredentialGate()
		m.gates[id] = gate
	}
	return gate
}

func (m *Manager) cancelCredentialRequests(id int64) {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.requests[id]))
	for _, cancel := range m.requests[id] {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (m *Manager) finishClose(listeners []*callbackListener, cancels []context.CancelFunc) {
	for _, cancel := range cancels {
		cancel()
	}
	for _, listener := range listeners {
		listener.stop()
	}
	m.backgroundWorks.Wait()
	m.workCancel()
	close(m.closeDone)
}

func (m *Manager) checkOpen() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openErrorLocked()
}

func (m *Manager) openErrorLocked() error {
	if m.initErr != nil {
		return m.initErr
	}
	if m.closed {
		return newError(CodeManagerClosed, "subscription manager is closed")
	}
	return nil
}

func credentialLabel(provider string, tokens TokenSet) string {
	if tokens.Email != "" {
		return tokens.Email
	}
	if tokens.OrgName != "" {
		return tokens.OrgName
	}
	return provider
}

func persistenceError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return newError(CodeCredentialPersistence, "credential store operation failed")
}

func changedOrPersistenceError(ctx context.Context, err error) error {
	if errors.Is(err, db.ErrCredentialChanged) {
		return newError(CodeCredentialChanged, "credential changed concurrently")
	}
	return persistenceError(ctx, err)
}

func sanitizedProviderError(ctx context.Context, code, message string, err error) *Error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return newError(code, message)
	}
	if errors.Is(err, ErrRevocationUnsupported) {
		return ErrRevocationUnsupported
	}
	var providerErr *Error
	if errors.As(err, &providerErr) {
		if providerErr.Code != "" {
			code = providerErr.Code
		}
		return &Error{
			Code:      code,
			Message:   message,
			Status:    providerErr.Status,
			ResetAt:   providerErr.ResetAt,
			RequestID: providerErr.RequestID,
		}
	}
	return newError(code, message)
}

func asTerminalError(err error) *Error {
	var terminal *Error
	if errors.As(err, &terminal) {
		return terminal
	}
	return newError(CodeCredentialPersistence, "credential operation failed")
}
