-- Migration: 000137_users_oidc_identity
-- Bind a local account to exactly one external OIDC identity. OIDC login
-- resolves (issuer, subject) before falling back to email; an email-only
-- match is accepted only when the provider marks the address verified, so
-- an unverified email at the IdP can no longer take over a local account.
-- Both columns are nullable: password accounts and accounts created before
-- this migration keep no binding.
DO $$ BEGIN RAISE NOTICE '[Migration 000137] Adding users.oidc_issuer / users.oidc_subject'; END $$;

ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_issuer VARCHAR(255);
ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_subject VARCHAR(255);

-- Soft-deleted rows must not pin an external identity forever: if the
-- account is deleted, the same (issuer, subject) may bind a new account.
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_oidc_identity
    ON users (oidc_issuer, oidc_subject)
    WHERE oidc_issuer IS NOT NULL AND oidc_subject IS NOT NULL AND deleted_at IS NULL;

COMMENT ON COLUMN users.oidc_issuer IS 'OIDC issuer this account is bound to; NULL for local-only accounts';
COMMENT ON COLUMN users.oidc_subject IS 'OIDC subject (sub) this account is bound to; NULL for local-only accounts';
