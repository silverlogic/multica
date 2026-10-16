package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/jira"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func runJiraSync(t *testing.T, conn db.JiraConnection) JiraSyncResponse {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.SyncJiraConnection(w, jiraSyncReq(testWorkspaceID, uuidToString(conn.ID)))
	if w.Code != http.StatusOK {
		t.Fatalf("sync: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp JiraSyncResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sync response: %v", err)
	}
	return resp
}

// mirroredIssue returns the Multica issue linked to a Jira key, with its
// property bag decoded.
func mirroredIssue(t *testing.T, conn db.JiraConnection, key string) (db.Issue, map[string]any) {
	t.Helper()
	ctx := context.Background()
	link, err := testHandler.Queries.GetJiraIssueLink(ctx, db.GetJiraIssueLinkParams{ConnectionID: conn.ID, JiraIssueKey: key})
	if err != nil {
		t.Fatalf("GetJiraIssueLink(%s): %v", key, err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, link.MulticaIssueID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", key, err)
	}
	props := map[string]any{}
	if err := json.Unmarshal(issue.Properties, &props); err != nil {
		t.Fatalf("decode properties: %v", err)
	}
	return issue, props
}

func jiraPropertyByName(t *testing.T, name string) db.IssueProperty {
	t.Helper()
	def, err := testHandler.Queries.GetIssuePropertyByName(context.Background(), db.GetIssuePropertyByNameParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		Name:        name,
	})
	if err != nil {
		t.Fatalf("GetIssuePropertyByName(%q): %v", name, err)
	}
	return def
}

func countJiraNamedProperties(t *testing.T) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_property WHERE workspace_id = $1 AND LOWER(name) IN ('jira key', 'jira link')`,
		testWorkspaceID).Scan(&n); err != nil {
		t.Fatalf("count properties: %v", err)
	}
	return n
}

// TestJiraProperties_FilledOnImport covers both import routes: pull sync
// creates the two definitions once and fills key + browse URL, an unchanged
// re-sync leaves the issue revision alone, and a webhook-created issue gets
// the same values through the shared create path.
func TestJiraProperties_FilledOnImport(t *testing.T) {
	ctx := context.Background()
	box := withJiraBox(t)
	mock := &mockJiraClient{searchResults: []jira.Issue{
		{ID: "10001", Key: "VUP-2159", Summary: "[FE] Footer"},
	}}
	testHandler.JiraClient = mock
	conn := seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	t.Cleanup(func() { cleanupJira(ctx) })
	created := subscribePropertyCreated()

	runJiraSync(t, conn)
	keyDef := jiraPropertyByName(t, "Jira key")
	linkDef := jiraPropertyByName(t, "Jira link")
	// Open clients refresh their cached property catalog from this event.
	for _, def := range []db.IssueProperty{keyDef, linkDef} {
		if !created[uuidToString(def.ID)] {
			t.Errorf("no property:created event for %q", def.Name)
		}
	}
	if keyDef.Type != "text" || linkDef.Type != "url" {
		t.Fatalf("types = %q/%q, want text/url", keyDef.Type, linkDef.Type)
	}
	stored, err := testHandler.Queries.GetJiraConnectionByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("GetJiraConnectionByID: %v", err)
	}
	if stored.KeyPropertyID != keyDef.ID || stored.LinkPropertyID != linkDef.ID {
		t.Errorf("connection does not remember the created definitions")
	}

	issue, props := mirroredIssue(t, conn, "VUP-2159")
	if got := props[uuidToString(keyDef.ID)]; got != "VUP-2159" {
		t.Errorf("Jira key = %v, want VUP-2159", got)
	}
	if got := props[uuidToString(linkDef.ID)]; got != "https://acme.atlassian.net/browse/VUP-2159" {
		t.Errorf("Jira link = %v, want the browse URL", got)
	}

	runJiraSync(t, conn)
	again, _ := mirroredIssue(t, conn, "VUP-2159")
	if again.Revision != issue.Revision {
		t.Errorf("unchanged re-sync moved revision %d -> %d", issue.Revision, again.Revision)
	}
	if n := countJiraNamedProperties(t); n != 2 {
		t.Errorf("re-sync created duplicate definitions: %d", n)
	}

	w := httptest.NewRecorder()
	testHandler.HandleJiraWebhook(w, jiraWebhookReq(uuidToString(conn.ID), jiraTestSecret,
		jiraIssuePayload("jira:issue_created", "10002", "VUP-2160", "[FE] Home", "")))
	if w.Code != http.StatusAccepted {
		t.Fatalf("webhook: expected 202, got %d (%s)", w.Code, w.Body.String())
	}
	_, props = mirroredIssue(t, conn, "VUP-2160")
	if got := props[uuidToString(keyDef.ID)]; got != "VUP-2160" {
		t.Errorf("webhook-created Jira key = %v, want VUP-2160", got)
	}
}

// TestJiraProperties_RenamedPropertyStaysAttached verifies a rename in the
// UI keeps the remembered definition in use, and that sync heals a cleared
// value instead of creating a fresh "Jira key".
func TestJiraProperties_RenamedPropertyStaysAttached(t *testing.T) {
	ctx := context.Background()
	box := withJiraBox(t)
	testHandler.JiraClient = &mockJiraClient{searchResults: []jira.Issue{
		{ID: "10001", Key: "WELD-1011", Summary: "Purchase individual course"},
	}}
	conn := seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	t.Cleanup(func() { cleanupJira(ctx) })

	runJiraSync(t, conn)
	keyDef := jiraPropertyByName(t, "Jira key")
	issue, _ := mirroredIssue(t, conn, "WELD-1011")
	if _, err := testPool.Exec(ctx, `UPDATE issue_property SET name = 'Ticket' WHERE id = $1`, uuidToString(keyDef.ID)); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE issue SET properties = '{}' WHERE id = $1`, uuidToString(issue.ID)); err != nil {
		t.Fatalf("clear properties: %v", err)
	}

	runJiraSync(t, conn)
	_, props := mirroredIssue(t, conn, "WELD-1011")
	if got := props[uuidToString(keyDef.ID)]; got != "WELD-1011" {
		t.Errorf("renamed property not refilled: %v", got)
	}
	var n int
	testPool.QueryRow(ctx, `SELECT count(*) FROM issue_property WHERE workspace_id = $1 AND LOWER(name) = 'jira key'`, testWorkspaceID).Scan(&n)
	if n != 0 {
		t.Errorf("rename caused a new \"Jira key\" definition")
	}
}

// TestJiraProperties_ArchivedAndReconnect verifies archiving a definition
// stops filling it without recreating it, and that a reconnected site adopts
// the existing definitions by name instead of duplicating them.
func TestJiraProperties_ArchivedAndReconnect(t *testing.T) {
	ctx := context.Background()
	box := withJiraBox(t)
	mock := &mockJiraClient{searchResults: []jira.Issue{
		{ID: "10001", Key: "WAYS-1434", Summary: "Journey import validation"},
	}}
	testHandler.JiraClient = mock
	conn := seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	t.Cleanup(func() { cleanupJira(ctx) })

	runJiraSync(t, conn)
	keyDef := jiraPropertyByName(t, "Jira key")
	linkDef := jiraPropertyByName(t, "Jira link")
	if _, err := testPool.Exec(ctx, `UPDATE issue_property SET archived_at = now() WHERE id = $1`, uuidToString(linkDef.ID)); err != nil {
		t.Fatalf("archive: %v", err)
	}

	mock.searchResults = append(mock.searchResults, jira.Issue{ID: "10002", Key: "WAYS-1448", Summary: "Key rotation"})
	if resp := runJiraSync(t, conn); resp.Created != 1 {
		t.Fatalf("sync with archived property = %+v, want 1 created", resp)
	}
	_, props := mirroredIssue(t, conn, "WAYS-1448")
	if _, ok := props[uuidToString(linkDef.ID)]; ok {
		t.Errorf("archived Jira link was filled")
	}
	if got := props[uuidToString(keyDef.ID)]; got != "WAYS-1448" {
		t.Errorf("Jira key = %v, want WAYS-1448", got)
	}

	// Disconnect + reconnect: the new connection remembers nothing yet.
	if err := testHandler.Queries.DeleteJiraConnection(ctx, db.DeleteJiraConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID}); err != nil {
		t.Fatalf("DeleteJiraConnection: %v", err)
	}
	conn = seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	mock.searchResults = []jira.Issue{{ID: "10003", Key: "WAYS-1449", Summary: "Old key removal"}}
	runJiraSync(t, conn)
	if n := countJiraNamedProperties(t); n != 2 {
		t.Errorf("reconnect duplicated definitions: %d", n)
	}
	_, props = mirroredIssue(t, conn, "WAYS-1449")
	if got := props[uuidToString(keyDef.ID)]; got != "WAYS-1449" {
		t.Errorf("reconnected Jira key = %v, want WAYS-1449 on the adopted definition", got)
	}
	if _, ok := props[uuidToString(linkDef.ID)]; ok {
		t.Errorf("reconnect filled the still-archived Jira link")
	}
}

// TestJiraProperties_NameTakenByOtherType verifies a same-named property of
// another type is left alone and does not block the import.
func TestJiraProperties_NameTakenByOtherType(t *testing.T) {
	ctx := context.Background()
	box := withJiraBox(t)
	testHandler.JiraClient = &mockJiraClient{searchResults: []jira.Issue{
		{ID: "10001", Key: "OPS-1", Summary: "Ops issue"},
	}}
	conn := seedJiraConnection(t, ctx, box, "https://acme.atlassian.net")
	t.Cleanup(func() { cleanupJira(ctx) })
	if _, err := testHandler.Queries.CreateIssueProperty(ctx, db.CreateIssuePropertyParams{
		WorkspaceID: parseUUID(testWorkspaceID), Name: "Jira key", Type: "number", Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateIssueProperty: %v", err)
	}

	if resp := runJiraSync(t, conn); resp.Created != 1 {
		t.Fatalf("sync = %+v, want 1 created", resp)
	}
	_, props := mirroredIssue(t, conn, "OPS-1")
	numberDef := jiraPropertyByName(t, "Jira key")
	if _, ok := props[uuidToString(numberDef.ID)]; ok {
		t.Errorf("filled a number-typed \"Jira key\" with a key")
	}
	linkDef := jiraPropertyByName(t, "Jira link")
	if got := props[uuidToString(linkDef.ID)]; got != "https://acme.atlassian.net/browse/OPS-1" {
		t.Errorf("Jira link = %v, want the browse URL", got)
	}
}

// subscribePropertyCreated records the ids of property:created events. Bus
// handlers are never unsubscribed in this suite; Publish is synchronous, so
// the map is filled by the time a sync returns.
func subscribePropertyCreated() map[string]bool {
	seen := map[string]bool{}
	testHandler.Bus.Subscribe(protocol.EventPropertyCreated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		if resp, ok := payload["property"].(PropertyResponse); ok {
			seen[resp.ID] = true
		}
	})
	return seen
}
