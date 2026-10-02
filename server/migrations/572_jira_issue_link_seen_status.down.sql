ALTER TABLE jira_issue_link
    DROP COLUMN IF EXISTS jira_status,
    DROP COLUMN IF EXISTS jira_priority;
