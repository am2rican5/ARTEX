package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	credentialLockAcquireTimeout = 15 * time.Second
	credentialLockCleanupTimeout = 5 * time.Second
	credentialAdvisoryLockBase   = int64(-1 << 63)
)

var ErrCredentialChanged = errors.New("LLM credential changed")

type LLMCredential struct {
	ID         int64           `json:"id"`
	Provider   string          `json:"provider"`
	Label      string          `json:"label"`
	Status     string          `json:"status"`
	Revision   int64           `json:"revision"`
	Generation int64           `json:"generation"`
	Payload    json.RawMessage `json:"-"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

func validLLMCredentialProvider(provider string) bool {
	return provider == "anthropic-oauth" || provider == "openai-codex-oauth"
}

func validLLMCredentialStatus(status string) bool {
	return status == "ready" || status == "reauth_required" || status == "revoked"
}

func normalizeLLMCredential(c *LLMCredential) error {
	c.Provider = strings.TrimSpace(c.Provider)
	c.Label = strings.TrimSpace(c.Label)
	if !validLLMCredentialProvider(c.Provider) {
		return fmt.Errorf("unsupported LLM credential provider %q", c.Provider)
	}
	if c.Label == "" {
		return errors.New("LLM credential label is required")
	}
	if !validLLMCredentialStatus(c.Status) {
		return fmt.Errorf("unsupported LLM credential status %q", c.Status)
	}
	if len(c.Payload) == 0 || !json.Valid(c.Payload) {
		return errors.New("LLM credential payload must be valid JSON")
	}
	return nil
}

func (d *DB) CreateLLMCredential(ctx context.Context, provider, label string, payload json.RawMessage) (*LLMCredential, error) {
	c := &LLMCredential{
		Provider:   provider,
		Label:      label,
		Status:     "ready",
		Generation: 1,
		Payload:    append(json.RawMessage(nil), payload...),
	}
	if err := normalizeLLMCredential(c); err != nil {
		return nil, err
	}
	sealed, err := sealSecret(colLLMCredentialPayload, string(c.Payload))
	if err != nil {
		return nil, err
	}
	err = d.QueryRowContext(ctx, `INSERT INTO llm_credentials(provider,label,status,revision,generation,payload)
VALUES ($1,$2,$3,1,$4,$5::jsonb)
RETURNING id,revision,created_at,updated_at`, c.Provider, c.Label, c.Status, c.Generation, sealed).
		Scan(&c.ID, &c.Revision, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (d *DB) LLMCredential(ctx context.Context, id int64) (*LLMCredential, error) {
	var (
		c      LLMCredential
		sealed string
	)
	err := d.QueryRowContext(ctx, `SELECT id,provider,label,status,revision,generation,payload::text,created_at,updated_at
FROM llm_credentials WHERE id=$1`, id).
		Scan(&c.ID, &c.Provider, &c.Label, &c.Status, &c.Revision, &c.Generation, &sealed, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := openSecret(colLLMCredentialPayload, sealed)
	if err != nil {
		return nil, err
	}
	c.Payload = json.RawMessage(plain)
	return &c, nil
}

func (d *DB) ListLLMCredentials(ctx context.Context) ([]LLMCredential, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,provider,label,status,revision,generation,created_at,updated_at
FROM llm_credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LLMCredential, 0)
	for rows.Next() {
		var c LLMCredential
		if err := rows.Scan(&c.ID, &c.Provider, &c.Label, &c.Status, &c.Revision, &c.Generation, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) UpdateLLMCredential(ctx context.Context, c *LLMCredential, expectedRevision int64) error {
	if c == nil {
		return errors.New("LLM credential is nil")
	}
	if err := normalizeLLMCredential(c); err != nil {
		return err
	}
	sealed, err := sealSecret(colLLMCredentialPayload, string(c.Payload))
	if err != nil {
		return err
	}
	err = d.QueryRowContext(ctx, `UPDATE llm_credentials
SET provider=$1,label=$2,status=$3,generation=$4,payload=$5::jsonb,revision=revision+1
WHERE id=$6 AND revision=$7
RETURNING revision,updated_at`, c.Provider, c.Label, c.Status, c.Generation, sealed, c.ID, expectedRevision).
		Scan(&c.Revision, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCredentialChanged
	}
	return err
}

func credentialAdvisoryLockKey(id int64) (int64, error) {
	if id <= 0 {
		return 0, fmt.Errorf("invalid LLM credential id %d", id)
	}
	return credentialAdvisoryLockBase + id, nil
}

func discardCredentialConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func (d *DB) LockLLMCredential(ctx context.Context, id int64) (func(), error) {
	key, err := credentialAdvisoryLockKey(id)
	if err != nil {
		return nil, err
	}
	acquireCtx, cancel := context.WithTimeout(ctx, credentialLockAcquireTimeout)
	defer cancel()
	conn, err := d.Conn(acquireCtx)
	if err != nil {
		if ctxErr := acquireCtx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if _, err := conn.ExecContext(acquireCtx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		discardCredentialConn(conn)
		if ctxErr := acquireCtx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("lock LLM credential %d: %w", id, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), credentialLockCleanupTimeout)
			defer cleanupCancel()
			var unlocked bool
			err := conn.QueryRowContext(cleanupCtx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
			if err != nil || !unlocked {
				discardCredentialConn(conn)
				return
			}
			_ = conn.Close()
		})
	}, nil
}

func (d *DB) ProfileCredentialBinding(ctx context.Context, profileID int64) (credentialID, generation int64, err error) {
	var authMode string
	var id sql.NullInt64
	err = d.QueryRowContext(ctx, `SELECT auth_mode,credential_id,auth_binding_generation
FROM llm_profiles WHERE id=$1`, profileID).Scan(&authMode, &id, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if authMode != "oauth" || !id.Valid {
		return 0, generation, nil
	}
	return id.Int64, generation, nil
}
