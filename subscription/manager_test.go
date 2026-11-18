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
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type observedDoneContext struct {
	context.Context
	done     chan struct{}
	observed chan struct{}
	once     sync.Once
}

func newObservedDoneContext() *observedDoneContext {
	return &observedDoneContext{
		Context:  context.Background(),
		done:     make(chan struct{}),
		observed: make(chan struct{}),
	}
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.done
}

type testAuthorizationTimer struct {
	mu       sync.Mutex
	active   bool
	callback func()
}

func newTestAuthorizationTimer(callback func()) *testAuthorizationTimer {
	return &testAuthorizationTimer{active: true, callback: callback}
}

func (timer *testAuthorizationTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := timer.active
	timer.active = false
	return wasActive
}

func (timer *testAuthorizationTimer) fire() bool {
	timer.mu.Lock()
	if !timer.active {
		timer.mu.Unlock()
		return false
	}
	timer.active = false
	callback := timer.callback
	timer.mu.Unlock()
	callback()
	return true
}

type testBinding struct {
	credentialID int64
	generation   int64
}

type testStore struct {
	mu       sync.Mutex
	nextID   int64
	creds    map[int64]*db.LLMCredential
	bindings map[int64]testBinding
	locks    map[int64]chan struct{}
}

func newTestStore() *testStore {
	return &testStore{
		nextID:   1,
		creds:    make(map[int64]*db.LLMCredential),
		bindings: make(map[int64]testBinding),
		locks:    make(map[int64]chan struct{}),
	}
}

func (s *testStore) CreateLLMCredential(_ context.Context, provider, label string, payload json.RawMessage) (*db.LLMCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	record := &db.LLMCredential{
		ID:         id,
		Provider:   provider,
		Label:      label,
		Status:     CredentialReady,
		Revision:   1,
		Generation: 1,
		Payload:    append(json.RawMessage(nil), payload...),
	}
	s.creds[id] = cloneTestCredential(record)
	return cloneTestCredential(record), nil
}

func (s *testStore) LLMCredential(_ context.Context, id int64) (*db.LLMCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTestCredential(s.creds[id]), nil
}

func (s *testStore) ListLLMCredentials(context.Context) ([]db.LLMCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]db.LLMCredential, 0, len(s.creds))
	for _, record := range s.creds {
		result = append(result, *cloneTestCredential(record))
		result[len(result)-1].Payload = nil
	}
	return result, nil
}

func (s *testStore) UpdateLLMCredential(_ context.Context, credential *db.LLMCredential, expectedRevision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.creds[credential.ID]
	if current == nil || current.Revision != expectedRevision {
		return db.ErrCredentialChanged
	}
	updated := cloneTestCredential(credential)
	updated.Revision = expectedRevision + 1
	credential.Revision = updated.Revision
	s.creds[credential.ID] = updated
	return nil
}

func (s *testStore) LockLLMCredential(ctx context.Context, id int64) (func(), error) {
	s.mu.Lock()
	lock := s.locks[id]
	if lock == nil {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		s.locks[id] = lock
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock:
		var once sync.Once
		return func() { once.Do(func() { lock <- struct{}{} }) }, nil
	}
}

func (s *testStore) ProfileCredentialBinding(_ context.Context, profileID int64) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.bindings[profileID]
	return binding.credentialID, binding.generation, nil
}

func (s *testStore) bind(profileID, credentialID, generation int64) {
	s.mu.Lock()
	s.bindings[profileID] = testBinding{credentialID: credentialID, generation: generation}
	s.mu.Unlock()
}

func (s *testStore) seed(provider, status string, tokens TokenSet) int64 {
	payload := credentialPayload{
		Version:   credentialPayloadVersion,
		Provider:  provider,
		AuthScope: authScope(provider, tokens),
		Tokens:    tokens,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	record, err := s.CreateLLMCredential(context.Background(), provider, provider, encoded)
	if err != nil {
		panic(err)
	}
	if status != CredentialReady {
		s.mu.Lock()
		s.creds[record.ID].Status = status
		s.mu.Unlock()
	}
	return record.ID
}

func cloneTestCredential(record *db.LLMCredential) *db.LLMCredential {
	if record == nil {
		return nil
	}
	clone := *record
	clone.Payload = append(json.RawMessage(nil), record.Payload...)
	return &clone
}

type testDriver struct {
	kind           string
	callback       CallbackConfig
	clock          *testClock
	mu             sync.Mutex
	exchangeCalls  int
	refreshCalls   int
	revokeCalls    int
	exchangeTokens map[string]TokenSet
	exchange       func(context.Context, CodeExchange) (TokenSet, error)
	refresh        func(context.Context, TokenSet) (TokenSet, error)
	revoke         func(context.Context, TokenSet) error
}

func (d *testDriver) Kind() string { return d.kind }

func (d *testDriver) Callback() CallbackConfig { return d.callback }

func (d *testDriver) AuthorizationURL(state, challenge string) string {
	values := url.Values{"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	return "https://auth.fixture.invalid/authorize?" + values.Encode()
}

func (d *testDriver) Exchange(ctx context.Context, input CodeExchange) (TokenSet, error) {
	d.mu.Lock()
	d.exchangeCalls++
	exchange := d.exchange
	tokens, found := d.exchangeTokens[input.Code]
	d.mu.Unlock()
	if exchange != nil {
		return exchange(ctx, input)
	}
	if !found {
		return TokenSet{}, errors.New("fixture exchange body containing secret-token")
	}
	return tokens, nil
}

func (d *testDriver) Refresh(ctx context.Context, old TokenSet) (TokenSet, error) {
	d.mu.Lock()
	d.refreshCalls++
	refresh := d.refresh
	d.mu.Unlock()
	if refresh == nil {
		return TokenSet{}, errors.New("fixture refresh unavailable")
	}
	return refresh(ctx, old)
}

func (d *testDriver) Revoke(ctx context.Context, old TokenSet) error {
	d.mu.Lock()
	d.revokeCalls++
	revoke := d.revoke
	d.mu.Unlock()
	if revoke == nil {
		return nil
	}
	return revoke(ctx, old)
}

func (d *testDriver) counts() (exchange, refresh, revoke int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.exchangeCalls, d.refreshCalls, d.revokeCalls
}

func TestManagerRefreshSingleflightSurvivesWaiterCancellation(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	credentialID := store.seed(ProviderClaude, CredentialReady, TokenSet{
		AccessToken:  "expired-access",
		RefreshToken: "rotating-refresh",
		ExpiresAt:    clock.Now().Add(-time.Minute),
		AccountID:    "account-1",
		OrgID:        "org-1",
		Email:        "owner@example.invalid",
	})
	store.bind(11, credentialID, 2)
	store.bind(12, credentialID, 4)

	refreshEntered := make(chan struct{})
	refreshRelease := make(chan struct{})
	var enteredOnce sync.Once
	driver := testOAuthDriver(clock)
	driver.refresh = func(context.Context, TokenSet) (TokenSet, error) {
		enteredOnce.Do(func() { close(refreshEntered) })
		<-refreshRelease
		return TokenSet{
			AccessToken: "fresh-access",
			ExpiresAt:   clock.Now().Add(time.Hour),
			AccountID:   "account-1",
		}, nil
	}
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := manager.Credential(firstCtx, Binding{ProfileID: 11, CredentialID: credentialID, Generation: 2})
		firstResult <- err
	}()
	<-refreshEntered
	secondResult := make(chan struct {
		snapshot Snapshot
		err      error
	}, 1)
	go func() {
		snapshot, err := manager.Credential(context.Background(), Binding{ProfileID: 12, CredentialID: credentialID, Generation: 4})
		secondResult <- struct {
			snapshot Snapshot
			err      error
		}{snapshot: snapshot, err: err}
	}()
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}
	close(refreshRelease)
	second := <-secondResult
	if second.err != nil {
		t.Fatalf("shared refresh failed: %v", second.err)
	}
	if second.snapshot.Tokens.AccessToken != "fresh-access" || second.snapshot.Tokens.RefreshToken != "rotating-refresh" {
		t.Fatalf("merged snapshot = %#v", second.snapshot.Tokens)
	}
	_, refreshCalls, _ := driver.counts()
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	stored, _ := store.LLMCredential(context.Background(), credentialID)
	payload, err := decodeCredential(stored, ProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Tokens.AccessToken != "fresh-access" {
		t.Fatalf("persisted access token = %q", payload.Tokens.AccessToken)
	}
}

func TestManagersSerializeRefreshThroughStoreLockAndReread(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	credentialID := store.seed(ProviderClaude, CredentialReady, TokenSet{
		AccessToken:  "expired-access",
		RefreshToken: "rotating-refresh",
		ExpiresAt:    clock.Now().Add(-time.Minute),
		AccountID:    "account-1",
	})
	store.bind(51, credentialID, 1)
	store.bind(52, credentialID, 1)
	refreshEntered := make(chan struct{})
	refreshRelease := make(chan struct{})
	var enteredOnce sync.Once
	driver := testOAuthDriver(clock)
	driver.refresh = func(context.Context, TokenSet) (TokenSet, error) {
		enteredOnce.Do(func() { close(refreshEntered) })
		<-refreshRelease
		return TokenSet{
			AccessToken: "fresh-access",
			ExpiresAt:   clock.Now().Add(time.Hour),
			AccountID:   "account-1",
		}, nil
	}
	firstManager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	secondManager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, firstManager)
	defer closeManager(t, secondManager)

	type credentialResult struct {
		snapshot Snapshot
		err      error
	}
	firstResult := make(chan credentialResult, 1)
	secondResult := make(chan credentialResult, 1)
	go func() {
		snapshot, err := firstManager.Credential(context.Background(), Binding{ProfileID: 51, CredentialID: credentialID, Generation: 1})
		firstResult <- credentialResult{snapshot: snapshot, err: err}
	}()
	<-refreshEntered
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		snapshot, err := secondManager.Credential(context.Background(), Binding{ProfileID: 52, CredentialID: credentialID, Generation: 1})
		secondResult <- credentialResult{snapshot: snapshot, err: err}
	}()
	<-secondStarted
	close(refreshRelease)
	first := <-firstResult
	second := <-secondResult
	if first.err != nil || second.err != nil {
		t.Fatalf("refresh results: first = %v, second = %v", first.err, second.err)
	}
	if first.snapshot.Tokens.AccessToken != "fresh-access" || second.snapshot.Tokens.AccessToken != "fresh-access" {
		t.Fatalf("refresh snapshots: first = %#v, second = %#v", first.snapshot, second.snapshot)
	}
	_, refreshCalls, _ := driver.counts()
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
}

func TestAttemptRejectsOwnerStateRedirectAndExpiry(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	driver := testOAuthDriver(clock)
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	attempt, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Attempt(context.Background(), "owner-b", attempt.ID); errorCode(err) != CodeAttemptOwnerMismatch {
		t.Fatalf("owner mismatch error = %v", err)
	}
	state := authorizationState(t, attempt.AuthorizationURL)
	if _, err := manager.Submit(context.Background(), "owner-b", attempt.ID, attempt.CallbackURL+"?state="+state+"&code=ok"); errorCode(err) != CodeAttemptOwnerMismatch {
		t.Fatalf("submit owner mismatch error = %v", err)
	}
	wrongPath := strings.Replace(attempt.CallbackURL, "/callback", "/other", 1)
	if _, err := manager.Submit(context.Background(), "owner-a", attempt.ID, wrongPath+"?state="+state+"&code=ok"); errorCode(err) != CodeRedirectMismatch {
		t.Fatalf("redirect mismatch error = %v", err)
	}
	if _, err := manager.Submit(context.Background(), "owner-a", attempt.ID, attempt.CallbackURL+"?state=wrong&code=ok"); errorCode(err) != CodeStateMismatch {
		t.Fatalf("state mismatch error = %v", err)
	}
	clock.Advance(authorizationTTL + time.Second)
	result, err := manager.Submit(context.Background(), "owner-a", attempt.ID, attempt.CallbackURL+"?state="+state+"&code=ok")
	if errorCode(err) != CodeAttemptExpired || result.Status != AttemptFailed {
		t.Fatalf("expired result = %#v, err = %v", result, err)
	}
	exchanges, _, _ := driver.counts()
	if exchanges != 0 {
		t.Fatalf("exchange calls = %d, want 0", exchanges)
	}
}

func TestDuplicateRedirectJoinsBlockedExchange(t *testing.T) {
	tests := []struct {
		name      string
		advance   bool
		duplicate func(Attempt, string) string
	}{
		{
			name: "provider denial",
			duplicate: func(attempt Attempt, state string) string {
				return attempt.CallbackURL + "?state=" + url.QueryEscape(state) + "&error=access_denied"
			},
		},
		{
			name:    "expired callback",
			advance: true,
			duplicate: func(attempt Attempt, state string) string {
				return attempt.CallbackURL + "?state=" + url.QueryEscape(state) + "&code=duplicate"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
			store := newTestStore()
			driver := testOAuthDriver(clock)
			exchangeStarted := make(chan struct{})
			releaseExchange := make(chan struct{})
			driver.exchange = func(ctx context.Context, _ CodeExchange) (TokenSet, error) {
				close(exchangeStarted)
				select {
				case <-ctx.Done():
					return TokenSet{}, ctx.Err()
				case <-releaseExchange:
					return validTestTokens(clock, "account-1"), nil
				}
			}
			manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
			defer closeManager(t, manager)

			attempt, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0)
			if err != nil {
				t.Fatal(err)
			}
			state := authorizationState(t, attempt.AuthorizationURL)
			type result struct {
				attempt Attempt
				err     error
			}
			firstResult := make(chan result, 1)
			go func() {
				got, submitErr := manager.Submit(context.Background(), "owner-a", attempt.ID, attempt.CallbackURL+"?state="+url.QueryEscape(state)+"&code=ok")
				firstResult <- result{attempt: got, err: submitErr}
			}()
			<-exchangeStarted
			if test.advance {
				clock.Advance(authorizationTTL + time.Second)
			}

			duplicateContext := newObservedDoneContext()
			duplicateResult := make(chan result, 1)
			go func() {
				got, submitErr := manager.Submit(duplicateContext, "owner-a", attempt.ID, test.duplicate(attempt, state))
				duplicateResult <- result{attempt: got, err: submitErr}
			}()
			select {
			case early := <-duplicateResult:
				close(releaseExchange)
				first := <-firstResult
				t.Fatalf("duplicate returned before exchange completed: duplicate = %#v, err = %v; first = %#v, err = %v", early.attempt, early.err, first.attempt, first.err)
			case <-duplicateContext.observed:
			}

			close(releaseExchange)
			first := <-firstResult
			duplicate := <-duplicateResult
			if first.err != nil || first.attempt.Status != AttemptComplete {
				t.Fatalf("first result = %#v, err = %v", first.attempt, first.err)
			}
			if duplicate.err != nil || duplicate.attempt.Status != AttemptComplete {
				t.Fatalf("duplicate result = %#v, err = %v", duplicate.attempt, duplicate.err)
			}
			if duplicate.attempt.CredentialID == 0 || duplicate.attempt.CredentialID != first.attempt.CredentialID {
				t.Fatalf("credential IDs = first %d, duplicate %d", first.attempt.CredentialID, duplicate.attempt.CredentialID)
			}
			records, err := store.ListLLMCredentials(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 {
				t.Fatalf("persisted credentials = %d, want 1", len(records))
			}
		})
	}
}

func TestMalformedManualSubmitRacesCompletingExchange(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	driver := testOAuthDriver(clock)
	exchangeStarted := make(chan struct{})
	releaseExchange := make(chan struct{})
	exchangeReturning := make(chan struct{})
	driver.exchange = func(ctx context.Context, _ CodeExchange) (TokenSet, error) {
		close(exchangeStarted)
		select {
		case <-ctx.Done():
			return TokenSet{}, ctx.Err()
		case <-releaseExchange:
			close(exchangeReturning)
			return validTestTokens(clock, "account-1"), nil
		}
	}
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	attempt, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0)
	if err != nil {
		t.Fatal(err)
	}
	state := authorizationState(t, attempt.AuthorizationURL)
	type result struct {
		attempt Attempt
		err     error
	}
	completion := make(chan result, 1)
	go func() {
		got, submitErr := manager.Submit(context.Background(), "owner-a", attempt.ID, attempt.CallbackURL+"?state="+url.QueryEscape(state)+"&code=ok")
		completion <- result{attempt: got, err: submitErr}
	}()
	<-exchangeStarted

	const malformedSubmitters = 32
	malformed := make(chan result, malformedSubmitters)
	var queued sync.WaitGroup
	queued.Add(malformedSubmitters)
	manager.mu.Lock()
	for range malformedSubmitters {
		go func() {
			queued.Done()
			got, submitErr := manager.Submit(
				context.Background(),
				"owner-a",
				attempt.ID,
				strings.Replace(attempt.CallbackURL, "/callback", "/malformed", 1)+"?state="+url.QueryEscape(state),
			)
			malformed <- result{attempt: got, err: submitErr}
		}()
	}
	queued.Wait()
	close(releaseExchange)
	<-exchangeReturning
	manager.mu.Unlock()

	completed := <-completion
	if completed.err != nil || completed.attempt.Status != AttemptComplete || completed.attempt.CredentialID == 0 {
		t.Fatalf("completion = %#v, err = %v", completed.attempt, completed.err)
	}
	for range malformedSubmitters {
		got := <-malformed
		if got.err == nil {
			if got.attempt.Status != AttemptComplete || got.attempt.CredentialID != completed.attempt.CredentialID {
				t.Fatalf("terminal malformed result = %#v, err = %v", got.attempt, got.err)
			}
			continue
		}
		if errorCode(got.err) != CodeRedirectMismatch || got.attempt.Status != AttemptPending {
			t.Fatalf("pending malformed result = %#v, err = %v", got.attempt, got.err)
		}
	}
	records, err := store.ListLLMCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("persisted credentials = %d, want 1", len(records))
	}
}

func TestCallbackListenerReleasedWithLastPendingAttempt(t *testing.T) {
	tests := []struct {
		name string
		stop func(*testing.T, *testClock, *Manager, Attempt, string)
	}{
		{
			name: "complete",
			stop: func(t *testing.T, _ *testClock, manager *Manager, attempt Attempt, state string) {
				t.Helper()
				result, err := manager.Submit(context.Background(), "owner-a", attempt.ID, attempt.CallbackURL+"?state="+url.QueryEscape(state)+"&code=ok")
				if err != nil || result.Status != AttemptComplete {
					t.Fatalf("complete result = %#v, err = %v", result, err)
				}
			},
		},
		{
			name: "failure",
			stop: func(t *testing.T, _ *testClock, manager *Manager, attempt Attempt, state string) {
				t.Helper()
				result, err := manager.Submit(context.Background(), "owner-a", attempt.ID, attempt.CallbackURL+"?state="+url.QueryEscape(state)+"&error=access_denied")
				if errorCode(err) != CodeAuthorizationFailed || result.Status != AttemptFailed {
					t.Fatalf("failed result = %#v, err = %v", result, err)
				}
			},
		},
		{
			name: "expiry",
			stop: func(t *testing.T, clock *testClock, manager *Manager, attempt Attempt, _ string) {
				t.Helper()
				clock.Advance(authorizationTTL + time.Second)
				result, err := manager.Attempt(context.Background(), "owner-a", attempt.ID)
				if err != nil || result.Status != AttemptFailed {
					t.Fatalf("expired result = %#v, err = %v", result, err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
			port := availableLoopbackPort(t)
			driver := testOAuthDriver(clock)
			driver.callback = testCallbackConfig(port)
			manager := NewManager(newTestStore(), Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
			defer closeManager(t, manager)

			attempt, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0)
			if err != nil {
				t.Fatal(err)
			}
			test.stop(t, clock, manager, attempt, authorizationState(t, attempt.AuthorizationURL))
			assertLoopbackPortAvailable(t, port)
		})
	}

	t.Run("idle expiry", func(t *testing.T) {
		clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
		port := availableLoopbackPort(t)
		driver := testOAuthDriver(clock)
		driver.callback = testCallbackConfig(port)
		manager := NewManager(newTestStore(), Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
		defer closeManager(t, manager)
		scheduled := make(chan *testAuthorizationTimer, 1)
		manager.scheduleExpiry = func(_ time.Duration, expire func()) authorizationTimer {
			timer := newTestAuthorizationTimer(expire)
			scheduled <- timer
			return timer
		}

		attempt, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0)
		if err != nil {
			t.Fatal(err)
		}
		timer := <-scheduled
		clock.Advance(authorizationTTL + time.Second)
		if !timer.fire() {
			t.Fatal("authorization expiry timer was not active")
		}
		assertLoopbackPortAvailable(t, port)
		result, err := manager.Attempt(context.Background(), "owner-a", attempt.ID)
		if err != nil || result.Status != AttemptFailed || result.Error == "" {
			t.Fatalf("idle expiry result = %#v, err = %v", result, err)
		}
	})

	t.Run("shutdown cancels expiry", func(t *testing.T) {
		clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
		port := availableLoopbackPort(t)
		driver := testOAuthDriver(clock)
		driver.callback = testCallbackConfig(port)
		manager := NewManager(newTestStore(), Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
		scheduled := make(chan *testAuthorizationTimer, 1)
		manager.scheduleExpiry = func(_ time.Duration, expire func()) authorizationTimer {
			timer := newTestAuthorizationTimer(expire)
			scheduled <- timer
			return timer
		}

		if _, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0); err != nil {
			t.Fatal(err)
		}
		timer := <-scheduled
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if timer.fire() {
			t.Fatal("authorization expiry timer remained active after shutdown")
		}
		assertLoopbackPortAvailable(t, port)
	})

	t.Run("retained for another attempt", func(t *testing.T) {
		clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
		port := availableLoopbackPort(t)
		driver := testOAuthDriver(clock)
		driver.callback = testCallbackConfig(port)
		manager := NewManager(newTestStore(), Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
		defer closeManager(t, manager)

		first, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0)
		if err != nil {
			t.Fatal(err)
		}
		second, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0)
		if err != nil {
			t.Fatal(err)
		}
		firstState := authorizationState(t, first.AuthorizationURL)
		firstResult, err := manager.Submit(context.Background(), "owner-a", first.ID, first.CallbackURL+"?state="+url.QueryEscape(firstState)+"&error=access_denied")
		if errorCode(err) != CodeAuthorizationFailed || firstResult.Status != AttemptFailed {
			t.Fatalf("first result = %#v, err = %v", firstResult, err)
		}
		assertLoopbackPortUnavailable(t, port)

		secondState := authorizationState(t, second.AuthorizationURL)
		secondResult, err := manager.Submit(context.Background(), "owner-a", second.ID, second.CallbackURL+"?state="+url.QueryEscape(secondState)+"&error=access_denied")
		if errorCode(err) != CodeAuthorizationFailed || secondResult.Status != AttemptFailed {
			t.Fatalf("second result = %#v, err = %v", secondResult, err)
		}
		assertLoopbackPortAvailable(t, port)
	})
}

func TestConcurrentCallbackAndManualSubmitKeepExchangeInputsBoundToAttempts(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	port := availableLoopbackPort(t)
	callbackURL := "http://127.0.0.1:" + strconv.Itoa(port) + "/callback"
	driver := testOAuthDriver(clock)
	driver.callback = CallbackConfig{RedirectURI: callbackURL, Port: port, Path: "/callback"}

	exchangeInputs := make(chan CodeExchange, 2)
	releaseExchange := make(chan struct{})
	var releaseOnce sync.Once
	driver.exchange = func(ctx context.Context, input CodeExchange) (TokenSet, error) {
		exchangeInputs <- input
		select {
		case <-ctx.Done():
			return TokenSet{}, ctx.Err()
		case <-releaseExchange:
			return validTestTokens(clock, "account-"+input.Code), nil
		}
	}
	manager := NewManager(newTestStore(), Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)
	defer releaseOnce.Do(func() { close(releaseExchange) })

	callbackAttempt, err := manager.Begin(context.Background(), "callback-owner", ProviderClaude, 0)
	if err != nil {
		t.Fatal(err)
	}
	manualAttempt, err := manager.Begin(context.Background(), "manual-owner", ProviderClaude, 0)
	if err != nil {
		t.Fatal(err)
	}
	callbackState, callbackChallenge := authorizationParameters(t, callbackAttempt.AuthorizationURL)
	manualState, manualChallenge := authorizationParameters(t, manualAttempt.AuthorizationURL)
	if callbackState == manualState || callbackChallenge == manualChallenge {
		t.Fatal("concurrent authorization attempts reused state or PKCE challenge")
	}

	type callbackResult struct {
		status int
		body   []byte
		err    error
	}
	callbackDone := make(chan callbackResult, 1)
	go func() {
		response, requestErr := (&http.Client{Timeout: 2 * time.Second}).Get(
			callbackURL + "?state=" + url.QueryEscape(callbackState) + "&code=callback-code",
		)
		if requestErr != nil {
			callbackDone <- callbackResult{err: requestErr}
			return
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		callbackDone <- callbackResult{status: response.StatusCode, body: body, err: readErr}
	}()

	type submitResult struct {
		attempt Attempt
		err     error
	}
	manualDone := make(chan submitResult, 1)
	go func() {
		result, submitErr := manager.Submit(
			context.Background(),
			"manual-owner",
			manualAttempt.ID,
			manualAttempt.CallbackURL+"?state="+url.QueryEscape(manualState)+"&code=manual-code",
		)
		manualDone <- submitResult{attempt: result, err: submitErr}
	}()

	observed := make(map[string]CodeExchange, 2)
	for range 2 {
		input := <-exchangeInputs
		observed[input.Code] = input
	}
	releaseOnce.Do(func() { close(releaseExchange) })

	callback := <-callbackDone
	if callback.err != nil {
		t.Fatal(callback.err)
	}
	if callback.status != http.StatusOK {
		t.Fatalf("callback status = %d, body = %q", callback.status, callback.body)
	}
	manual := <-manualDone
	if manual.err != nil || manual.attempt.Status != AttemptComplete {
		t.Fatalf("manual result = %#v, err = %v", manual.attempt, manual.err)
	}
	completedCallback, err := manager.Attempt(context.Background(), "callback-owner", callbackAttempt.ID)
	if err != nil || completedCallback.Status != AttemptComplete {
		t.Fatalf("callback result = %#v, err = %v", completedCallback, err)
	}

	expected := map[string]struct {
		state       string
		challenge   string
		redirectURI string
	}{
		"callback-code": {state: callbackState, challenge: callbackChallenge, redirectURI: callbackAttempt.CallbackURL},
		"manual-code":   {state: manualState, challenge: manualChallenge, redirectURI: manualAttempt.CallbackURL},
	}
	if len(observed) != len(expected) {
		t.Fatalf("exchange input count = %d, want %d", len(observed), len(expected))
	}
	for code, want := range expected {
		input, found := observed[code]
		if !found {
			t.Fatalf("exchange input for code %q was not observed", code)
		}
		if input.Code != code || input.State != want.state || input.RedirectURI != want.redirectURI || input.Verifier == "" {
			t.Fatalf("exchange input for code %q did not preserve its validated attempt", code)
		}
		digest := sha256.Sum256([]byte(input.Verifier))
		challenge := base64.RawURLEncoding.EncodeToString(digest[:])
		if challenge != want.challenge {
			t.Fatalf("exchange input for code %q used PKCE challenge %q, want %q", code, challenge, want.challenge)
		}
	}
}

func TestCallbackListenerStartsLazilyAndReturnsSecretFreeAttempt(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	port := availableLoopbackPort(t)
	callbackURL := "http://127.0.0.1:" + strconv.Itoa(port) + "/callback"
	driver := testOAuthDriver(clock)
	driver.callback = CallbackConfig{RedirectURI: callbackURL, Port: port, Path: "/callback"}
	driver.exchangeTokens["browser-secret-code"] = TokenSet{
		AccessToken:  "access-token-never-in-dto",
		RefreshToken: "refresh-token-never-in-dto",
		ExpiresAt:    clock.Now().Add(time.Hour),
		AccountID:    "account-1",
	}
	manager := NewManager(newTestStore(), Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	probe, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("port was not free before Begin: %v", err)
	}
	_ = probe.Close()
	attempt, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, 0)
	if err != nil {
		t.Fatal(err)
	}
	state := authorizationState(t, attempt.AuthorizationURL)
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(callbackURL + "?state=" + url.QueryEscape(state) + "&code=browser-secret-code")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, body = %q", response.StatusCode, body)
	}
	completed, err := manager.Attempt(context.Background(), "owner-a", attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != AttemptComplete || completed.CredentialID == 0 {
		t.Fatalf("completed attempt = %#v", completed)
	}
	encoded, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"browser-secret-code", "access-token-never-in-dto", "refresh-token-never-in-dto"} {
		if strings.Contains(string(encoded), secret) || strings.Contains(string(body), secret) {
			t.Fatalf("secret %q leaked in callback response or DTO", secret)
		}
	}
}

func TestPendingReloginCannotResurrectDisconnectedCredential(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	credentialID := store.seed(ProviderClaude, CredentialReady, TokenSet{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    clock.Now().Add(time.Hour),
		AccountID:    "account-1",
		OrgID:        "org-1",
	})
	driver := testOAuthDriver(clock)
	driver.exchangeTokens["same-account"] = TokenSet{
		AccessToken:  "reauth-access",
		RefreshToken: "reauth-refresh",
		ExpiresAt:    clock.Now().Add(2 * time.Hour),
		AccountID:    "account-1",
		OrgID:        "org-1",
	}
	driver.exchangeTokens["other-account"] = TokenSet{
		AccessToken:  "other-access",
		RefreshToken: "other-refresh",
		ExpiresAt:    clock.Now().Add(2 * time.Hour),
		AccountID:    "account-2",
		OrgID:        "org-1",
	}
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	pending, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, credentialID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Disconnect(context.Background(), credentialID); err != nil {
		t.Fatal(err)
	}
	state := authorizationState(t, pending.AuthorizationURL)
	result, err := manager.Submit(context.Background(), "owner-a", pending.ID, pending.CallbackURL+"?state="+state+"&code=same-account")
	if errorCode(err) != CodeCredentialChanged || result.Status != AttemptFailed {
		t.Fatalf("pre-disconnect attempt result = %#v, err = %v", result, err)
	}
	record, _ := store.LLMCredential(context.Background(), credentialID)
	if record.Status != CredentialRevoked {
		t.Fatalf("credential status = %q, want revoked", record.Status)
	}

	relogin, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, credentialID)
	if err != nil {
		t.Fatal(err)
	}
	reloginState := authorizationState(t, relogin.AuthorizationURL)
	result, err = manager.Submit(context.Background(), "owner-a", relogin.ID, relogin.CallbackURL+"?state="+reloginState+"&code=same-account")
	if err != nil || result.Status != AttemptComplete {
		t.Fatalf("same-account relogin result = %#v, err = %v", result, err)
	}

	identitySwap, err := manager.Begin(context.Background(), "owner-a", ProviderClaude, credentialID)
	if err != nil {
		t.Fatal(err)
	}
	swapState := authorizationState(t, identitySwap.AuthorizationURL)
	result, err = manager.Submit(context.Background(), "owner-a", identitySwap.ID, identitySwap.CallbackURL+"?state="+swapState+"&code=other-account")
	if errorCode(err) != CodeIdentityChanged || result.Status != AttemptFailed {
		t.Fatalf("identity swap result = %#v, err = %v", result, err)
	}
}

func TestRegisterRequestRejectsStaleBindingAndDisconnectCancelsOnlySelectedCredential(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	firstID := store.seed(ProviderClaude, CredentialReady, validTestTokens(clock, "account-1"))
	secondID := store.seed(ProviderClaude, CredentialReady, validTestTokens(clock, "account-2"))
	store.bind(21, firstID, 3)
	store.bind(22, secondID, 7)
	driver := testOAuthDriver(clock)
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	if _, _, err := manager.RegisterRequest(context.Background(), Binding{ProfileID: 21, CredentialID: firstID, Generation: 2}); errorCode(err) != CodeBindingChanged {
		t.Fatalf("stale binding error = %v", err)
	}
	firstCtx, releaseFirst, err := manager.RegisterRequest(context.Background(), Binding{ProfileID: 21, CredentialID: firstID, Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	secondCtx, releaseSecond, err := manager.RegisterRequest(context.Background(), Binding{ProfileID: 22, CredentialID: secondID, Generation: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()

	if err := manager.Disconnect(context.Background(), firstID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstCtx.Done():
	default:
		t.Fatal("selected credential request was not canceled")
	}
	select {
	case <-secondCtx.Done():
		t.Fatal("unrelated credential request was canceled")
	default:
	}
	record, _ := store.LLMCredential(context.Background(), firstID)
	payload, err := decodeCredential(record, ProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != CredentialRevoked || payload.Tokens.AccessToken != "" || payload.Tokens.RefreshToken != "" {
		t.Fatalf("durable revoke record = %#v, payload = %#v", record, payload)
	}
}

func TestDisconnectReportsUnsupportedRemoteRevocationAfterLocalRevoke(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	credentialID := store.seed(ProviderClaude, CredentialReady, validTestTokens(clock, "account-1"))
	driver := testOAuthDriver(clock)
	driver.revoke = func(context.Context, TokenSet) error { return ErrRevocationUnsupported }
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	err := manager.Disconnect(context.Background(), credentialID)
	if !errors.Is(err, ErrRevocationUnsupported) || !IsTerminal(err) {
		t.Fatalf("disconnect error = %v", err)
	}
	record, _ := store.LLMCredential(context.Background(), credentialID)
	if record.Status != CredentialRevoked {
		t.Fatalf("credential status = %q, want revoked", record.Status)
	}
}

func TestProviderRefreshFailureIsSanitizedAndTerminal(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	credentialID := store.seed(ProviderClaude, CredentialReady, TokenSet{
		AccessToken:  "expired-access",
		RefreshToken: "secret-refresh-token",
		ExpiresAt:    clock.Now().Add(-time.Minute),
		AccountID:    "account-1",
	})
	store.bind(61, credentialID, 1)
	driver := testOAuthDriver(clock)
	driver.refresh = func(context.Context, TokenSet) (TokenSet, error) {
		return TokenSet{}, errors.New("upstream body exposed secret-refresh-token")
	}
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	_, err := manager.Credential(context.Background(), Binding{ProfileID: 61, CredentialID: credentialID, Generation: 1})
	if errorCode(err) != CodeRefreshFailed || !IsTerminal(err) {
		t.Fatalf("refresh error = %v", err)
	}
	if strings.Contains(err.Error(), "secret-refresh-token") || strings.Contains(err.Error(), "upstream body") {
		t.Fatalf("provider failure leaked raw error: %v", err)
	}
}

func TestRefreshRejectsIdentityChangeWithoutOverwritingCredential(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	credentialID := store.seed(ProviderClaude, CredentialReady, TokenSet{
		AccessToken:  "expired-access",
		RefreshToken: "refresh-1",
		ExpiresAt:    clock.Now().Add(-time.Minute),
		AccountID:    "account-1",
		OrgID:        "org-1",
	})
	store.bind(31, credentialID, 1)
	driver := testOAuthDriver(clock)
	driver.refresh = func(context.Context, TokenSet) (TokenSet, error) {
		return TokenSet{
			AccessToken: "wrong-access",
			ExpiresAt:   clock.Now().Add(time.Hour),
			AccountID:   "account-2",
			OrgID:       "org-1",
		}, nil
	}
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})
	defer closeManager(t, manager)

	_, err := manager.Credential(context.Background(), Binding{ProfileID: 31, CredentialID: credentialID, Generation: 1})
	if errorCode(err) != CodeIdentityChanged {
		t.Fatalf("identity change error = %v", err)
	}
	record, _ := store.LLMCredential(context.Background(), credentialID)
	payload, decodeErr := decodeCredential(record, ProviderClaude)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if payload.Tokens.AccessToken != "expired-access" || record.Revision != 1 {
		t.Fatalf("identity-changing refresh was persisted: record = %#v, payload = %#v", record, payload)
	}
}

func TestCloseDrainsRefreshPersistence(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	credentialID := store.seed(ProviderClaude, CredentialReady, TokenSet{
		AccessToken:  "expired-access",
		RefreshToken: "refresh-1",
		ExpiresAt:    clock.Now().Add(-time.Minute),
		AccountID:    "account-1",
	})
	store.bind(41, credentialID, 1)
	refreshEntered := make(chan struct{})
	refreshRelease := make(chan struct{})
	driver := testOAuthDriver(clock)
	driver.refresh = func(context.Context, TokenSet) (TokenSet, error) {
		close(refreshEntered)
		<-refreshRelease
		return TokenSet{
			AccessToken: "persisted-before-close",
			ExpiresAt:   clock.Now().Add(time.Hour),
			AccountID:   "account-1",
		}, nil
	}
	manager := NewManager(store, Options{Drivers: []OAuthDriver{driver}, Now: clock.Now})

	credentialDone := make(chan error, 1)
	go func() {
		_, err := manager.Credential(context.Background(), Binding{ProfileID: 41, CredentialID: credentialID, Generation: 1})
		credentialDone <- err
	}()
	<-refreshEntered
	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		closeDone <- manager.Close(ctx)
	}()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before refresh persistence: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(refreshRelease)
	if err := <-credentialDone; err != nil {
		t.Fatalf("refresh failed during close: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	record, _ := store.LLMCredential(context.Background(), credentialID)
	payload, err := decodeCredential(record, ProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Tokens.AccessToken != "persisted-before-close" {
		t.Fatalf("persisted token = %q", payload.Tokens.AccessToken)
	}
	if _, err := manager.Credential(context.Background(), Binding{ProfileID: 41, CredentialID: credentialID, Generation: 1}); errorCode(err) != CodeManagerClosed {
		t.Fatalf("post-close credential error = %v", err)
	}
}

func testOAuthDriver(clock *testClock) *testDriver {
	port := availableLoopbackPortValue()
	return &testDriver{
		kind: ProviderClaude,
		callback: CallbackConfig{
			RedirectURI: "http://127.0.0.1:" + strconv.Itoa(port) + "/callback",
			Port:        port,
			Path:        "/callback",
		},
		clock: clock,
		exchangeTokens: map[string]TokenSet{
			"ok": validTestTokens(clock, "account-1"),
		},
	}
}

func validTestTokens(clock *testClock, accountID string) TokenSet {
	return TokenSet{
		AccessToken:  "access-" + accountID,
		RefreshToken: "refresh-" + accountID,
		ExpiresAt:    clock.Now().Add(time.Hour),
		AccountID:    accountID,
	}
}

func authorizationState(t *testing.T, authorizationURL string) string {
	t.Helper()
	state, _ := authorizationParameters(t, authorizationURL)
	return state
}

func authorizationParameters(t *testing.T, authorizationURL string) (state, challenge string) {
	t.Helper()
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	values := parsed.Query()
	state = values.Get("state")
	challenge = values.Get("code_challenge")
	if state == "" || challenge == "" {
		t.Fatal("authorization URL has no state or PKCE challenge")
	}
	return state, challenge
}

func availableLoopbackPort(t *testing.T) int {
	t.Helper()
	port := availableLoopbackPortValue()
	if port == 0 {
		t.Fatal("could not allocate loopback port")
	}
	return port
}

func availableLoopbackPortValue() int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func testCallbackConfig(port int) CallbackConfig {
	return CallbackConfig{
		RedirectURI: "http://127.0.0.1:" + strconv.Itoa(port) + "/callback",
		Port:        port,
		Path:        "/callback",
	}
}

func assertLoopbackPortAvailable(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("callback port %d is still bound: %v", port, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close callback port %d probe: %v", port, err)
	}
}

func assertLoopbackPortUnavailable(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return
	}
	_ = listener.Close()
	t.Fatalf("callback port %d was released with another pending attempt", port)
}

func errorCode(err error) string {
	var subscriptionErr *Error
	if errors.As(err, &subscriptionErr) {
		return subscriptionErr.Code
	}
	return ""
}

func closeManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}
