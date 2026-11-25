DROP INDEX IF EXISTS uq_users_oidc_identity;
ALTER TABLE users DROP COLUMN oidc_subject;
ALTER TABLE users DROP COLUMN oidc_issuer;
