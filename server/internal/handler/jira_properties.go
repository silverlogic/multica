package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueproperty"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Imported Jira issues carry their Jira key and browse URL in two workspace
// custom properties, so the key can be filtered, sorted and shown as a
// column, and the link leads back to Jira. The connection row remembers which
// definitions it fills (key_property_id / link_property_id): a rename in the
// UI keeps them attached, and archiving one stops the import from filling it.
// A connection without remembered definitions (new, or reconnected after a
// disconnect) adopts existing ones by name before creating its own.

type jiraPropertyField struct {
	name     string
	propType string
	icon     string
}

var (
	jiraKeyProperty  = jiraPropertyField{name: "Jira key", propType: "text", icon: "hash"}
	jiraLinkProperty = jiraPropertyField{name: "Jira link", propType: "url", icon: "link"}
)

const jiraPropertyDescription = "Set by the Jira integration. Edits are overwritten on the next sync."

var errJiraPropertyCap = errors.New("workspace is at the active property limit")

// jiraProperties are the definitions an import fills. An invalid ID means
// that field is skipped (archived, unavailable, or its name taken by a
// property of another type).
type jiraProperties struct {
	key  pgtype.UUID
	link pgtype.UUID
}

// resolveJiraProperties finds or creates the connection's key/link
// definitions and remembers them on the connection. Failures are logged and
// only disable the affected field: the import itself never fails over them.
func (h *Handler) resolveJiraProperties(ctx context.Context, conn db.JiraConnection) jiraProperties {
	keyRemember, keyFill := h.resolveJiraProperty(ctx, conn.WorkspaceID, conn.KeyPropertyID, jiraKeyProperty)
	linkRemember, linkFill := h.resolveJiraProperty(ctx, conn.WorkspaceID, conn.LinkPropertyID, jiraLinkProperty)
	if keyRemember != conn.KeyPropertyID || linkRemember != conn.LinkPropertyID {
		if err := h.Queries.SetJiraConnectionPropertyIDs(ctx, db.SetJiraConnectionPropertyIDsParams{
			ID:             conn.ID,
			WorkspaceID:    conn.WorkspaceID,
			KeyPropertyID:  keyRemember,
			LinkPropertyID: linkRemember,
		}); err != nil {
			slog.Warn("jira: remember property ids failed", "connection_id", uuidToString(conn.ID), "err", err)
		}
	}
	return jiraProperties{key: keyFill, link: linkFill}
}

// resolveJiraProperty returns the definition to remember on the connection
// (kept even while archived, so un-archiving resumes filling it) and the one
// to fill now (invalid when archived or of the wrong type).
func (h *Handler) resolveJiraProperty(ctx context.Context, workspaceID, storedID pgtype.UUID, field jiraPropertyField) (remember, fill pgtype.UUID) {
	if storedID.Valid {
		def, err := h.Queries.GetIssueProperty(ctx, db.GetIssuePropertyParams{ID: storedID, WorkspaceID: workspaceID})
		if err == nil {
			return def.ID, jiraFillableProperty(def, field)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("jira: load property failed", "property", field.name, "err", err)
			return storedID, pgtype.UUID{}
		}
		// The remembered definition is gone; adopt or create one by name.
	}

	def, err := h.Queries.GetIssuePropertyByName(ctx, db.GetIssuePropertyByNameParams{WorkspaceID: workspaceID, Name: field.name})
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		def, err = h.createJiraProperty(ctx, workspaceID, field)
		if err != nil {
			slog.Warn("jira: create property failed", "property", field.name, "err", err)
			return pgtype.UUID{}, pgtype.UUID{}
		}
	default:
		slog.Warn("jira: look up property failed", "property", field.name, "err", err)
		return pgtype.UUID{}, pgtype.UUID{}
	}
	if def.Type != field.propType {
		slog.Warn("jira: property name is taken by another type; not filling it",
			"property", field.name, "type", def.Type)
		return pgtype.UUID{}, pgtype.UUID{}
	}
	return def.ID, jiraFillableProperty(def, field)
}

func jiraFillableProperty(def db.IssueProperty, field jiraPropertyField) pgtype.UUID {
	if def.ArchivedAt.Valid || def.Type != field.propType {
		return pgtype.UUID{}
	}
	return def.ID
}

// createJiraProperty creates a definition under the same workspace lock and
// active-definition cap as the CreateProperty endpoint. A concurrent import
// creating the same name loses the unique index race and adopts the winner.
func (h *Handler) createJiraProperty(ctx context.Context, workspaceID pgtype.UUID, field jiraPropertyField) (db.IssueProperty, error) {
	var created db.IssueProperty
	err := h.withPropertyLockCtx(ctx, []string{"props:" + uuidToString(workspaceID)}, func(q *db.Queries) error {
		active, err := q.CountActiveIssueProperties(ctx, workspaceID)
		if err != nil {
			return err
		}
		if active >= maxActivePropertiesPerWorkspace {
			return errJiraPropertyCap
		}
		created, err = q.CreateIssueProperty(ctx, db.CreateIssuePropertyParams{
			WorkspaceID: workspaceID,
			Name:        field.name,
			Type:        field.propType,
			Description: jiraPropertyDescription,
			Icon:        field.icon,
			Config:      []byte(`{}`),
		})
		return err
	})
	if err != nil && isUniqueViolation(err) {
		return h.Queries.GetIssuePropertyByName(ctx, db.GetIssuePropertyByNameParams{WorkspaceID: workspaceID, Name: field.name})
	}
	if err != nil {
		return created, err
	}
	// Clients cache the property catalog until this event; without it an
	// open tab never shows the new key/link values.
	h.publish(protocol.EventPropertyCreated, uuidToString(workspaceID), "system", "", map[string]any{
		"property": propertyToResponse(created, 0),
	})
	return created, nil
}

// values returns the property values for one Jira issue, keyed by
// definition. Fields without a fillable definition are left out.
func (p jiraProperties) values(baseURL, issueKey string) map[pgtype.UUID]json.RawMessage {
	out := make(map[pgtype.UUID]json.RawMessage, 2)
	if p.key.Valid {
		out[p.key], _ = json.Marshal(issueKey)
	}
	if p.link.Valid {
		out[p.link], _ = json.Marshal(strings.TrimRight(baseURL, "/") + "/browse/" + url.PathEscape(issueKey))
	}
	return out
}

// writeJiraPropertyValues sets the key/link values on an already mirrored
// issue, taking the same per-definition locks as SetIssueProperty (in UUID
// order, like issue create). It returns the issue row after the writes and
// whether any value changed; on failure it returns the input row unchanged.
func (h *Handler) writeJiraPropertyValues(ctx context.Context, issue db.Issue, values map[pgtype.UUID]json.RawMessage) (db.Issue, bool) {
	if len(values) == 0 {
		return issue, false
	}
	ids := make([]pgtype.UUID, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return uuidToString(ids[i]) < uuidToString(ids[j]) })
	lockKeys := make([]string, len(ids))
	for i, id := range ids {
		lockKeys[i] = "prop:" + uuidToString(id)
	}

	updated := issue
	err := h.withPropertyLockCtx(ctx, lockKeys, func(q *db.Queries) error {
		for _, id := range ids {
			def, err := q.GetIssueProperty(ctx, db.GetIssuePropertyParams{ID: id, WorkspaceID: issue.WorkspaceID})
			if err != nil {
				return err
			}
			if def.ArchivedAt.Valid {
				continue // archived since resolve; leave the old value alone
			}
			value, err := issueproperty.ValidateValue(def, values[id])
			if err != nil {
				return err
			}
			updated, err = q.SetIssuePropertyValue(ctx, db.SetIssuePropertyValueParams{
				ID:          issue.ID,
				WorkspaceID: issue.WorkspaceID,
				Key:         uuidToString(def.ID),
				Value:       value,
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("jira: write issue properties failed", "issue_id", uuidToString(issue.ID), "err", err)
		return issue, false
	}
	return updated, updated.Revision != issue.Revision
}
