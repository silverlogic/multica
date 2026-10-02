package vcs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// bitbucketProvider implements Provider for Bitbucket Cloud. Its differences:
// webhooks are HMAC-SHA256 signed in X-Hub-Signature ("sha256=<hex>") and
// typed by X-Event-Key; there is one fixed API (api.bitbucket.org/2.0) reached
// with an Atlassian API token over basic auth (account email + token; app
// passwords are gone); repositories are "workspace/repo_slug"; and build
// statuses arrive as repo:commit_status_* events, one per status key.
type bitbucketProvider struct{}

func init() { register(bitbucketProvider{}) }

func (bitbucketProvider) Kind() Kind { return KindBitbucket }

// BitbucketCloudURL is the instance URL every Bitbucket Cloud connection
// stores: the provider has a single public site, unlike the self-hosted ones.
const BitbucketCloudURL = "https://bitbucket.org"

func (bitbucketProvider) EventKind(h http.Header) EventKind {
	switch h.Get("X-Event-Key") {
	case "pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected":
		return EventPullRequest
	case "repo:commit_status_created", "repo:commit_status_updated":
		return EventCIStatus
	default:
		return EventOther
	}
}

// VerifySignature checks X-Hub-Signature, the hex HMAC-SHA256 of the raw body
// prefixed with its method ("sha256="). Bitbucket documents that the method may
// change, so any other method is rejected rather than guessed at. An empty
// stored secret never validates.
func (bitbucketProvider) VerifySignature(secret string, h http.Header, body []byte) bool {
	if secret == "" {
		return false
	}
	method, sig, ok := strings.Cut(strings.TrimSpace(h.Get("X-Hub-Signature")), "=")
	if !ok || method != "sha256" {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// bitbucketSHALength is the commit hash length both PR and CI events are
// stored at. Pull request payloads carry an abbreviated hash while build
// statuses carry the full one; the handler joins them on exact equality, so
// both sides are cut to the abbreviation's length.
const bitbucketSHALength = 12

func shortSHA(sha string) string {
	if len(sha) > bitbucketSHALength {
		return sha[:bitbucketSHALength]
	}
	return sha
}

type bbLink struct {
	Href string `json:"href"`
}

type bbPullRequestPayload struct {
	PullRequest struct {
		ID          int32  `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		State       string `json:"state"` // OPEN | MERGED | DECLINED | SUPERSEDED
		Draft       bool   `json:"draft"`
		CreatedOn   string `json:"created_on"`
		UpdatedOn   string `json:"updated_on"`
		Links       struct {
			HTML bbLink `json:"html"`
		} `json:"links"`
		Source struct {
			Branch struct {
				Name string `json:"name"`
			} `json:"branch"`
			Commit struct {
				Hash string `json:"hash"`
			} `json:"commit"`
		} `json:"source"`
		Author struct {
			Nickname    string `json:"nickname"`
			DisplayName string `json:"display_name"`
			Links       struct {
				Avatar bbLink `json:"avatar"`
			} `json:"links"`
		} `json:"author"`
	} `json:"pullrequest"`
	Repository struct {
		FullName string `json:"full_name"` // "workspace/repo_slug"
	} `json:"repository"`
}

// ParsePullRequest maps a pullrequest:* payload. The action lives only in the
// X-Event-Key header, so Action stays empty; state comes from the payload.
// Bitbucket sends no merged/closed timestamps or diff stats: a terminal PR's
// updated_on is its close time.
func (bitbucketProvider) ParsePullRequest(body []byte) (PullRequestEvent, error) {
	var d bbPullRequestPayload
	if err := json.Unmarshal(body, &d); err != nil {
		return PullRequestEvent{}, err
	}
	pr := d.PullRequest
	owner, name, _ := strings.Cut(d.Repository.FullName, "/")
	ev := PullRequestEvent{
		RepoOwner:       owner,
		RepoName:        name,
		Number:          pr.ID,
		Title:           pr.Title,
		Body:            pr.Description,
		State:           normalizeBitbucketPRState(pr.State, pr.Draft),
		HTMLURL:         pr.Links.HTML.Href,
		Branch:          pr.Source.Branch.Name,
		HeadSHA:         shortSHA(pr.Source.Commit.Hash),
		AuthorLogin:     coalesce(pr.Author.Nickname, pr.Author.DisplayName),
		AuthorAvatarURL: pr.Author.Links.Avatar.Href,
		CreatedAt:       pr.CreatedOn,
		UpdatedAt:       pr.UpdatedOn,
	}
	switch ev.State {
	case "merged":
		ev.MergedAt, ev.ClosedAt = pr.UpdatedOn, pr.UpdatedOn
	case "closed":
		ev.ClosedAt = pr.UpdatedOn
	}
	return ev, nil
}

// normalizeBitbucketPRState maps OPEN/MERGED/DECLINED/SUPERSEDED. A superseded
// PR was replaced by another and will not merge, so it reads as closed.
func normalizeBitbucketPRState(state string, draft bool) string {
	switch strings.ToUpper(state) {
	case "MERGED":
		return derivePRState("", false, true)
	case "DECLINED", "SUPERSEDED":
		return derivePRState("closed", false, false)
	default:
		return derivePRState("open", draft, false)
	}
}

type bbCommitStatusPayload struct {
	CommitStatus struct {
		Key         string `json:"key"`
		State       string `json:"state"` // SUCCESSFUL | FAILED | INPROGRESS | STOPPED
		URL         string `json:"url"`
		Description string `json:"description"`
		UpdatedOn   string `json:"updated_on"`
		CreatedOn   string `json:"created_on"`
		Commit      struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"commit_status"`
}

func (bitbucketProvider) ParseCIStatus(body []byte) (CIStatusEvent, error) {
	var d bbCommitStatusPayload
	if err := json.Unmarshal(body, &d); err != nil {
		return CIStatusEvent{}, err
	}
	cs := d.CommitStatus
	return CIStatusEvent{
		SHA:         shortSHA(cs.Commit.Hash),
		Context:     cs.Key,
		State:       normalizeBitbucketStatusState(cs.State),
		TargetURL:   cs.URL,
		Description: cs.Description,
		UpdatedAt:   coalesce(cs.UpdatedOn, cs.CreatedOn),
	}, nil
}

// normalizeBitbucketStatusState maps build states onto passed/failed/pending.
// STOPPED is a terminal non-success, like a cancelled GitHub check.
func normalizeBitbucketStatusState(s string) string {
	switch strings.ToUpper(s) {
	case "SUCCESSFUL":
		return "passed"
	case "FAILED", "STOPPED":
		return "failed"
	default: // INPROGRESS
		return "pending"
	}
}

// BitbucketCredential joins an Atlassian account email and API token into the
// single token string a connection stores and ValidateToken accepts.
func BitbucketCredential(email, apiToken string) string {
	return strings.TrimSpace(email) + ":" + strings.TrimSpace(apiToken)
}

// bitbucketAPIBase returns the REST base for an instance URL: the public API
// for bitbucket.org, otherwise "<instance>/2.0" (test servers).
func bitbucketAPIBase(instanceURL string) string {
	base := NormalizeInstanceURL(instanceURL)
	if u, err := url.Parse(base); err == nil {
		switch strings.ToLower(u.Host) {
		case "bitbucket.org", "www.bitbucket.org", "api.bitbucket.org":
			return "https://api.bitbucket.org/2.0"
		}
	}
	return base + "/2.0"
}

// ValidateToken calls GET /2.0/user with basic auth. token is a
// BitbucketCredential ("email:api-token"); the API token needs the
// read:user:bitbucket scope.
func (bitbucketProvider) ValidateToken(ctx context.Context, instanceURL, token string) (Account, error) {
	email, apiToken, ok := strings.Cut(token, ":")
	if !ok || email == "" || apiToken == "" {
		return Account{}, ErrUnauthorized
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bitbucketAPIBase(instanceURL)+"/user", nil)
	if err != nil {
		return Account{}, fmt.Errorf("bitbucket: build request: %w", err)
	}
	req.SetBasicAuth(email, apiToken)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return Account{}, fmt.Errorf("bitbucket: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Account{}, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Account{}, fmt.Errorf("bitbucket: GET /user: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var u struct {
		Nickname    string `json:"nickname"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return Account{}, fmt.Errorf("bitbucket: decode user: %w", err)
	}
	login := coalesce(u.Nickname, u.DisplayName)
	if login == "" {
		return Account{}, errors.New("bitbucket: user response missing nickname")
	}
	return Account{Login: login}, nil
}
