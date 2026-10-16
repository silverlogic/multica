package handler

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// defaultStatuses mirrors a freshly seeded workspace catalog plus one custom
// status, in ListIssueStatusEntries order.
var defaultStatuses = []db.IssueStatus{
	{Key: "backlog", Name: "Backlog", Category: "unstarted"},
	{Key: "todo", Name: "Todo", Category: "unstarted"},
	{Key: "in_progress", Name: "In Progress", Category: "started"},
	{Key: "in_review", Name: "In Review", Category: "started"},
	{Key: "blocked", Name: "Blocked", Category: "started"},
	{Key: "qa", Name: "QA Testing", Category: "started"},
	{Key: "done", Name: "Done", Category: "done"},
	{Key: "cancelled", Name: "Cancelled", Category: "closed"},
}

func TestMapJiraStatus(t *testing.T) {
	cases := []struct {
		name, jiraStatus, category string
		statuses                   []db.IssueStatus
		want                       string
	}{
		{"exact name", "In Progress", "indeterminate", defaultStatuses, "in_progress"},
		{"spacing and case", "To Do", "new", defaultStatuses, "todo"},
		{"punctuation", "in-review", "indeterminate", defaultStatuses, "in_review"},
		{"custom status by name", "QA Testing", "indeterminate", defaultStatuses, "qa"},
		{"status key", "in_review", "indeterminate", defaultStatuses, "in_review"},
		{"name beats category", "Cancelled", "done", defaultStatuses, "cancelled"},
		{"unstarted category fallback", "Selected for Development", "new", defaultStatuses, "todo"},
		{"started category fallback", "Code Review", "indeterminate", defaultStatuses, "in_progress"},
		{"done category fallback", "Released", "done", defaultStatuses, "done"},
		{"unknown name and category", "Mystery", "", defaultStatuses, ""},
		{"empty status", "", "", defaultStatuses, ""},
		{
			"preferred key archived: first of the category",
			"Ready", "new",
			[]db.IssueStatus{
				{Key: "backlog", Name: "Backlog", Category: "unstarted"},
				{Key: "done", Name: "Done", Category: "done"},
			},
			"backlog",
		},
		{
			"no status in the category",
			"Shipping", "indeterminate",
			[]db.IssueStatus{{Key: "todo", Name: "Todo", Category: "unstarted"}},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapJiraStatus(tc.statuses, tc.jiraStatus, tc.category); got != tc.want {
				t.Errorf("mapJiraStatus(%q, %q) = %q, want %q", tc.jiraStatus, tc.category, got, tc.want)
			}
		})
	}
}

func TestMapJiraPriority(t *testing.T) {
	cases := map[string]string{
		"Highest": "urgent", "Blocker": "urgent", "Critical": "urgent",
		"High": "high", "Major": "high",
		"Medium": "medium", "Normal": "medium",
		"Low": "low", "Lowest": "low", "Minor": "low", "Trivial": "low",
		" high ": "high",
		"P1":     "",
		"":       "",
	}
	for jiraName, want := range cases {
		if got := mapJiraPriority(jiraName); got != want {
			t.Errorf("mapJiraPriority(%q) = %q, want %q", jiraName, got, want)
		}
	}
}
