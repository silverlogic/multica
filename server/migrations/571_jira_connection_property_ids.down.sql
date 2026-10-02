ALTER TABLE jira_connection
    DROP COLUMN IF EXISTS key_property_id,
    DROP COLUMN IF EXISTS link_property_id;
