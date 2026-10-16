package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/jira"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestJiraStatus_ImportMapsStatusAndPriority verifies new mirrored issues
// start in Jira's status and priority, by name and by category.
func TestJiraStatus_ImportMapsStatusAndPriority(t *testing.T) {
	ctx := context.Background()
	box := withJiraBox(t)
	testHandler.JiraClient = &mockJiraClient{searchResults: []jira.Issue{
		{ID: "1", Key: "MAP-1", Summary: "by name", Status: "In Progress", StatusCategory: "indeterminate", Priority: "High"},
		{ID: "2", Key: "MAP-2", Summary: "by category", Status: "Released", StatusCategory: "done", Priority: "Lowest"},
		{ID: "3", Key: "MAP-3", Summary: "unmapped", Status: "Mystery", Priority: "P1"},
	}}
	conn := seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	t.Cleanup(func() { cleanupJira(ctx) })

	runJiraSync(t, conn)
	for key, want := range map[string][2]string{
		"MAP-1": {"in_progress", "high"},
		"MAP-2": {"done", "low"},
		"MAP-3": {"todo", "none"},
	} {
		issue, _ := mirroredIssue(t, conn, key)
		if issue.Status != want[0] || issue.Priority != want[1] {
			t.Errorf("%s = %s/%s, want %s/%s", key, issue.Status, issue.Priority, want[0], want[1])
		}
	}
}

// TestJiraStatus_OnlyJiraChangesOverwrite verifies a status or priority set
// in Multica survives syncs while Jira's value is unchanged, and that a
// later change in Jira does apply.
func TestJiraStatus_OnlyJiraChangesOverwrite(t *testing.T) {
	ctx := context.Background()
	box := withJiraBox(t)
	mock := &mockJiraClient{searchResults: []jira.Issue{
		{ID: "1", Key: "OWN-1", Summary: "owned", Status: "To Do", StatusCategory: "new", Priority: "Medium"},
	}}
	testHandler.JiraClient = mock
	conn := seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	t.Cleanup(func() { cleanupJira(ctx) })

	runJiraSync(t, conn)
	issue, _ := mirroredIssue(t, conn, "OWN-1")
	moves := subscribeIssueStatusMoves(uuidToString(issue.ID))
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'blocked', priority = 'urgent' WHERE id = $1`, uuidToString(issue.ID)); err != nil {
		t.Fatalf("edit in Multica: %v", err)
	}

	runJiraSync(t, conn)
	issue, _ = mirroredIssue(t, conn, "OWN-1")
	if issue.Status != "blocked" || issue.Priority != "urgent" {
		t.Fatalf("unchanged Jira values overwrote the Multica edit: %s/%s", issue.Status, issue.Priority)
	}

	// Jira moves only the status: status follows, the Multica priority stays.
	mock.searchResults[0].Status, mock.searchResults[0].StatusCategory = "In Review", "indeterminate"
	runJiraSync(t, conn)
	issue, _ = mirroredIssue(t, conn, "OWN-1")
	if issue.Status != "in_review" || issue.Priority != "urgent" {
		t.Errorf("after Jira status change = %s/%s, want in_review/urgent", issue.Status, issue.Priority)
	}

	mock.searchResults[0].Priority = "Low"
	runJiraSync(t, conn)
	issue, _ = mirroredIssue(t, conn, "OWN-1")
	if issue.Priority != "low" {
		t.Errorf("after Jira priority change = %s, want low", issue.Priority)
	}
	// The activity log and clients learn about the move from issue:updated.
	select {
	case move := <-moves:
		if move != "blocked->in_review" {
			t.Errorf("published status move = %q, want blocked->in_review", move)
		}
	default:
		t.Errorf("Jira status change published no status_changed issue:updated")
	}
}

// subscribeIssueStatusMoves collects "from->to" for issue:updated events that
// report a status change on one issue. Bus handlers are never unsubscribed
// in this suite, so it filters on the issue and never blocks a publisher;
// Publish is synchronous, so events are in the channel when a sync returns.
func subscribeIssueStatusMoves(issueID string) <-chan string {
	moves := make(chan string, 4)
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		resp, ok := payload["issue"].(IssueResponse)
		if !ok || resp.ID != issueID {
			return
		}
		if changed, _ := payload["status_changed"].(bool); !changed {
			return
		}
		prev, _ := payload["prev_status"].(string)
		select {
		case moves <- prev + "->" + resp.Status:
		default:
		}
	})
	return moves
}

// TestJiraStatus_UnseenLinkAppliesOnce verifies links created before the
// seen values existed (NULL) take Jira's current status on the next sync.
func TestJiraStatus_UnseenLinkAppliesOnce(t *testing.T) {
	ctx := context.Background()
	box := withJiraBox(t)
	testHandler.JiraClient = &mockJiraClient{searchResults: []jira.Issue{
		{ID: "1", Key: "OLD-1", Summary: "pre-mapping import", Status: "Done", StatusCategory: "done", Priority: "High"},
	}}
	conn := seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	t.Cleanup(func() { cleanupJira(ctx) })

	runJiraSync(t, conn)
	issue, _ := mirroredIssue(t, conn, "OLD-1")
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'todo', priority = 'none' WHERE id = $1`, uuidToString(issue.ID)); err != nil {
		t.Fatalf("reset issue: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE jira_issue_link SET jira_status = NULL, jira_priority = NULL WHERE multica_issue_id = $1`, uuidToString(issue.ID)); err != nil {
		t.Fatalf("clear seen values: %v", err)
	}

	runJiraSync(t, conn)
	issue, _ = mirroredIssue(t, conn, "OLD-1")
	if issue.Status != "done" || issue.Priority != "high" {
		t.Errorf("unseen link = %s/%s, want done/high", issue.Status, issue.Priority)
	}
}

// TestJiraStatus_WebhookUpdateMovesStatus verifies the webhook route applies
// a Jira transition carried in the payload, including its status category.
func TestJiraStatus_WebhookUpdateMovesStatus(t *testing.T) {
	ctx := context.Background()
	box := withJiraBox(t)
	testHandler.JiraClient = &mockJiraClient{}
	conn := seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	t.Cleanup(func() { cleanupJira(ctx) })

	post := func(raw []byte) {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.HandleJiraWebhook(w, jiraWebhookReq(uuidToString(conn.ID), jiraTestSecret, raw))
		if w.Code != http.StatusAccepted {
			t.Fatalf("webhook: expected 202, got %d (%s)", w.Code, w.Body.String())
		}
	}
	post(jiraIssuePayload("jira:issue_created", "1", "HOOK-1", "Hooked", ""))
	issue, _ := mirroredIssue(t, conn, "HOOK-1")
	if issue.Status != "todo" || issue.Priority != "high" {
		t.Fatalf("created = %s/%s, want todo/high", issue.Status, issue.Priority)
	}

	post([]byte(`{"webhookEvent":"jira:issue_updated","issue":{"id":"1","key":"HOOK-1","fields":{
		"summary":"Hooked","status":{"name":"Deployed","statusCategory":{"key":"done"}},"priority":{"name":"High"}}}}`))
	issue, _ = mirroredIssue(t, conn, "HOOK-1")
	if issue.Status != "done" {
		t.Errorf("after transition status = %s, want done (category fallback)", issue.Status)
	}
}
