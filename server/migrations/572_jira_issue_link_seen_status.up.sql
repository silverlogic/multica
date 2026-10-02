-- The Jira status and priority names each link last saw. Inbound sync moves
-- the Multica issue's status/priority only when Jira's value changes, so an
-- edit made in Multica (by a person or an agent) is not reverted by every
-- sync of an issue Jira has not touched. NULL means not seen yet: the next
-- sync applies Jira's current value once.
--
-- Nullable TEXT, no FK, no index (only read through the link row), so this
-- is a safe metadata-only ALTER.
ALTER TABLE jira_issue_link
    ADD COLUMN IF NOT EXISTS jira_status TEXT,
    ADD COLUMN IF NOT EXISTS jira_priority TEXT;
