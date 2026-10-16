package handler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/jira"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Jira → Multica status and priority mapping for mirrored issues.
//
// Status: a Jira status maps to the active workspace status with the same
// name, compared ignoring case, spaces and punctuation ("To Do" → Todo,
// "In-Progress" → In Progress); the status key is accepted too
// ("in_review"). Without a name match, the Jira status category picks the
// workspace's default status of the matching Multica category, so custom
// Jira workflows still land in a sensible column.
//
// Priority: Jira's default scheme (Highest…Lowest) and the older
// Blocker…Trivial scheme map onto Multica's four levels. Unknown names are
// not mapped.

// jiraCategoryTargets maps a Jira status category key to the Multica status
// category and the built-in status key preferred within it.
var jiraCategoryTargets = map[string]struct{ category, preferredKey string }{
	"new":           {"unstarted", "todo"},
	"indeterminate": {"started", "in_progress"},
	"done":          {"done", "done"},
}

// mapJiraStatus returns the workspace status key for a Jira status, or ""
// when neither the name nor the category resolves. statuses must be the
// workspace's active statuses in catalog order (ListIssueStatusEntries).
func mapJiraStatus(statuses []db.IssueStatus, name, categoryKey string) string {
	if want := normalizeJiraName(name); want != "" {
		for _, st := range statuses {
			if normalizeJiraName(st.Name) == want || normalizeJiraName(st.Key) == want {
				return st.Key
			}
		}
	}
	target, ok := jiraCategoryTargets[categoryKey]
	if !ok {
		return ""
	}
	first := ""
	for _, st := range statuses {
		if st.Category != target.category {
			continue
		}
		if st.Key == target.preferredKey {
			return st.Key
		}
		if first == "" {
			first = st.Key
		}
	}
	return first
}

// mapJiraPriority returns the Multica priority for a Jira priority name, or
// "" when the name is unknown.
func mapJiraPriority(name string) string {
	switch normalizeJiraName(name) {
	case "highest", "blocker", "critical", "urgent":
		return "urgent"
	case "high", "major":
		return "high"
	case "medium", "normal":
		return "medium"
	case "low", "lowest", "minor", "trivial":
		return "low"
	}
	return ""
}

// jiraCreateStatus is the status a newly mirrored issue starts in: Jira's,
// mapped, or todo when it does not map.
func jiraCreateStatus(statuses []db.IssueStatus, ev jira.IssueEvent) string {
	if key := mapJiraStatus(statuses, ev.Status, ev.StatusCategory); key != "" {
		return key
	}
	return "todo"
}

// jiraCreatePriority is the priority a newly mirrored issue starts with:
// Jira's, mapped, or none when it does not map.
func jiraCreatePriority(ev jira.IssueEvent) string {
	if p := mapJiraPriority(ev.Priority); p != "" {
		return p
	}
	return "none"
}

// normalizeJiraName lowercases and keeps only letters and digits.
func normalizeJiraName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// jiraSyncPlan is the workspace state shared by every issue one sync run or
// webhook delivery mirrors, loaded once per run.
type jiraSyncPlan struct {
	props    jiraProperties
	statuses []db.IssueStatus // active workspace statuses, catalog order
}

func (h *Handler) prepareJiraSync(ctx context.Context, conn db.JiraConnection) jiraSyncPlan {
	// Seed first, like the statuses endpoint: an unseeded workspace has no
	// rows yet, and mapping needs the built-ins' names and categories.
	err := issuestatus.Ensure(ctx, h.Queries, conn.WorkspaceID)
	var statuses []db.IssueStatus
	if err == nil {
		statuses, err = h.Queries.ListIssueStatusEntries(ctx, db.ListIssueStatusEntriesParams{WorkspaceID: conn.WorkspaceID})
	}
	if err != nil {
		// Without the catalog no status maps; priorities and the rest still sync.
		slog.Warn("jira: list workspace statuses failed", "workspace_id", uuidToString(conn.WorkspaceID), "err", err)
	}
	return jiraSyncPlan{props: h.resolveJiraProperties(ctx, conn), statuses: statuses}
}

// jiraSeen is the Jira status/priority a sync records on the link as last
// seen. An invalid field keeps the stored value, so a change that did not
// land is retried by the next sync.
type jiraSeen struct {
	status   pgtype.Text
	priority pgtype.Text
}

func jiraSeenFromEvent(ev jira.IssueEvent) jiraSeen {
	return jiraSeen{status: ptrToText(strPtrOrNil(ev.Status)), priority: ptrToText(strPtrOrNil(ev.Priority))}
}

// applyJiraStatusPriority moves an already mirrored issue to Jira's status
// and priority, but only for a field whose Jira value changed since the link
// last saw it: a value set in Multica stays until Jira's changes again.
// Unmapped Jira values change nothing.
func (h *Handler) applyJiraStatusPriority(ctx context.Context, plan jiraSyncPlan, link db.JiraIssueLink, issue db.Issue, ev jira.IssueEvent) (db.Issue, jiraSeen) {
	targetStatus, targetPriority := issue.Status, issue.Priority
	if ev.Status != "" && link.JiraStatus != strToText(ev.Status) {
		if key := mapJiraStatus(plan.statuses, ev.Status, ev.StatusCategory); key != "" {
			targetStatus = key
		}
	}
	if ev.Priority != "" && link.JiraPriority != strToText(ev.Priority) {
		if p := mapJiraPriority(ev.Priority); p != "" {
			targetPriority = p
		}
	}
	seen := jiraSeenFromEvent(ev)
	if targetStatus == issue.Status && targetPriority == issue.Priority {
		return issue, seen
	}
	moved, ok := h.moveIssueFromJira(ctx, issue, targetStatus, targetPriority)
	if !ok {
		return issue, jiraSeen{}
	}
	return moved, seen
}

// moveIssueFromJira writes status/priority with MoveIssueFromJira and, like
// every status write, ends the issue's wakeups when it lands in done/closed.
// It reports false when the write did not land.
func (h *Handler) moveIssueFromJira(ctx context.Context, issue db.Issue, status, priority string) (db.Issue, bool) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Warn("jira: begin status write failed", "err", err)
		return issue, false
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	moved, err := qtx.MoveIssueFromJira(ctx, db.MoveIssueFromJiraParams{
		ID:               issue.ID,
		WorkspaceID:      issue.WorkspaceID,
		TargetStatus:     status,
		TargetPriority:   priority,
		ExpectedStatus:   issue.Status,
		ExpectedPriority: issue.Priority,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Changed in Multica since the sync read it; that edit wins for now.
		return issue, false
	}
	var cancelledWakeups []db.AgentTaskQueue
	if err == nil && moved.Status != issue.Status {
		cancelledWakeups, err = service.StopClosedIssueWakeups(ctx, qtx, moved)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		slog.Warn("jira: status write failed", "issue_id", uuidToString(issue.ID), "err", err)
		return issue, false
	}
	h.broadcastCancelledWakeups(ctx, moved.WorkspaceID, cancelledWakeups)
	return moved, true
}
