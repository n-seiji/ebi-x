// Package github reads the pull request state ebi-x watches for a Slack
// thread: whether it merged or closed, its head commit's checks, and new
// reviews and comments.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is the API of github.com.
const DefaultAPIURL = "https://api.github.com"

// maxListPages bounds how many pages one list request reads.
const maxListPages = 10

// Client calls the GitHub REST API with a token that can read the watched
// repositories.
type Client struct {
	apiURL  string
	webHost string
	token   string
	http    *http.Client
}

// NewClient returns a client for apiURL, or for github.com when it is empty.
func NewClient(apiURL, token string) (*Client, error) {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	parsed, err := url.Parse(strings.TrimRight(apiURL, "/"))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("GitHub API URL %q must be an https URL", apiURL)
	}
	// github.com serves its API from api.github.com; GitHub Enterprise
	// Server serves it from the web host under /api/v3.
	webHost := parsed.Host
	if webHost == "api.github.com" {
		webHost = "github.com"
	}
	return &Client{
		apiURL:  parsed.String(),
		webHost: webHost,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// PullRef names one pull request.
type PullRef struct {
	Owner  string
	Repo   string
	Number int
}

// String returns owner/repo#number.
func (r PullRef) String() string {
	return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number)
}

// ParsePullURL reads a pull request's web URL on the client's GitHub host.
func (c *Client) ParsePullURL(raw string) (PullRef, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, c.webHost) {
		return PullRef{}, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return PullRef{}, false
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 || !validName(parts[0]) || !validName(parts[1]) {
		return PullRef{}, false
	}
	return PullRef{Owner: parts[0], Repo: parts[1], Number: number}, true
}

func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// Pull is the part of a pull request the watcher reacts to.
type Pull struct {
	Title   string
	URL     string
	State   string
	Merged  bool
	HeadSHA string
}

// Pull reads a pull request.
func (c *Client) Pull(ctx context.Context, ref PullRef) (Pull, error) {
	var body struct {
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.get(ctx, c.repoPath(ref, "pulls", strconv.Itoa(ref.Number)), &body); err != nil {
		return Pull{}, err
	}
	return Pull{Title: body.Title, URL: body.HTMLURL, State: body.State, Merged: body.Merged, HeadSHA: body.Head.SHA}, nil
}

// Check states of a commit.
const (
	ChecksNone    = "none"
	ChecksPending = "pending"
	ChecksSuccess = "success"
	ChecksFailure = "failure"
)

// Checks summarizes a commit's check runs and commit statuses. State is
// failure once any finished check failed, pending while any is unfinished,
// success when all passed, and none when the commit has no checks.
type Checks struct {
	State  string
	Failed []string
}

// Checks reads the check runs and commit statuses of sha.
func (c *Client) Checks(ctx context.Context, ref PullRef, sha string) (Checks, error) {
	var failed []string
	pending, total := false, 0
	for page := 1; page <= maxListPages; page++ {
		var body struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := c.get(ctx, c.repoPath(ref, "commits", sha, "check-runs")+pageQuery(page), &body); err != nil {
			return Checks{}, err
		}
		for _, run := range body.CheckRuns {
			total++
			switch {
			case run.Status != "completed":
				pending = true
			case failedConclusion(run.Conclusion):
				failed = append(failed, run.Name)
			}
		}
		if len(body.CheckRuns) < 100 || total >= body.TotalCount {
			break
		}
	}
	var status struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := c.get(ctx, c.repoPath(ref, "commits", sha, "status"), &status); err != nil {
		return Checks{}, err
	}
	for _, item := range status.Statuses {
		total++
		switch item.State {
		case "pending":
			pending = true
		case "failure", "error":
			failed = append(failed, item.Context)
		}
	}
	switch {
	case len(failed) > 0:
		return Checks{State: ChecksFailure, Failed: failed}, nil
	case pending:
		return Checks{State: ChecksPending}, nil
	case total == 0:
		return Checks{State: ChecksNone}, nil
	}
	return Checks{State: ChecksSuccess}, nil
}

func failedConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "action_required", "startup_failure":
		return true
	}
	return false
}

// Activity kinds.
const (
	KindReview        = "review"
	KindReviewComment = "review_comment"
	KindComment       = "comment"
)

// Activity is one review or comment on a pull request.
type Activity struct {
	Kind   string
	ID     int64
	Author string
	// State is a review's verdict, such as CHANGES_REQUESTED.
	State string
	Body  string
	Path  string
	URL   string
}

// Cursor is the newest review and comment IDs already seen, per kind,
// since each kind has its own ID sequence.
type Cursor struct {
	Review        int64 `json:"review"`
	ReviewComment int64 `json:"review_comment"`
	Comment       int64 `json:"comment"`
}

// ActivitySince returns the reviews and comments newer than cursor, oldest
// kind first, and the cursor that covers them. Items by ignoreAuthor, such
// as the bot's own token user, advance the cursor but are not returned.
func (c *Client) ActivitySince(ctx context.Context, ref PullRef, cursor Cursor, ignoreAuthor string) ([]Activity, Cursor, error) {
	type item struct {
		ID   int64 `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State   string `json:"state"`
		Body    string `json:"body"`
		Path    string `json:"path"`
		HTMLURL string `json:"html_url"`
	}
	lists := []struct {
		kind string
		path string
		last *int64
	}{
		{KindReview, c.repoPath(ref, "pulls", strconv.Itoa(ref.Number), "reviews"), &cursor.Review},
		{KindReviewComment, c.repoPath(ref, "pulls", strconv.Itoa(ref.Number), "comments"), &cursor.ReviewComment},
		{KindComment, c.repoPath(ref, "issues", strconv.Itoa(ref.Number), "comments"), &cursor.Comment},
	}
	var activity []Activity
	for _, list := range lists {
		newest := *list.last
		for page := 1; page <= maxListPages; page++ {
			var items []item
			if err := c.get(ctx, list.path+pageQuery(page), &items); err != nil {
				return nil, Cursor{}, err
			}
			for _, it := range items {
				if it.ID <= *list.last {
					continue
				}
				newest = max(newest, it.ID)
				// A pending review is the reviewer's unsent draft.
				if strings.EqualFold(it.User.Login, ignoreAuthor) || it.State == "PENDING" {
					continue
				}
				activity = append(activity, Activity{
					Kind: list.kind, ID: it.ID, Author: it.User.Login, State: it.State,
					Body: it.Body, Path: it.Path, URL: it.HTMLURL,
				})
			}
			if len(items) < 100 {
				break
			}
		}
		*list.last = newest
	}
	return activity, cursor, nil
}

// Login returns the token user's login.
func (c *Client) Login(ctx context.Context) (string, error) {
	var body struct {
		Login string `json:"login"`
	}
	if err := c.get(ctx, "/user", &body); err != nil {
		return "", err
	}
	return body.Login, nil
}

func (c *Client) repoPath(ref PullRef, parts ...string) string {
	escaped := []string{"repos", url.PathEscape(ref.Owner), url.PathEscape(ref.Repo)}
	for _, part := range parts {
		escaped = append(escaped, url.PathEscape(part))
	}
	return "/" + strings.Join(escaped, "/")
}

func pageQuery(page int) string {
	return "?per_page=100&page=" + strconv.Itoa(page)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("GitHub GET %s: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("GitHub GET %s: %s: %s", path, response.Status, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("GitHub GET %s: decode: %w", path, err)
	}
	return nil
}
