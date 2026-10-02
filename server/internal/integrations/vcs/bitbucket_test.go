package vcs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func bitbucketSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestBitbucketVerifySignature(t *testing.T) {
	p, _ := For("bitbucket")
	body := []byte(`{"pullrequest":{}}`)
	h := http.Header{}
	h.Set("X-Hub-Signature", bitbucketSig("s3cret", body))
	if !p.VerifySignature("s3cret", h, body) {
		t.Error("valid signature rejected")
	}
	if p.VerifySignature("other", h, body) {
		t.Error("signature with the wrong secret accepted")
	}
	if p.VerifySignature("s3cret", h, []byte(`{"tampered":true}`)) {
		t.Error("signature over a different body accepted")
	}
	if p.VerifySignature("", h, body) {
		t.Error("empty secret must never validate")
	}
	for _, bad := range []string{"", "sha1=abcd", "sha256=not-hex", "abcd"} {
		h.Set("X-Hub-Signature", bad)
		if p.VerifySignature("s3cret", h, body) {
			t.Errorf("malformed signature %q accepted", bad)
		}
	}
}

func TestBitbucketEventKind(t *testing.T) {
	p, _ := For("bitbucket")
	cases := map[string]EventKind{
		"pullrequest:created":        EventPullRequest,
		"pullrequest:updated":        EventPullRequest,
		"pullrequest:fulfilled":      EventPullRequest,
		"pullrequest:rejected":       EventPullRequest,
		"repo:commit_status_created": EventCIStatus,
		"repo:commit_status_updated": EventCIStatus,
		"repo:push":                  EventOther,
		"":                           EventOther,
	}
	for key, want := range cases {
		h := http.Header{}
		h.Set("X-Event-Key", key)
		if got := p.EventKind(h); got != want {
			t.Errorf("EventKind(%q) = %v, want %v", key, got, want)
		}
	}
}

const bitbucketPRPayload = `{
  "pullrequest": {
    "id": 42,
    "title": "BA-1688 Footer",
    "description": "Closes TSL-118",
    "state": "%s",
    "draft": %s,
    "created_on": "2026-10-01T10:30:00.123456+00:00",
    "updated_on": "2026-10-02T11:00:00.654321+00:00",
    "links": {"html": {"href": "https://bitbucket.org/acme/web/pull-requests/42"}},
    "source": {"branch": {"name": "feature/BA-1688-footer"}, "commit": {"hash": "d3022fc0ca3d"}},
    "author": {"nickname": "alisson", "display_name": "Alisson P", "links": {"avatar": {"href": "https://avatar.test/a.png"}}}
  },
  "repository": {"full_name": "acme/web", "name": "Web App"}
}`

func bitbucketPR(state, draft string) []byte {
	return []byte(fmt.Sprintf(bitbucketPRPayload, state, draft))
}

func TestBitbucketParsePullRequest(t *testing.T) {
	p, _ := For("bitbucket")
	pr, err := p.ParsePullRequest(bitbucketPR("OPEN", "false"))
	if err != nil {
		t.Fatalf("ParsePullRequest: %v", err)
	}
	want := PullRequestEvent{
		RepoOwner:       "acme",
		RepoName:        "web", // repo slug from full_name, not the display name
		Number:          42,
		Title:           "BA-1688 Footer",
		Body:            "Closes TSL-118",
		State:           "open",
		HTMLURL:         "https://bitbucket.org/acme/web/pull-requests/42",
		Branch:          "feature/BA-1688-footer",
		HeadSHA:         "d3022fc0ca3d",
		AuthorLogin:     "alisson",
		AuthorAvatarURL: "https://avatar.test/a.png",
		CreatedAt:       "2026-10-01T10:30:00.123456+00:00",
		UpdatedAt:       "2026-10-02T11:00:00.654321+00:00",
	}
	if pr != want {
		t.Errorf("parsed PR:\n got %+v\nwant %+v", pr, want)
	}

	cases := []struct{ state, draft, want string }{
		{"OPEN", "true", "draft"},
		{"MERGED", "false", "merged"},
		{"DECLINED", "false", "closed"},
		{"SUPERSEDED", "false", "closed"},
	}
	for _, c := range cases {
		pr, err := p.ParsePullRequest(bitbucketPR(c.state, c.draft))
		if err != nil {
			t.Fatalf("ParsePullRequest(%s): %v", c.state, err)
		}
		if pr.State != c.want {
			t.Errorf("state %s draft=%s = %q, want %q", c.state, c.draft, pr.State, c.want)
		}
	}
	merged, _ := p.ParsePullRequest(bitbucketPR("MERGED", "false"))
	if merged.MergedAt != merged.UpdatedAt || merged.ClosedAt != merged.UpdatedAt {
		t.Errorf("merged PR times = merged %q closed %q, want updated_on", merged.MergedAt, merged.ClosedAt)
	}
	if _, err := p.ParsePullRequest([]byte(`not json`)); err == nil {
		t.Error("malformed payload parsed without error")
	}
}

// PR payloads abbreviate the head hash while build statuses send all 40
// characters; both must land at the same length or CI never joins the PR.
func TestBitbucketParseCIStatusMatchesPRHead(t *testing.T) {
	p, _ := For("bitbucket")
	ci, err := p.ParseCIStatus([]byte(`{"commit_status": {
		"key": "pipelines-build", "state": "SUCCESSFUL",
		"url": "https://bitbucket.org/acme/web/pipelines/results/7",
		"description": "Build passed", "updated_on": "2026-10-02T11:05:00+00:00",
		"commit": {"hash": "d3022fc0ca3d1c9b8a7e6f5d4c3b2a1908f7e6d5"}}}`))
	if err != nil {
		t.Fatalf("ParseCIStatus: %v", err)
	}
	pr, _ := p.ParsePullRequest(bitbucketPR("OPEN", "false"))
	if ci.SHA != pr.HeadSHA {
		t.Errorf("CI sha %q does not match PR head %q", ci.SHA, pr.HeadSHA)
	}
	if ci.Context != "pipelines-build" || ci.State != "passed" || ci.TargetURL == "" || ci.UpdatedAt == "" {
		t.Errorf("unexpected CI status %+v", ci)
	}
	for state, want := range map[string]string{"FAILED": "failed", "STOPPED": "failed", "INPROGRESS": "pending"} {
		if got := normalizeBitbucketStatusState(state); got != want {
			t.Errorf("normalizeBitbucketStatusState(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestBitbucketValidateToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2.0/user" {
			t.Errorf("path = %q, want /2.0/user", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "dev@acme.test" || pass != "api-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"nickname":"alisson","display_name":"Alisson P"}`))
	}))
	defer srv.Close()
	p, _ := For("bitbucket")

	acct, err := p.ValidateToken(context.Background(), srv.URL, BitbucketCredential(" dev@acme.test ", "api-token"))
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if acct.Login != "alisson" {
		t.Errorf("login = %q, want alisson", acct.Login)
	}
	if _, err := p.ValidateToken(context.Background(), srv.URL, BitbucketCredential("dev@acme.test", "wrong")); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("rejected token err = %v, want ErrUnauthorized", err)
	}
	if _, err := p.ValidateToken(context.Background(), srv.URL, "token-without-email"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("credential without email err = %v, want ErrUnauthorized", err)
	}
}

func TestBitbucketAPIBase(t *testing.T) {
	cases := map[string]string{
		"https://bitbucket.org":     "https://api.bitbucket.org/2.0",
		"https://bitbucket.org/":    "https://api.bitbucket.org/2.0",
		"https://www.bitbucket.org": "https://api.bitbucket.org/2.0",
		"http://127.0.0.1:9999":     "http://127.0.0.1:9999/2.0",
	}
	for in, want := range cases {
		if got := bitbucketAPIBase(in); got != want {
			t.Errorf("bitbucketAPIBase(%q) = %q, want %q", in, got, want)
		}
	}
}
