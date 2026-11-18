# Native subscription accounts

ARTEX can use an experimental Claude or ChatGPT Codex browser session directly from the Go process. This is an alternative to an API-key profile or an explicit Ollama profile, not an automatic replacement for either.

| Account type | Credential provider | Profile format | Fixed callback |
|---|---|---|---|
| Claude account | `anthropic-oauth` | `anthropic` | `http://localhost:54545/callback` |
| ChatGPT Codex account | `openai-codex-oauth` | `openai-responses` | `http://localhost:1455/auth/callback` |

No OMP/Bun process, broker, `auth.json`, npm package, Claude CLI, or Codex CLI is required at runtime. ARTEX stores the account session in PostgreSQL in the existing encrypted credential store. It does not install or maintain a separate native-client credential file. Do not add a placeholder or dummy encryption secret: keep the generated `keys/secret.key`, or supply one real 32-byte `ARTEX_SECRET_KEY` through the deployment's secret manager.

## Status and limits

This integration is based on protocol behavior pinned from OMP commit `1bdbf6d242d99ca62bdf2feeff4519ff992f647d`. ARTEX's in-process Norma fork is pinned to Norma v0.4.11 commit `facbfad0e8dcfaa4015522ced21290b148f8a61f` under Apache-2.0. These pins make the implementation reviewable; they do not make the providers' private or legacy protocols stable.

The authorization issuers, client identity, callback URI, model origins, and required request identity are fixed in the native adapters. A native profile cannot set an arbitrary Base URL or User-Agent. Use an API-key or Ollama profile when a custom endpoint or request identity is required.

This feature is not a provider endorsement, an official OAuth guarantee, or a terms or policy bypass. The provider can change protocol behavior, terms, account access, model access, or billing at any time. In particular, Codex uses the pinned legacy Codex flow; it is not Sign in with ChatGPT (SIWC). Confirm that the account owner authorizes use and that the deployment complies with the provider's current terms before connecting it.

ARTEX does **not** report subscription quota, balance, plan entitlement, or a promise of no additional charges. A stored session only means local credential material is available. The provider decides whether the selected model is available and whether credits or additional-use settings apply. API token prices are not used as subscription charges.

The native model list is a source-pinned catalog, not live account discovery. Catalog membership does not prove entitlement. Codex currently exposes no operator output cap and no true reasoning-off control through this protocol. “Not sent” keeps the provider default; a lower reasoning effort is not equivalent to off.

## Callback deployment

### Native binary on the host

Leave `ARTEX_OAUTH_CALLBACK_LISTEN_HOST` unset. The binary defaults to the IPv4 loopback listener `127.0.0.1`, and opens a provider port only while an authorization attempt is pending. Do not set the variable to `0.0.0.0` for a normal host installation.

The provider redirect contracts use the hostname `localhost` exactly. Do not rewrite a redirect URI to `127.0.0.1`, even though the ARTEX listener defaults to the IPv4 literal. The manager validates the exact scheme, host, port, and path that belonged to the attempt.

### Docker Compose opt-in

The default `docker-compose.yml` publishes neither callback port. Opt in only on a host where account authorization is needed:

```bash
docker compose -f docker-compose.yml -f docker-compose.oauth.yml up -d --build
```

Keep both `-f` arguments on later `up`, `stop`, `restart`, `logs`, and `down` operations for this deployment. The override:

- sets `ARTEX_OAUTH_CALLBACK_LISTEN_HOST=0.0.0.0` **inside the container only**;
- publishes Claude port 54545 and Codex port 1455 only on host `127.0.0.1`;
- gives ARTEX a two-minute stop grace period.

Container `0.0.0.0` is necessary because Docker port publishing targets the container network interface; it cannot reach a process listening on the container's own `127.0.0.1`. Publishing the ports without changing the in-container listener would not work. The host mappings remain private to host loopback and must not be changed to `0.0.0.0` or a LAN address.

ARTEX still creates its in-container callback listener lazily when **Connect new account** or **Reauthenticate** starts, and removes it when no attempt remains. Docker itself reserves the two host mappings for the life of the container.

Normal shutdown budgets 10 seconds for HTTP shutdown, then 30 seconds to cancel and settle task, main-agent, and side-question runtimes while PostgreSQL remains open, up to 5 seconds to flush circuit-breaker health, then 60 seconds for native/auth close and critical credential persistence. Individual authorization HTTP work is bounded at 30 seconds inside that final stage. The normal maximum is therefore 105 seconds, within the two-minute Compose grace period.

Docker can still force-terminate a process that overruns the grace period or is killed directly. Durable native-use markers protect the next restart: ARTEX pauses an unfinished task with an uncertain native outcome instead of issuing another model request automatically.

### Port conflicts

The provider ports are fixed; ARTEX never selects another port and never kills the process that owns one.

- With the native binary, **Connect new account** reports that the callback listener is unavailable if a fixed port is busy. Identify the owner, decide whether to stop or relocate it, and start a **new** authorization attempt.
- With the Compose override, Docker reports the conflict while creating or starting the container because it reserves the host mappings up front. Resolve the conflict deliberately, then rerun the same Compose command.

Do not redirect a fixed callback to a different port. An old or expired attempt cannot be repaired by changing its URL.

## Connect, create, test, and activate

A native account and a model profile are separate objects. Connecting an account does not create, activate, or silently replace a profile.

1. Sign in to ARTEX and open **System → LLM → Accounts**.
2. Under **Claude account** or **ChatGPT Codex account**, choose **Connect new account**.
3. Choose **Open authorization page**, finish the provider flow, and return to ARTEX. A reachable loopback callback completes automatically; otherwise use the manual or SSH procedure below.
4. Confirm that the account appears under **Connected accounts**. **Session stored** is not a model-access or quota check.
5. Open **Model profiles**, choose **Create**, set **Authentication** to **Experimental native account**, and select the connected account explicitly.
6. Enter a name. Use the model refresh button to load the pinned candidates and select a model. The provider fixes the format (`anthropic` or `openai-responses`), endpoint, and User-Agent.
7. Choose **Create**. New native profiles are not activated automatically and default to exclusion from automatic pool failover.
8. Reopen the saved profile and choose **Test connection**. This sends a real minimal model request through the saved profile. It can consume account allowance and is not a quota or billing report.
9. After a successful test, return to the profile card and choose **Set as active** if it should become the global default. Existing task chains, agent/role bindings, or conversation overrides can still select another profile; update those explicitly where required.

A selected subscription failure does not silently fall back to another account or a paid API key. Keep API-key and Ollama profiles as explicit alternatives and select them deliberately.

### Reauthentication and account changes

Use **Reauthenticate** on the existing connected-account row to refresh that exact stored identity. ARTEX rejects an account or organization change during in-place reauthentication. To use a different account, choose **Connect new account**, then edit the model profile and bind the new credential explicitly.

Changing a profile's credential increments its binding generation. Stale provider instances are rejected on their next request. Provider replay state is scoped to the provider, model, and stable account authorization scope; it is not replayed after a provider, model, or account scope change. A normal token refresh for the same confirmed account does not create a new account identity.

## Task recovery after restart

Before every task-scoped native model dispatch, ARTEX durably marks that task in PostgreSQL. During a healthy process this marker is not a live hold: normal tool-result continuation, stored opaque provider state, compaction, and subsequent model calls continue unchanged.

On restart, ARTEX conservatively pauses every unfinished task that made a native request since its last explicit recovery acknowledgement. It does this even when the provider may have completed the last request, because ARTEX cannot prove whether the response and its usage were fully observed before the process stopped. The periodic abandoned-claim sweep also parks a task when an expired execution claim had used a native provider.

While the recovery hold is present, ARTEX makes no automatic model request for that task:

- startup restoration and lease recovery do not restart it;
- FIFO admission, internal resume/revive paths, profile changes, and account changes do not clear the hold;
- timeout settlement does not run its final planner request;
- an interrupted side question is not replayed automatically.

The task is paused and receives a system-error activity explaining that the previous native outcome is unknown. Stopping or ending the task without resuming it may leave it terminal; it does not authorize another request.

Only the task's explicit **Resume** action acknowledges the unknown outcome. That admission atomically clears `native_used` and `native_recovery_hold`, reopens only intents blocked for `native_outcome_unknown`, and then allows new requests. If the admission fails, the hold remains. Review provider-side activity or usage first when that information matters.

This deliberate false-positive pause avoids an unobserved duplicate, quota-consuming call. It is not a per-request response ledger and does not claim to determine whether the provider completed the prior request. It adds no broker, CLI, or alternate replay runtime.

## Remote authorization

A provider redirects the browser to the browser machine's `localhost`, not automatically to the ARTEX host. Do not expose callback ports on a public or LAN interface to compensate.

### SSH local forwarding

When ARTEX and its callback ports are available on the remote host's loopback, forward the UI and both fixed callbacks:

```bash
ssh -N \
  -L 8787:127.0.0.1:8787 \
  -L 54545:127.0.0.1:54545 \
  -L 1455:127.0.0.1:1455 \
  user@artex-host
```

Open `http://127.0.0.1:8787`, sign in, and start the account connection there. OpenSSH binds local forwards to loopback by default. The remote target is the remote host's IPv4 loopback; the provider-supplied browser URL still uses the exact `localhost` callback contract. Do not edit that final URL to use `127.0.0.1`.

For Docker, use the opt-in Compose override first so the remote host's published loopback ports can reach a listener on the container interface. Merely adding `ports` to a container whose process still listens on container `127.0.0.1` is insufficient.

### Manual final redirect

If the browser cannot reach the callback, it normally ends on a failed page whose address bar contains the complete final redirect URL.

1. Start **Connect new account** from an authenticated ARTEX session.
2. Complete provider authorization before the displayed expiration time (attempts expire after ten minutes).
3. Copy the entire final URL from the address bar, preserving `http://localhost`, the fixed port and path, `state`, `code`, and all other query parameters.
4. In the same initiating ARTEX session, paste it into **Manual final redirect URL** and choose **Submit final redirect**.

Do not submit a bare authorization code or token. Do not log, bookmark, message, or save the final URL; it contains a short-lived authorization code. A different ARTEX login token, even for the same administrator, does not own the attempt. The backend verifies the initiating session, attempt ID, exact callback, state, PKCE verifier, expiry, and account identity before storing anything.

## Disconnect and remote access

Choose **Disconnect** on the connected-account row to revoke local use. ARTEX immediately marks the credential disconnected, clears locally stored token material, cancels that credential's in-flight local model calls, and prevents new calls. Profiles retain the explicit credential reference and show a disconnected state rather than falling back.

The current Claude and Codex drivers do not provide confirmed remote token revocation. The UI therefore reports **Disconnected locally; remote revocation unconfirmed**. This does not guarantee that the provider invalidated the token, stopped a request already accepted upstream, reversed usage, or canceled charges. Visit the provider's account security/authorized-apps portal to end remote sessions or access when revocation is unsupported or unconfirmed.

## Management API

All endpoints below require a valid ARTEX JWT (`Authorization: Bearer …`) and are covered by the same administrator boundary as the rest of `/api`. ARTEX does not currently add per-credential user roles: any valid ARTEX administrator session can list, disconnect, and bind credentials. Start, poll, and manual submit are additionally bound to the exact JWT that initiated that authorization attempt.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/llm/credentials` | List safe credential metadata: ID, provider, label, status, and timestamps |
| `POST` | `/api/llm/credentials/login` | Start a new login with `provider`, or reauthenticate with `provider` plus `credential_id` |
| `GET` | `/api/llm/credentials/attempt/{id}` | Poll the initiating session's pending attempt |
| `POST` | `/api/llm/credentials/attempt/{id}/submit` | Submit the complete final URL in `redirect_url` |
| `POST` | `/api/llm/credentials/{id}/disconnect` | Remove local access and report whether remote revocation was confirmed |
| `GET` | `/api/llm/profiles` | List model profiles and safe credential bindings |
| `POST` | `/api/llm/profiles` | Create or update a profile; native profiles use `auth_mode: "oauth"` and `credential_id` |
| `POST` | `/api/llm/models` | Return pinned native catalog candidates with `availability: "unknown"` |
| `POST` | `/api/llm/test` | Make a real minimal connection test through the selected profile |
| `POST` | `/api/llm/profiles/active` | Explicitly make a saved profile active |

Credential responses never contain access tokens, refresh tokens, ID tokens, PKCE verifiers, or the encrypted database payload. The pending authorization URL contains attempt state, and a manually submitted redirect contains a short-lived code; handle both as sensitive transient data. The API never accepts a raw provider access or refresh token.

Provider authentication failures use subscription-specific error codes and do not become an ARTEX HTTP 401. An HTTP 401 from these management paths means the ARTEX JWT itself is absent or invalid.

## Backup and restore

A usable native-credential backup is a matched set:

1. the PostgreSQL dump, including `llm_credentials`, `llm_profiles`, bindings, the ARTEX password hash, and the task `native_used` / `native_recovery_hold` recovery markers added by `0009_native_task_recovery.sql`;
2. the exact secret-encryption key that sealed that database: either `keys/secret.key` or the deployment's `ARTEX_SECRET_KEY` value;
3. the JWT signing key at `keys/jwt.key` and the same `ARTEX_JWT_KEY_DIR` layout/settings;
4. optionally, the runtime `data/` tree if the rest of the ARTEX state is required.

`scripts/backup.sh` creates the PostgreSQL dump. Its `--with-data` option archives `data/`; it does **not** include `keys/`. Back up `secret.key` and `jwt.key` separately with restrictive permissions, or preserve the corresponding managed secret values. Never print either key or put it in logs, tickets, shell history, or this repository.

Restore into a clean, compatible PostgreSQL database using the command documented by `scripts/backup.sh`, then restore the matching keys and environment settings **before** starting ARTEX. In Docker, the repository's `./keys` directory is mounted at `/app/keys`. If `ARTEX_SECRET_KEY` was used, restore that same secret through the secret manager rather than generating a file with a different value.

ARTEX refuses to generate a new secret key over encrypted rows and refuses to start when the supplied key cannot decrypt them. Restoring the database without its matching secret key cannot recover native sessions. Restoring a different `jwt.key` invalidates existing ARTEX login sessions; restore the matched JWT key instead. If provider-side sessions have expired or been revoked since the backup, use **Reauthenticate** after the local restore.

No `auth.json`, broker state, npm directory, or native CLI installation belongs in this backup.

## Database upgrade paths

Migration versions are append-only and validated by filename and SHA-256 checksum. Use exactly one of these paths:

- **Fresh database:** start the new binary normally. It applies migrations `0001` through `0009` in order.
- **Standard upstream database:** if `schema_migrations` already records upstream `0006_activity_outbox.sql` and `0007_finding_report_state.sql`, start the new binary normally. It applies native migrations `0008_llm_oauth_credentials.sql` and `0009_native_task_recovery.sql`. Do not run the maintenance procedure below.
- **Early experimental native database:** a binary built from native commit `bf23bce26baad3c62b88e10bca8edeb6b4a747ca` recorded the native migrations under versions `0006` and `0007`, before upstream assigned those versions. Follow the offline procedure below once.

Do not infer the third case from the presence of native tables. Inspect the ledger:

```sql
SELECT version, name, checksum, applied_at
FROM schema_migrations
ORDER BY version;
```

The early-native procedure applies **only** when versions 6 and 7 are exactly:

| Version | Name | SHA-256 |
|---|---|---|
| 6 | `0006_llm_oauth_credentials.sql` | `e3aa3abecde97b42924aa96adb0da44329bbb33e5b7f0e1b4339b3660092e71c` |
| 7 | `0007_native_task_recovery.sql` | `ad9d901f673b765cf796d4185e9dfa5af65f3cb1da99c369be837e996111f1ad` |

If either row differs, stop. In particular, upstream names at versions 6 and 7 identify the standard path, not an early-native database. Never edit the expected values to make an unfamiliar ledger pass.

### One-time early-native ledger transition

1. Stop every ARTEX application instance while leaving PostgreSQL available. For the opt-in Compose deployment:

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.oauth.yml stop artex
   ```

2. Create and validate a database backup. Compose deployments can use `scripts/backup.sh --with-data`; host-native deployments must take an equivalent PostgreSQL dump and preserve any required runtime data. Separately preserve `keys/secret.key` (or the managed `ARTEX_SECRET_KEY`) and `keys/jwt.key`. Do not proceed without a restorable backup.
3. From the new checkout, verify that all four migration files are the exact release inputs:

   ```text
   $ shasum -a 256 db/migrations/0006_activity_outbox.sql db/migrations/0007_finding_report_state.sql db/migrations/0008_llm_oauth_credentials.sql db/migrations/0009_native_task_recovery.sql
   a320b7788c22a8acb8493885a69c06101b826649c5ad1bcce4f1fbda58283687  db/migrations/0006_activity_outbox.sql
   f224b62872f88e0182edb8153051b6016dec82492cc17bda229d768450724686  db/migrations/0007_finding_report_state.sql
   e3aa3abecde97b42924aa96adb0da44329bbb33e5b7f0e1b4339b3660092e71c  db/migrations/0008_llm_oauth_credentials.sql
   ad9d901f673b765cf796d4185e9dfa5af65f3cb1da99c369be837e996111f1ad  db/migrations/0009_native_task_recovery.sql
   ```

   Any mismatch means this procedure does not match the target binary; stop and use that release's instructions.
4. Run the following SQL in one `psql` session. For Compose, use:

   ```bash
   docker compose exec -T postgres sh -c \
     'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
   \set ON_ERROR_STOP on
   SELECT pg_advisory_lock(7337741001);
   BEGIN;
   LOCK TABLE schema_migrations IN EXCLUSIVE MODE;

   DO $guard$
   DECLARE
       ledger_versions INTEGER[];
       changed INTEGER;
   BEGIN
       SELECT array_agg(version ORDER BY version)
         INTO ledger_versions
         FROM schema_migrations;
       IF ledger_versions IS DISTINCT FROM ARRAY[1,2,3,4,5,6,7]::INTEGER[] THEN
           RAISE EXCEPTION 'expected exactly migration versions 1..7, found %', ledger_versions;
       END IF;

       IF NOT EXISTS (
           SELECT 1 FROM schema_migrations
           WHERE version=6
             AND name='0006_llm_oauth_credentials.sql'
             AND checksum='e3aa3abecde97b42924aa96adb0da44329bbb33e5b7f0e1b4339b3660092e71c'
       ) THEN
           RAISE EXCEPTION 'migration 6 is not the exact early-native credential migration';
       END IF;
       IF NOT EXISTS (
           SELECT 1 FROM schema_migrations
           WHERE version=7
             AND name='0007_native_task_recovery.sql'
             AND checksum='ad9d901f673b765cf796d4185e9dfa5af65f3cb1da99c369be837e996111f1ad'
       ) THEN
           RAISE EXCEPTION 'migration 7 is not the exact early-native recovery migration';
       END IF;
       IF EXISTS (SELECT 1 FROM schema_migrations WHERE version IN (8,9)) THEN
           RAISE EXCEPTION 'migration destination version 8 or 9 is already occupied';
       END IF;

       IF to_regclass('llm_credentials') IS NULL
          OR NOT EXISTS (
              SELECT 1 FROM information_schema.columns
              WHERE table_schema=current_schema() AND table_name='llm_profiles'
                AND column_name='auth_mode'
          )
          OR NOT EXISTS (
              SELECT 1 FROM information_schema.columns
              WHERE table_schema=current_schema() AND table_name='tasks'
                AND column_name='native_used'
          )
          OR NOT EXISTS (
              SELECT 1 FROM information_schema.columns
              WHERE table_schema=current_schema() AND table_name='tasks'
                AND column_name='native_recovery_hold'
          ) THEN
           RAISE EXCEPTION 'early-native schema objects are incomplete; restore or investigate instead of rewriting the ledger';
       END IF;

       IF to_regclass('activity_outbox') IS NOT NULL
          OR EXISTS (
              SELECT 1 FROM information_schema.columns
              WHERE table_schema=current_schema() AND table_name='findings'
                AND column_name='report_state'
          ) THEN
           RAISE EXCEPTION 'canonical upstream 6/7 schema already exists; restore or investigate instead of adopting it';
       END IF;

       UPDATE schema_migrations
          SET version=8, name='0008_llm_oauth_credentials.sql'
        WHERE version=6
          AND name='0006_llm_oauth_credentials.sql'
          AND checksum='e3aa3abecde97b42924aa96adb0da44329bbb33e5b7f0e1b4339b3660092e71c';
       GET DIAGNOSTICS changed = ROW_COUNT;
       IF changed <> 1 THEN
           RAISE EXCEPTION 'expected to move exactly one native credential ledger row, moved %', changed;
       END IF;

       UPDATE schema_migrations
          SET version=9, name='0009_native_task_recovery.sql'
        WHERE version=7
          AND name='0007_native_task_recovery.sql'
          AND checksum='ad9d901f673b765cf796d4185e9dfa5af65f3cb1da99c369be837e996111f1ad';
       GET DIAGNOSTICS changed = ROW_COUNT;
       IF changed <> 1 THEN
           RAISE EXCEPTION 'expected to move exactly one native recovery ledger row, moved %', changed;
       END IF;
   END
   $guard$;

   CREATE TABLE IF NOT EXISTS activity_outbox (
       id BIGSERIAL PRIMARY KEY,
       exploration_id BIGINT NOT NULL,
       payload JSONB NOT NULL,
       last_error TEXT,
       attempts INT NOT NULL DEFAULT 0,
       created_at TIMESTAMPTZ NOT NULL DEFAULT now()
   );
   CREATE INDEX IF NOT EXISTS activity_outbox_exploration_idx
       ON activity_outbox (exploration_id, id);

   ALTER TABLE findings
       ADD COLUMN IF NOT EXISTS report_state TEXT NOT NULL DEFAULT 'none';
   CREATE INDEX IF NOT EXISTS idx_findings_report_state
       ON findings (report_state, created_at DESC);

   INSERT INTO schema_migrations(version, name, checksum)
   VALUES
       (6, '0006_activity_outbox.sql',
        'a320b7788c22a8acb8493885a69c06101b826649c5ad1bcce4f1fbda58283687'),
       (7, '0007_finding_report_state.sql',
        'f224b62872f88e0182edb8153051b6016dec82492cc17bda229d768450724686');

   DO $verify$
   DECLARE
       ledger_versions INTEGER[];
   BEGIN
       SELECT array_agg(version ORDER BY version)
         INTO ledger_versions
         FROM schema_migrations;
       IF ledger_versions IS DISTINCT FROM ARRAY[1,2,3,4,5,6,7,8,9]::INTEGER[] THEN
           RAISE EXCEPTION 'transition did not produce migration versions 1..9: %', ledger_versions;
       END IF;
       IF NOT EXISTS (
           SELECT 1 FROM schema_migrations
           WHERE version=8
             AND name='0008_llm_oauth_credentials.sql'
             AND checksum='e3aa3abecde97b42924aa96adb0da44329bbb33e5b7f0e1b4339b3660092e71c'
       ) OR NOT EXISTS (
           SELECT 1 FROM schema_migrations
           WHERE version=9
             AND name='0009_native_task_recovery.sql'
             AND checksum='ad9d901f673b765cf796d4185e9dfa5af65f3cb1da99c369be837e996111f1ad'
       ) THEN
           RAISE EXCEPTION 'native migration rows were not preserved at versions 8 and 9';
       END IF;
   END
   $verify$;

   COMMIT;
   SELECT pg_advisory_unlock(7337741001);
   SQL
   ```

   For a host-native PostgreSQL connection, feed the same SQL body to `psql -X -v ON_ERROR_STOP=1 "$ARTEX_PG_DSN"` instead. The advisory lock is the same lock used by ARTEX startup. `ON_ERROR_STOP` and the enclosing transaction make every guard fail closed: a wrong filename, checksum, version set, occupied destination, or unexpected schema aborts without changing the ledger or schema. The terminated `psql` session also releases the advisory lock. Do not bypass a failure or manually commit an aborted transaction.
5. Start the new binary. It validates the complete `0001`–`0009` ledger before serving. The transition changes only migration versions/names for the two native rows; their checksums and `applied_at` values are preserved. It does not update `llm_credentials`, profile bindings, encrypted payloads, or either native task flag. Confirm that connected accounts and held tasks match the pre-upgrade deployment before resuming work.

Do not run the new binary against the early-native ledger before this procedure. Startup applies the current schema baseline before validating migration history, so a failed attempt can create the canonical objects and intentionally cause the preflight guard above to refuse automatic adoption. Restore the backup before retrying.

## Rollback to a native-unaware ARTEX version

Prepare the routing while the current binary still understands native profiles:

1. Stop or pause running tasks, conversations, agents, and scheduled/automatic work that may issue a model call.
2. Create or select an explicit API-key or Ollama profile and test it.
3. Make that non-native profile active. Remove native profiles from every agent/role binding, task profile chain and active task cursor, conversation override, judge/auxiliary binding, and pool target, or replace each binding with the tested non-native profile. Turning pool rotation off is not a substitute for clearing explicit bindings.
4. Confirm that new work resolves to the non-native profile, then stop ARTEX cleanly. For the opt-in container deployment, use the same two Compose files so the two-minute grace period remains in effect.
5. Restore a backup whose migration ledger is compatible with the target older binary, together with that backup's matching encryption and JWT keys, before starting it. The current native-aware ledger contains versions the older binary does not ship, so an image-only downgrade is rejected even after the routing cutover. State recorded after the restored backup is not preserved by this rollback.

Do not downgrade first. Older code that does not understand `auth_mode`, credential bindings, or native terminal errors could treat the profile as unusable and fall through to a global/environment API key or another profile.

Do not delete migration ledger rows or drop credential tables to make an incompatible database appear compatible. Keep the native-aware database backup, its recovery markers and encrypted credential records, and its matching `secret.key`/`ARTEX_SECRET_KEY` for a later return to the native-aware version. Preserve its `jwt.key` as well. Disconnect an account only when local revocation is actually desired, not as a schema-cleanup step.
