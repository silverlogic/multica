package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func bitbucketPRBody(t *testing.T, state, title, branch, headHash string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"pullrequest": map[string]any{
			"id": 42, "title": title, "description": "", "state": state, "draft": false,
			"created_on": "2026-10-01T10:00:00.000000+00:00",
			"updated_on": map[string]string{"OPEN": "2026-10-01T10:00:00.000000+00:00", "MERGED": "2026-10-01T12:00:00.000000+00:00"}[state],
			"links":      map[string]any{"html": map[string]any{"href": "https://bitbucket.org/acme/web/pull-requests/42"}},
			"source": map[string]any{
				"branch": map[string]any{"name": branch},
				"commit": map[string]any{"hash": headHash},
			},
			"author": map[string]any{"nickname": "alisson"},
		},
		"repository": map[string]any{"full_name": "acme/web"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func postBitbucketWebhook(t *testing.T, connID, eventKey string, raw []byte, signature string) int {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.HandleVCSWebhook(w, vcsWebhookReq(connID, map[string]string{
		"X-Event-Key": eventKey, "X-Hub-Signature": signature,
	}, raw))
	return w.Code
}

// TestVCSWebhook_BitbucketPullRequestLifecycle covers a Bitbucket PR end to
// end: it links on the identifier in its title, a build status sent with the
// full commit hash joins the PR's abbreviated head, and the merge completes
// the issue.
func TestVCSWebhook_BitbucketPullRequestLifecycle(t *testing.T) {
	ctx := context.Background()
	box := withVCSBox(t)
	connID := seedVCSConnection(t, ctx, box, "bitbucket", "https://bitbucket.org")
	issue := newVCSIssue(t, "Bitbucket PR test")
	t.Cleanup(func() { cleanupVCS(ctx, issue.ID) })

	opened := bitbucketPRBody(t, "OPEN", issue.Identifier+" footer", "feature/footer", "d3022fc0ca3d")
	if code := postBitbucketWebhook(t, connID, "pullrequest:created", opened, "sha256="+giteaSig(opened)); code != http.StatusAccepted {
		t.Fatalf("pullrequest:created: expected 202, got %d", code)
	}
	rows, err := testHandler.Queries.ListVCSPullRequestsByIssue(ctx, parseUUID(issue.ID))
	if err != nil {
		t.Fatalf("ListVCSPullRequestsByIssue: %v", err)
	}
	if len(rows) != 1 || rows[0].Provider != "bitbucket" || rows[0].RepoOwner != "acme" || rows[0].RepoName != "web" ||
		rows[0].PrNumber != 42 || rows[0].State != "open" {
		t.Fatalf("unexpected rows after open: %+v", rows)
	}

	status, _ := json.Marshal(map[string]any{"commit_status": map[string]any{
		"key": "pipelines", "state": "SUCCESSFUL", "url": "https://bitbucket.org/acme/web/pipelines/results/1",
		"updated_on": "2026-10-01T11:00:00+00:00",
		"commit":     map[string]any{"hash": "d3022fc0ca3d1c9b8a7e6f5d4c3b2a1908f7e6d5"},
	}})
	if code := postBitbucketWebhook(t, connID, "repo:commit_status_updated", status, "sha256="+giteaSig(status)); code != http.StatusAccepted {
		t.Fatalf("commit status: expected 202, got %d", code)
	}
	rows, _ = testHandler.Queries.ListVCSPullRequestsByIssue(ctx, parseUUID(issue.ID))
	if len(rows) != 1 || rows[0].ChecksTotal != 1 || rows[0].ChecksPassed != 1 {
		t.Fatalf("expected 1 passed check joined on the short head hash, got %+v", rows)
	}

	merged := bitbucketPRBody(t, "MERGED", issue.Identifier+" footer", "feature/footer", "d3022fc0ca3d")
	if code := postBitbucketWebhook(t, connID, "pullrequest:fulfilled", merged, "sha256="+giteaSig(merged)); code != http.StatusAccepted {
		t.Fatalf("pullrequest:fulfilled: expected 202, got %d", code)
	}
	rows, _ = testHandler.Queries.ListVCSPullRequestsByIssue(ctx, parseUUID(issue.ID))
	if len(rows) != 1 || rows[0].State != "merged" {
		t.Fatalf("expected merged PR, got %+v", rows)
	}
	updated, _ := testHandler.Queries.GetIssue(ctx, parseUUID(issue.ID))
	if updated.Status != "done" {
		t.Errorf("merged PR should complete the issue, got %q", updated.Status)
	}
}

// TestVCSWebhook_BitbucketLinksByJiraKey verifies a branch named after a Jira
// ticket links the issue mirrored from that ticket.
func TestVCSWebhook_BitbucketLinksByJiraKey(t *testing.T) {
	ctx := context.Background()
	box := withVCSBox(t)
	jiraBox := withJiraBox(t)
	connID := seedVCSConnection(t, ctx, box, "bitbucket", "https://bitbucket.org")
	issue := newVCSIssue(t, "Mirrored from Jira")
	jiraConn := seedJiraConnection(t, ctx, jiraBox, "https://acme.atlassian.net")
	t.Cleanup(func() {
		cleanupVCS(ctx, issue.ID)
		cleanupJira(ctx)
	})
	if _, err := testHandler.Queries.UpsertJiraIssueLink(ctx, db.UpsertJiraIssueLinkParams{
		WorkspaceID: jiraConn.WorkspaceID, ConnectionID: jiraConn.ID, JiraIssueKey: "BA-1688",
		JiraIssueID: "10001", MulticaIssueID: parseUUID(issue.ID), SyncStatus: "synced",
	}); err != nil {
		t.Fatalf("UpsertJiraIssueLink: %v", err)
	}

	raw := bitbucketPRBody(t, "OPEN", "Footer tweaks", "feature/BA-1688-footer", "d3022fc0ca3d")
	if code := postBitbucketWebhook(t, connID, "pullrequest:created", raw, "sha256="+giteaSig(raw)); code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", code)
	}
	rows, _ := testHandler.Queries.ListVCSPullRequestsByIssue(ctx, parseUUID(issue.ID))
	if len(rows) != 1 {
		t.Fatalf("branch named after the Jira key did not link the mirrored issue: %+v", rows)
	}
}

func TestVCSWebhook_BitbucketBadSignature(t *testing.T) {
	ctx := context.Background()
	box := withVCSBox(t)
	connID := seedVCSConnection(t, ctx, box, "bitbucket", "https://bitbucket.org")
	t.Cleanup(func() { cleanupVCS(ctx, "") })

	raw := bitbucketPRBody(t, "OPEN", "untrusted", "feature/x", "abc")
	for _, sig := range []string{"", "sha256=00", giteaSig(raw)} { // last: missing "sha256=" prefix
		if code := postBitbucketWebhook(t, connID, "pullrequest:created", raw, sig); code != http.StatusUnauthorized {
			t.Errorf("signature %q: expected 401, got %d", sig, code)
		}
	}
}

// TestConnectVCS_BitbucketCredential verifies Bitbucket connects with an
// account email plus API token and stores them as one basic-auth credential.
func TestConnectVCS_BitbucketCredential(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if r.URL.Path != "/2.0/user" || !ok || user != "dev@acme.test" || pass != "api-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"nickname":"alisson"}`))
	}))
	defer api.Close()
	withVCSBox(t)
	t.Cleanup(func() { cleanupVCS(context.Background(), "") })

	connect := func(body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.ConnectVCS(w, vcsHandlerRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/vcs/connections", body, ""))
		return w
	}

	if w := connect(map[string]any{"provider": "bitbucket", "instance_url": api.URL, "access_token": "api-token"}); w.Code != http.StatusBadRequest {
		t.Errorf("connect without account_email: expected 400, got %d", w.Code)
	}
	w := connect(map[string]any{
		"provider": "bitbucket", "instance_url": api.URL,
		"account_email": "dev@acme.test", "access_token": "api-token",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("connect: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp VCSConnectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Provider != "bitbucket" || resp.AccountLogin != "alisson" || resp.WebhookSecret == "" {
		t.Errorf("unexpected connect response %+v", resp)
	}
	conn, err := testHandler.Queries.GetVCSConnectionByID(context.Background(), parseUUID(resp.ID))
	if err != nil {
		t.Fatalf("GetVCSConnectionByID: %v", err)
	}
	stored, err := testHandler.openVCSSecret(conn.AccessTokenEncrypted)
	if err != nil || stored != "dev@acme.test:api-token" {
		t.Errorf("stored credential = %q (%v), want email:token", stored, err)
	}
}
