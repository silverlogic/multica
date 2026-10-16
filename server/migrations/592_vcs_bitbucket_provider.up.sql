-- Add Bitbucket Cloud as a VCS provider. NOT VALID keeps the swap to a brief
-- catalog lock (no table scan); every existing row already satisfies the
-- widened list.
ALTER TABLE vcs_connection DROP CONSTRAINT IF EXISTS vcs_connection_provider_check;
ALTER TABLE vcs_connection ADD CONSTRAINT vcs_connection_provider_check
    CHECK (provider IN ('forgejo', 'gitea', 'gitlab', 'bitbucket')) NOT VALID;

ALTER TABLE vcs_pull_request DROP CONSTRAINT IF EXISTS vcs_pull_request_provider_check;
ALTER TABLE vcs_pull_request ADD CONSTRAINT vcs_pull_request_provider_check
    CHECK (provider IN ('forgejo', 'gitea', 'gitlab', 'bitbucket')) NOT VALID;
