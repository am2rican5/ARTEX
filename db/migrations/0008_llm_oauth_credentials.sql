-- Native OAuth credentials are encrypted JSONB records shared by one or more
-- LLM profiles. payload is sealed by db/secrets.go after migrations run.
CREATE TABLE IF NOT EXISTS llm_credentials (
    id         BIGSERIAL PRIMARY KEY,
    provider   TEXT NOT NULL CHECK (provider IN ('anthropic-oauth','openai-codex-oauth')),
    label      TEXT NOT NULL CHECK (btrim(label) <> ''),
    status     TEXT NOT NULL DEFAULT 'ready'
               CHECK (status IN ('ready','reauth_required','revoked')),
    revision   BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_llm_credentials_upd ON llm_credentials;
CREATE TRIGGER trg_llm_credentials_upd BEFORE UPDATE ON llm_credentials
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS auth_mode TEXT NOT NULL DEFAULT 'api_key';
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS credential_id BIGINT;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS auth_binding_generation BIGINT NOT NULL DEFAULT 0;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='llm_profiles'::regclass
          AND conname='llm_profiles_credential_id_fkey'
    ) THEN
        ALTER TABLE llm_profiles ADD CONSTRAINT llm_profiles_credential_id_fkey
            FOREIGN KEY (credential_id) REFERENCES llm_credentials(id) ON DELETE RESTRICT;
    END IF;
END $$;
CREATE INDEX IF NOT EXISTS idx_llm_profiles_credential
    ON llm_profiles(credential_id) WHERE credential_id IS NOT NULL;
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_auth_mode_check;
ALTER TABLE llm_profiles ADD CONSTRAINT llm_profiles_auth_mode_check
    CHECK (auth_mode IN ('api_key','oauth'));
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_auth_binding_generation_check;
ALTER TABLE llm_profiles ADD CONSTRAINT llm_profiles_auth_binding_generation_check
    CHECK (auth_binding_generation >= 0);
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_auth_binding_check;
ALTER TABLE llm_profiles ADD CONSTRAINT llm_profiles_auth_binding_check CHECK (
    (auth_mode='api_key' AND credential_id IS NULL)
    OR (auth_mode='oauth' AND credential_id IS NOT NULL AND api_key IS NULL)
);

CREATE OR REPLACE FUNCTION validate_llm_profile_auth() RETURNS trigger AS $$
DECLARE credential_provider TEXT;
BEGIN
    IF NEW.auth_mode = 'api_key' THEN
        RETURN NEW;
    END IF;
    SELECT provider INTO credential_provider
      FROM llm_credentials WHERE id=NEW.credential_id FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'LLM credential % does not exist', NEW.credential_id
            USING ERRCODE='foreign_key_violation';
    END IF;
    IF NOT ((credential_provider='anthropic-oauth' AND NEW.format='anthropic')
            OR (credential_provider='openai-codex-oauth' AND NEW.format='openai-responses')) THEN
        RAISE EXCEPTION 'LLM credential provider % is incompatible with profile format %',
            credential_provider, NEW.format USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_llm_profiles_auth ON llm_profiles;
CREATE TRIGGER trg_llm_profiles_auth
    BEFORE INSERT OR UPDATE OF auth_mode,credential_id,format,api_key ON llm_profiles
    FOR EACH ROW EXECUTE FUNCTION validate_llm_profile_auth();

CREATE OR REPLACE FUNCTION validate_llm_credential_provider() RETURNS trigger AS $$
BEGIN
    IF NEW.provider IS DISTINCT FROM OLD.provider AND EXISTS (
        SELECT 1 FROM llm_profiles p
        WHERE p.credential_id=NEW.id
          AND NOT ((NEW.provider='anthropic-oauth' AND p.format='anthropic')
                   OR (NEW.provider='openai-codex-oauth' AND p.format='openai-responses'))
    ) THEN
        RAISE EXCEPTION 'LLM credential provider % is incompatible with a bound profile', NEW.provider
            USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_llm_credentials_provider ON llm_credentials;
CREATE TRIGGER trg_llm_credentials_provider
    BEFORE UPDATE OF provider ON llm_credentials
    FOR EACH ROW EXECUTE FUNCTION validate_llm_credential_provider();
