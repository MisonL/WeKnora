-- Bind a local account to exactly one external OIDC identity (parity with
-- versioned migration 000137). Both columns are nullable so local accounts
-- and pre-migration rows stay unbound; the unique index prevents two live
-- accounts from claiming the same external identity.
ALTER TABLE users ADD COLUMN oidc_issuer VARCHAR(255);
ALTER TABLE users ADD COLUMN oidc_subject VARCHAR(255);

CREATE UNIQUE INDEX IF NOT EXISTS uq_users_oidc_identity
    ON users (oidc_issuer, oidc_subject)
    WHERE oidc_issuer IS NOT NULL AND oidc_subject IS NOT NULL AND deleted_at IS NULL;
