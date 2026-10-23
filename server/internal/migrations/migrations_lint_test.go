package migrations

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Fork migrations that share their numbers with upstream ones. Databases record
// them by full name, so renaming them would make those databases apply them again.
var forkMigrationsSharingUpstreamNumbers = map[string]bool{
	"583_jira_integration":                    true,
	"584_jira_connection_workspace_index":     true,
	"585_jira_issue_link_workspace_index":     true,
	"586_jira_issue_link_multica_issue_index": true,
	"587_issue_origin_jira":                   true,
	"588_issue_origin_jira_validate":          true,
	"589_jira_connection_jql":                 true,
	"590_jira_connection_property_ids":        true,
	"591_jira_issue_link_seen_status":         true,
	"592_vcs_bitbucket_provider":              true,
}

func TestMigrationNumericPrefixesAreUnique(t *testing.T) {
	files := migrationFilesForLint(t, "*.up.sql")

	// Migrations through 128 contain historical duplicate numeric prefixes.
	// From 129 onward, keep the numeric sequence unique so release tooling and
	// operators can identify one schema change unambiguously by its number.
	const firstUniqueMigrationNumber = 129
	stemByNumber := make(map[int]string)
	forkStemsSeen := make(map[string]bool)
	for _, file := range files {
		stem, _, ok := splitMigrationFilename(filepath.Base(file))
		if !ok {
			continue
		}
		if forkMigrationsSharingUpstreamNumbers[stem] {
			forkStemsSeen[stem] = true
			continue
		}
		prefix, _, ok := strings.Cut(stem, "_")
		if !ok {
			continue
		}
		number, err := strconv.Atoi(prefix)
		if err != nil || number < firstUniqueMigrationNumber {
			continue
		}
		if previous, exists := stemByNumber[number]; exists {
			t.Errorf("migrations %s and %s share numeric prefix %s", previous, stem, prefix)
			continue
		}
		stemByNumber[number] = stem
	}
	for stem := range forkMigrationsSharingUpstreamNumbers {
		if !forkStemsSeen[stem] {
			t.Errorf("allowlisted fork migration %s has no .up.sql file", stem)
		}
	}
}

// A value the fork adds to the allowed list of an upstream column, and the fork
// migration that adds it. Production holds rows using the value, so a later CHECK
// that lists the column's values without it fails to validate there.
type forkColumnValue struct {
	table  string
	column string
	value  string
	since  string
}

var forkColumnValues = []forkColumnValue{
	{table: "issue", column: "origin_type", value: "'jira'", since: "587_issue_origin_jira"},
	{table: "vcs_connection", column: "provider", value: "'bitbucket'", since: "592_vcs_bitbucket_provider"},
	{table: "vcs_pull_request", column: "provider", value: "'bitbucket'", since: "592_vcs_bitbucket_provider"},
}

type migrationFile struct {
	name string
	sql  string
}

func TestLaterMigrationsKeepForkConstraintValues(t *testing.T) {
	var files []migrationFile
	for _, path := range migrationFilesForLint(t, "*.up.sql") {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, migrationFile{name: filepath.Base(path), sql: string(sql)})
	}
	for _, problem := range missingForkColumnValues(files, forkColumnValues) {
		t.Error(problem)
	}
}

func TestMissingForkColumnValues(t *testing.T) {
	rules := []forkColumnValue{
		{table: "issue", column: "origin_type", value: "'jira'", since: "587_issue_origin_jira"},
	}
	const withoutJira = "ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;\n" +
		"ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check\n" +
		"    CHECK (origin_type IN ('autopilot', 'slack_chat')) NOT VALID;\n"
	const withJira = "ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check\n" +
		"    CHECK (origin_type IN ('autopilot', 'slack_chat', 'jira')) NOT VALID;\n"
	const renamedWithoutJira = "ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check_v2\n" +
		"    CHECK (origin_type IN ('autopilot', 'slack_chat')) NOT VALID;\n"
	const otherCheck = "ALTER TABLE issue ADD CONSTRAINT issue_origin_pair_check\n" +
		"    CHECK ((origin_type IS NULL) = (origin_id IS NULL)) NOT VALID;\n"
	const reported = "593_issue_origin_foo.up.sql lists issue.origin_type values without 'jira'; add 'jira' to the list in that migration"

	for _, tc := range []struct {
		name  string
		files []migrationFile
		want  []string
	}{
		{
			name:  "later migration without the value",
			files: []migrationFile{{name: "593_issue_origin_foo.up.sql", sql: withoutJira}},
			want:  []string{reported},
		},
		{
			name:  "later migration under a new constraint name without the value",
			files: []migrationFile{{name: "593_issue_origin_foo.up.sql", sql: renamedWithoutJira}},
			want:  []string{reported},
		},
		{
			name:  "later migration with the value",
			files: []migrationFile{{name: "593_issue_origin_foo.up.sql", sql: withJira}},
		},
		{
			name:  "later check that does not list the column's values",
			files: []migrationFile{{name: "593_issue_origin_foo.up.sql", sql: otherCheck}},
		},
		{
			name:  "migration sorted before the fork migration",
			files: []migrationFile{{name: "366_issue_origin_telegram_chat.up.sql", sql: withoutJira}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := missingForkColumnValues(tc.files, rules)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("problems = %q, want %q", got, tc.want)
			}
		})
	}
}

var sqlLineComment = regexp.MustCompile(`--[^\n]*`)

// missingForkColumnValues reports each statement, in a migration sorted after a
// rule's fork migration, that alters the rule's table with a CHECK listing the
// column's values without the fork's value. Any constraint name matches.
func missingForkColumnValues(files []migrationFile, rules []forkColumnValue) []string {
	sorted := append([]migrationFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })

	var problems []string
	for _, rule := range rules {
		since := rule.since + ".up.sql"
		alterTable := regexp.MustCompile(`(?is)\balter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?(?:"?public"?\.)?"?` +
			regexp.QuoteMeta(rule.table) + `"?\s`)
		listsValues := regexp.MustCompile(`(?is)\bcheck\s*\((?:.*[^\w])?"?` + regexp.QuoteMeta(rule.column) +
			`"?(?:\s*::\s*\w+)?\s*(?:\bin\s*\(|=\s*any\s*\()`)
		for _, file := range sorted {
			if file.name <= since {
				continue
			}
			sql := sqlLineComment.ReplaceAllString(file.sql, "")
			for _, statement := range strings.Split(sql, ";") {
				if alterTable.MatchString(statement) && listsValues.MatchString(statement) &&
					!strings.Contains(statement, rule.value) {
					problems = append(problems, fmt.Sprintf(
						"%s lists %s.%s values without %s; add %s to the list in that migration",
						file.name, rule.table, rule.column, rule.value, rule.value))
				}
			}
		}
	}
	return problems
}

func TestMigrationFilesHaveMatchingDirections(t *testing.T) {
	files := migrationFilesForLint(t, "*.sql")

	directionsByStem := make(map[string]map[string]bool)
	for _, file := range files {
		stem, direction, ok := splitMigrationFilename(filepath.Base(file))
		if !ok {
			continue
		}
		if directionsByStem[stem] == nil {
			directionsByStem[stem] = make(map[string]bool)
		}
		directionsByStem[stem][direction] = true
	}

	for stem, directions := range directionsByStem {
		if !directions["up"] || !directions["down"] {
			t.Errorf("migration %s must have both .up.sql and .down.sql files", stem)
		}
	}
}

func migrationFilesForLint(t *testing.T, pattern string) []string {
	t.Helper()

	dir := realMigrationsDir(t)
	files, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no migration files matched %s in %s", pattern, dir)
	}
	sort.Strings(files)
	return files
}

func realMigrationsDir(t *testing.T) string {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve migration lint test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..", "migrations"))
}

func splitMigrationFilename(name string) (stem, direction string, ok bool) {
	for _, candidateDirection := range []string{"up", "down"} {
		suffix := fmt.Sprintf(".%s.sql", candidateDirection)
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix), candidateDirection, true
		}
	}
	return "", "", false
}
