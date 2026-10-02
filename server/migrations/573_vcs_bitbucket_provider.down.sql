-- Restore the pre-573 provider list. Existing Bitbucket rows stay readable
-- because the replacement constraints are NOT VALID; new ones are blocked.
ALTER TABLE vcs_connection DROP CONSTRAINT IF EXISTS vcs_connection_provider_check;
ALTER TABLE vcs_connection ADD CONSTRAINT vcs_connection_provider_check
    CHECK (provider IN ('forgejo', 'gitea', 'gitlab')) NOT VALID;

ALTER TABLE vcs_pull_request DROP CONSTRAINT IF EXISTS vcs_pull_request_provider_check;
ALTER TABLE vcs_pull_request ADD CONSTRAINT vcs_pull_request_provider_check
    CHECK (provider IN ('forgejo', 'gitea', 'gitlab')) NOT VALID;
