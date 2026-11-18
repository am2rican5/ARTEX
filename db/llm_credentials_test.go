package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLLMCredentialEncryptionAndMetadataList(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	payload := json.RawMessage(`{"access_token":"fixture-access-secret","refresh_token":"fixture-refresh-secret","expires_at":"2030-01-01T00:00:00Z"}`)
	credential, err := d.CreateLLMCredential(context.Background(), "anthropic-oauth", "encrypted fixture", payload)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID) })

	var stored string
	if err := d.QueryRow(`SELECT payload::text FROM llm_credentials WHERE id=$1`, credential.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !isSealed(colLLMCredentialPayload, stored) || strings.Contains(stored, "fixture-access-secret") || strings.Contains(stored, "fixture-refresh-secret") {
		t.Fatalf("credential payload is not sealed: %s", stored)
	}

	loaded, err := d.LLMCredential(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || !json.Valid(loaded.Payload) || !strings.Contains(string(loaded.Payload), "fixture-access-secret") {
		t.Fatalf("decrypted credential = %+v", loaded)
	}
	listed, err := d.ListLLMCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var metadata *LLMCredential
	for i := range listed {
		if listed[i].ID == credential.ID {
			metadata = &listed[i]
			break
		}
	}
	if metadata == nil {
		t.Fatal("created credential missing from metadata list")
	}
	if len(metadata.Payload) != 0 {
		t.Fatalf("metadata list returned secret payload: %s", metadata.Payload)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "payload") || strings.Contains(string(encoded), "fixture-access-secret") {
		t.Fatalf("public credential JSON exposed a secret field: %s", encoded)
	}
}

func TestLLMCredentialSharedProfilesCASAndSessionLock(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()

	credential, err := d.CreateLLMCredential(ctx, "anthropic-oauth", "shared fixture", json.RawMessage(`{"access_token":"shared-one"}`))
	if err != nil {
		t.Fatal(err)
	}
	var (
		profileIDs []int64
		otherID    int64
	)
	t.Cleanup(func() {
		for _, id := range profileIDs {
			_, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
		}
		_, _ = d.Exec(`DELETE FROM llm_credentials WHERE id IN ($1,$2)`, credential.ID, otherID)
	})
	other, err := d.CreateLLMCredential(ctx, "anthropic-oauth", "independent fixture", json.RawMessage(`{"access_token":"other-one"}`))
	if err != nil {
		t.Fatal(err)
	}
	otherID = other.ID
	for i := range 2 {
		id, err := d.SaveProfile(&LLMProfile{
			Name:         fmt.Sprintf("oauth-shared-%d-%d", i, time.Now().UnixNano()),
			Format:       "anthropic",
			Model:        fmt.Sprintf("claude-fixture-%d", i),
			AuthMode:     "oauth",
			CredentialID: &credential.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		profileIDs = append(profileIDs, id)
		boundID, generation, err := d.ProfileCredentialBinding(ctx, id)
		if err != nil || boundID != credential.ID || generation != 0 {
			t.Fatalf("profile binding = (%d,%d,%v), want (%d,0,nil)", boundID, generation, err, credential.ID)
		}
	}

	stale := *credential
	credential.Status = "reauth_required"
	credential.Generation++
	credential.Payload = json.RawMessage(`{"access_token":"shared-two"}`)
	if err := d.UpdateLLMCredential(ctx, credential, stale.Revision); err != nil {
		t.Fatal(err)
	}
	if credential.Revision != stale.Revision+1 {
		t.Fatalf("revision = %d, want %d", credential.Revision, stale.Revision+1)
	}
	stale.Label = "stale writer"
	if err := d.UpdateLLMCredential(ctx, &stale, stale.Revision); !errors.Is(err, ErrCredentialChanged) {
		t.Fatalf("stale update error = %v, want ErrCredentialChanged", err)
	}
	for _, id := range profileIDs {
		_, generation, err := d.ProfileCredentialBinding(ctx, id)
		if err != nil || generation != 0 {
			t.Fatalf("credential refresh changed profile binding generation: %d %v", generation, err)
		}
	}

	unlockShared, err := d.LockLLMCredential(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	unlockOther, err := d.LockLLMCredential(ctx, other.ID)
	if err != nil {
		unlockShared()
		t.Fatalf("independent credential lock blocked: %v", err)
	}
	unlockOther()

	blockedCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		unlock, err := d.LockLLMCredential(blockedCtx, credential.ID)
		if unlock != nil {
			unlock()
		}
		blocked <- err
	}()
	select {
	case err := <-blocked:
		if !errors.Is(err, context.DeadlineExceeded) {
			unlockShared()
			t.Fatalf("contended credential lock error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		unlockShared()
		t.Fatal("contended credential lock ignored its bounded context")
	}
	unlockShared()
	unlockShared()
	unlockAgain, err := d.LockLLMCredential(ctx, credential.ID)
	if err != nil {
		t.Fatalf("credential lock remained held after unlock: %v", err)
	}
	unlockAgain()
}

func TestOAuthProfileBindingAndAPIKeySwitching(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()

	credential, err := d.CreateLLMCredential(ctx, "anthropic-oauth", "profile fixture", json.RawMessage(`{"access_token":"profile-token"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID) })
	profileID, err := d.SaveProfile(&LLMProfile{
		Name:     fmt.Sprintf("oauth-switch-%d", time.Now().UnixNano()),
		Format:   "openai",
		Model:    "api-fixture",
		APIKey:   "api-key-preserved-on-failed-switch",
		Priority: 999,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID) })

	profile, err := d.ProfileByID(profileID)
	if err != nil {
		t.Fatal(err)
	}
	profile.AuthMode = "oauth"
	profile.CredentialID = &credential.ID
	if _, err := d.SaveProfile(profile); err == nil {
		t.Fatal("incompatible anthropic credential was accepted by openai profile")
	}
	preserved, err := d.ProfileByID(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.AuthMode != "api_key" || preserved.CredentialID != nil || preserved.APIKey != "api-key-preserved-on-failed-switch" || preserved.AuthBindingGeneration != 0 {
		t.Fatalf("failed OAuth switch changed API profile: %+v", preserved)
	}

	preserved.Format = "anthropic"
	preserved.AuthMode = "oauth"
	preserved.CredentialID = &credential.ID
	if _, err := d.SaveProfile(preserved); err != nil {
		t.Fatal(err)
	}
	oauth, err := d.ProfileByID(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if oauth.AuthMode != "oauth" || oauth.CredentialID == nil || *oauth.CredentialID != credential.ID || oauth.APIKey != "" || oauth.APIKeyHint != "" || oauth.AuthBindingGeneration != 1 {
		t.Fatalf("OAuth profile = %+v", oauth)
	}
	boundID, generation, err := d.ProfileCredentialBinding(ctx, profileID)
	if err != nil || boundID != credential.ID || generation != oauth.AuthBindingGeneration {
		t.Fatalf("binding lookup = (%d,%d,%v)", boundID, generation, err)
	}

	oauth.Model = "claude-fixture-updated"
	if _, err := d.SaveProfile(oauth); err != nil {
		t.Fatal(err)
	}
	unchanged, err := d.ProfileByID(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.AuthBindingGeneration != generation {
		t.Fatalf("ordinary profile update changed binding generation: %d -> %d", generation, unchanged.AuthBindingGeneration)
	}

	credential.Status = "revoked"
	if err := d.UpdateLLMCredential(ctx, credential, credential.Revision); err != nil {
		t.Fatal(err)
	}
	pool, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range pool {
		if candidate.ID == profileID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("selected revoked OAuth profile disappeared from pool candidates")
	}
	if _, err := d.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID); err == nil {
		t.Fatal("referenced OAuth credential deletion succeeded")
	}

	unchanged.AuthMode = "api_key"
	unchanged.CredentialID = nil
	unchanged.APIKey = "replacement-api-key"
	if _, err := d.SaveProfile(unchanged); err != nil {
		t.Fatal(err)
	}
	apiProfile, err := d.ProfileByID(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if apiProfile.AuthMode != "api_key" || apiProfile.CredentialID != nil || apiProfile.APIKey != "replacement-api-key" || apiProfile.AuthBindingGeneration != generation+1 {
		t.Fatalf("API-key profile after switch = %+v", apiProfile)
	}

	boundID, staleGeneration, err := d.ProfileCredentialBinding(ctx, profileID)
	if err != nil || boundID != 0 || staleGeneration != apiProfile.AuthBindingGeneration {
		t.Fatalf("API-key binding lookup = (%d,%d,%v), want (0,%d,nil)", boundID, staleGeneration, err, apiProfile.AuthBindingGeneration)
	}
	apiProfile.APIKey = ""
	if _, err := d.SaveProfile(apiProfile); err != nil {
		t.Fatal(err)
	}
	preserved, err = d.ProfileByID(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.APIKey != "replacement-api-key" || preserved.AuthBindingGeneration != generation+1 {
		t.Fatalf("blank API-key update did not preserve key/binding: %+v", preserved)
	}

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := d.ProfileCredentialBinding(cancelledCtx, profileID); !errors.Is(err, context.Canceled) {
		t.Fatalf("binding lookup query error = %v, want context canceled", err)
	}

	if _, err := d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID); err != nil {
		t.Fatal(err)
	}
	boundID, staleGeneration, err = d.ProfileCredentialBinding(ctx, profileID)
	if err != nil || boundID != 0 || staleGeneration != 0 {
		t.Fatalf("deleted profile binding lookup = (%d,%d,%v), want (0,0,nil)", boundID, staleGeneration, err)
	}
}
