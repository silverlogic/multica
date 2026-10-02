-- Imported Jira issues carry their Jira key and browse URL in two workspace
-- custom properties ("Jira key" / "Jira link"). The connection remembers
-- which definitions it fills, so renaming a property in the UI keeps it
-- attached; NULL means not yet resolved (looked up by name, else created,
-- on the next import).
--
-- Nullable UUIDs, no FK, no index (only read through the connection row),
-- so this is a safe metadata-only ALTER.
ALTER TABLE jira_connection
    ADD COLUMN IF NOT EXISTS key_property_id UUID,
    ADD COLUMN IF NOT EXISTS link_property_id UUID;
