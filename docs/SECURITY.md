# Security — deploy checklist, key rotation, and incident record

This file exists so the security work done across four hardening phases
(2026-08) stays load-bearing instead of turning into tribal knowledge. Read
it before every production deploy and before rotating any secret.

## Before every production deploy

- [ ] `.env` has real, production-appropriate values for every variable in
      `.env.example` — especially:
  - `APP_ENV=production` — without this, `CLAMAV_ADDR` isn't required at
    boot (see below) and the `TRUSTED_PROXY_IPS` warning won't fire.
  - `APP_BASE_URL=https://...` — the auth cookie's `Secure` flag is derived
    from this, not from the request (see `config.CookieSecure`'s doc
    comment). Get this wrong and the session cookie ships without
    `Secure` on production HTTPS.
  - `TRUSTED_PROXY_IPS` — set to the IP/CIDR of whatever sits directly in
    front of this process (the Next.js server, in the current architecture).
    Without it, `c.IP()` returns the proxy's address for every request,
    which silently defeats the per-IP login rate limiter and mislabels
    every audit-log IP. The server still boots and runs — this fails
    quietly, not loudly, so it has to be checked by hand.
  - `CORS_ORIGINS` — the real frontend origin(s), comma-separated. This
    doubles as the allow-list for `handler.OriginCheck`'s CSRF
    defense-in-depth, so getting it wrong doesn't just break CORS, it also
    makes every mutating request from the real frontend fail closed.
  - `CLAMAV_ADDR` — required when `APP_ENV=production` (the server refuses
    to start otherwise). Confirm the `clamav` container in
    `docker-compose.yml` is actually healthy, not just running —
    `clamdcheck.sh` takes a few minutes on a cold volume, and uploads
    fail-closed until it passes.
  - `TOTP_ENC_KEY` — required unconditionally (like `PII_ENC_KEY`; the
    server refuses to boot without it). Encrypts every TOTP secret at rest
    — see "Two-factor authentication (2FA)" below.
- [ ] `docker compose up -d` has been run since the last `.env` change at
      the repo root, so Postgres's port binding (`127.0.0.1:5432`, not
      `0.0.0.0:5432`) actually took effect on the running container.
- [ ] Full test suite green: `go test ./... -timeout 20m` — the default
      10-minute `go test` timeout is too short for this suite (see
      `internal/service`'s throwaway-DB-per-test design), so a bare
      `go test ./...` can report a false `FAIL` on nothing but a slow
      machine. Always pass `-timeout 20m` (or more) before trusting a
      failure.
- [ ] CI green on the actual PR/branch being deployed — `govulncheck` and
      `gosec` (backend), `npm audit` (frontend, report-only — see that
      workflow's comment for why) all run automatically; don't deploy past
      a red CI run to "fix it after."

## Key rotation

Three secrets in `.env` are encryption/signing keys with a real rotation
procedure — get the order wrong and you make live data unreadable, not just
"less secure."

### `JWT_SECRET`
No data migration needed — it only signs tokens, nothing is encrypted with
it. Generate a new value (`openssl rand -base64 48`), update `.env`, restart
the service. Every existing session is invalidated immediately (this is the
whole point if the rotation is incident-driven) — everyone has to log back
in.

### `PII_ENC_KEY` (encrypts `ta_profiles.citizen_id_enc`)
Has exactly one key at a time (`internal/pii.Cipher` holds one AEAD, full
stop) — swapping the env var without migrating existing rows makes every
previously-stored citizen ID permanently undecryptable the next time
`RevealCitizenID` is called (i.e., the next time a transfer-cover document
is generated), with no error until an officer notices a blank field.

Use `cmd/rotate-pii-key`:
```
# 1. Dry run — decrypts every row with the OLD key, writes nothing.
OLD_PII_ENC_KEY=... NEW_PII_ENC_KEY=... DATABASE_URL=... \
  go run ./cmd/rotate-pii-key

# 2. Apply — one transaction, all rows or none.
OLD_PII_ENC_KEY=... NEW_PII_ENC_KEY=... DATABASE_URL=... \
  go run ./cmd/rotate-pii-key -apply -new-version=N
#   N must be higher than every citizen_id_key_version already on the
#   table — the tool checks this and refuses otherwise.

# 3. Only after the tool reports every row done: update PII_ENC_KEY in the
#    running service's environment and restart it.
```
If there are zero rows with `citizen_id_enc` set (check first — `SELECT
count(*) FROM ta_profiles WHERE citizen_id_enc IS NOT NULL`), there is
nothing to migrate; just swap the key directly.

### `TA_DOCS_ENC_KEY` (encrypts uploaded TA documents on disk)
Same one-key-at-a-time limitation as PII_ENC_KEY, but the blast radius is a
directory tree instead of a few database rows, and it keeps growing — see
`cmd/rotate-docs-key`'s package doc comment for the full usage, safety
model (per-file atomic replace, verified before it overwrites the
original), and — importantly — how to resume a run that stopped partway
through (re-running is safe; a file already rotated just fails to decrypt
under the OLD key on the next pass, which is the correct signal, not
corruption). **Back up `UPLOAD_DIR` before running `-apply`** — the tool is
tested, but this is real, hard-to-replace TA data and the backup costs
almost nothing next to what re-uploading hundreds of documents would cost.

### `TOTP_ENC_KEY` (encrypts `users.totp_secret_enc` / `totp_pending_secret_enc`)
Same one-key-at-a-time limitation as `PII_ENC_KEY` — swapping this without
migrating existing rows makes every enrolled user's stored TOTP secret
permanently undecryptable, which means their authenticator app can no longer
be validated against and they are locked out at the next login. **There is
no `cmd/rotate-totp-key` tool yet** (see "Known accepted risks" below) — do
not rotate this key on a running system with any `totp_enabled_at IS NOT
NULL` rows without first either building that tool (mirror
`cmd/rotate-pii-key`'s dry-run/apply/version-check shape exactly) or accepting
that every 2FA-enrolled user will need `cmd/reset-2fa` run against their
account and will have to re-enrol.

### Secrets this project can't rotate by itself
- `SMTP_PASS`, `BOT_API_CLIENT_ID`, `SSO_CLIENT_SECRET` (if SSO is ever
  wired up) are credentials issued by external systems (KKU mail/relay,
  Bank of Thailand's API portal, KKU IT's SSO provider). Rotating these
  means logging into that external system and generating a new value
  there first — update `.env` afterward.
- Any user's own account password (including the seeded admin) — if you
  suspect one is compromised, use the existing `POST
  /users/:id/reset-password` (admin/staff) flow, which forces a
  `must_change_password` on next login. Don't hand-edit `password_hash`.

## Two-factor authentication (2FA)

TOTP-based (RFC 6238), added 2026-08-16. Login is two steps when 2FA is
enabled — see `internal/handler/auth.go`'s `AuthHandler.Login` /
`LoginTwoFactor` and `internal/handler/middleware.go`'s `AccountGuard`.

**Policy**: mandatory for admin, staff, and any account with
`is_executive=true`; optional (opt-in from `/account`) for lecturer and TA.
Enforced by `AccountGuard`'s `mfa_setup_required` branch — every endpoint
except `/me`, `/me/2fa/*`, and `/auth/heartbeat` 403s for a mandatory-tier
account with no TOTP enrolled.

**Recovery, in order of preference**:
1. One of the 10 recovery codes issued at enrolment (`POST
   /auth/login/2fa` accepts either a 6-digit TOTP or a recovery code).
2. An admin-initiated reset: `POST /users/:id/2fa/reset`, **admin-only**
   (not staff — see `MFAService.AdminReset`'s doc comment for why:
   staff already hold unrestricted password reset, and adding
   unrestricted 2FA reset on top would chain into a one-click admin
   takeover), gated behind the acting admin's own password, refuses
   targeting self.
3. **Break-glass** — `cmd/reset-2fa`, for when there is no admin left with
   access (the last admin lost their phone and their recovery codes, so
   nobody can call #2 on their behalf):
   ```
   # Dry run first — reports current state, writes nothing.
   DATABASE_URL=... go run ./cmd/reset-2fa -email=admin@example.com

   # Apply — clears the account's TOTP secret and every recovery code.
   DATABASE_URL=... go run ./cmd/reset-2fa -email=admin@example.com -apply
   ```
   Deliberately bypasses the API, the audit log, and every in-app
   authorization check — by the time this is needed, none of those are
   reachable. Whoever holds `DATABASE_URL` already holds the keys to the
   whole system regardless. The account is left free to log in with just
   its password and gets routed to `/setup-2fa` to re-enrol, same as any
   other mandatory-tier account with no TOTP on file.

**Demo sandbox**: mandatory 2FA is skipped entirely inside every demo slot
(`config.Config.IsDemoSlot`, set only by `internal/demo.Bootstrap` on each
slot's own Config copy). This is deliberately **not** the same flag as
`DemoMode` — see `IsDemoSlot`'s doc comment: `DemoMode` is also `true` on
the real production `Config` whenever the sandbox feature is switched on at
all, so branching the mandatory-2FA check on it would have silently
disabled 2FA enforcement for every real admin/staff account. If a future
change ever needs to check "is this a demo request", use `IsDemoSlot`, not
`DemoMode`.

## SSO (currently a stub)

`AuthHandler.SSOCallback` returns 501 — there is no live OAuth/OIDC flow to
secure yet. **Before implementing it**:
- it needs `state` (CSRF) and PKCE, not just an authorization-code exchange
  — add this as part of the implementation, not as a follow-up hardening
  pass afterward.
- it must branch on `TOTPEnabled` and issue an MFA challenge exactly like
  `Login` does, not go straight to `CreateAndSupersede` + `Issue` +
  `setAuthCookie`. SSO is a second, independent path to a session — nothing
  about a KKU SSO assertion proves possession of the account's TOTP
  secret, and skipping the 2FA branch "because SSO already authenticated
  them" would let a mandatory-tier account bypass 2FA entirely through
  this path.

## Known accepted risks (revisit periodically, not blocking)

- **Frontend `npm audit`** — `postcss`/`sharp` (both nested under `next`)
  carry high-severity advisories with no fix except bumping `next` past the
  pinned `16.2.12` to `16.3.1`+. Not done as a side effect of wiring up the
  audit step — `ta_payment_front/AGENTS.md` warns this version line can
  carry real breaking changes, so the bump needs its own dedicated test
  pass across the app. The CI step reports this without failing the build
  (see that workflow's comment) — re-tighten it once the bump happens.
- **`backup/*.sql`** — gitignored, but if a full DB dump is ever placed in
  `backup/` again for a one-off task, treat it as sensitive as the database
  itself (it likely contains password hashes and, if PII rows exist by
  then, ciphertext that's only as safe as `PII_ENC_KEY`). Delete it from
  disk once its purpose is done rather than leaving it to accumulate.
- **No `cmd/rotate-totp-key` tool** — `TOTP_ENC_KEY` has the same
  one-key-at-a-time limitation as `PII_ENC_KEY` and `TA_DOCS_ENC_KEY`, but
  unlike those two there is no rotation tool yet (see "`TOTP_ENC_KEY`"
  above). Low urgency while enrolment is new and the user count is small;
  build it (mirroring `cmd/rotate-pii-key`) before it isn't.

## Incident record

**2026-08-14 — secrets found committed to `ta_payment_back`'s git history.**
`.env` (real values: `JWT_SECRET`, DB password, `TA_DOCS_ENC_KEY`,
`PII_ENC_KEY`, `BOT_API_CLIENT_ID`, seed admin password) and
`backup/ta_payment_before_wipe_20260724.sql` (a full DB dump) were committed
between the repo's first commit (2026-07-09) and their removal
(2026-07-31), and confirmed present on `origin/main`. Removing them from the
tip does not remove them from history — anyone with read access to the repo
could still recover the original values from the commits that added them.

Response:
1. Every secret above was rotated (see "Key rotation" section) —
   `TA_DOCS_ENC_KEY` and `PII_ENC_KEY` rotated for real against the live
   data (16 documents re-encrypted and verified; 0 `ta_profiles` rows had a
   citizen ID on file at the time, so nothing needed migrating there),
   `JWT_SECRET` and the database password regenerated and applied.
   `BOT_API_CLIENT_ID` and the seeded admin account's password are external
   credentials this codebase cannot rotate on its own — **still need action
   from whoever administers those** (BOT's API portal; and either changing
   the admin's password normally or forcing a reset via `POST
   /users/:id/reset-password`).
2. Git history rewrite (`git filter-repo` + force-push to `origin/main`) to
   remove `.env` and `backup/` from every commit — this is destructive to
   shared history and needs coordination with anyone holding a clone, so it
   is tracked as a separate, explicit step rather than folded silently into
   this file. Check this repo's actual remote history (`git log --all
   --oneline -- .env backup/`) to confirm whether that rewrite has
   happened before assuming the exposure is fully closed — rotating the
   secrets closes the *live* exposure immediately, but the old
   (now-invalid) values and the DB dump remain recoverable from history
   until the rewrite runs.
3. A proper `.gitignore` (`.env`, `/data/`, `/backup/`) already existed in
   this repo before this incident — it just didn't retroactively protect
   what was committed before it was added. Keeping secrets out of the repo
   going forward was never the gap; this file's checklist is the actual
   fix, since a `.gitignore` can't stop a `git add -A` run before it
   existed.
