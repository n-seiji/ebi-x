package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func testClient(t *testing.T, routes map[string]string) *Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") > "1" {
			body = "[]"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	return client
}

func TestParsePullURL(t *testing.T) {
	client, err := NewClient("", "token")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		raw  string
		want PullRef
		ok   bool
	}{
		"pull":        {raw: "https://github.com/n-seiji/ebi-x/pull/15", want: PullRef{"n-seiji", "ebi-x", 15}, ok: true},
		"files tab":   {raw: "https://github.com/n-seiji/ebi-x/pull/15/files", want: PullRef{"n-seiji", "ebi-x", 15}, ok: true},
		"issue":       {raw: "https://github.com/n-seiji/ebi-x/issues/15"},
		"other host":  {raw: "https://example.com/n-seiji/ebi-x/pull/15"},
		"http":        {raw: "http://github.com/n-seiji/ebi-x/pull/15"},
		"bad number":  {raw: "https://github.com/n-seiji/ebi-x/pull/x"},
		"odd owner":   {raw: "https://github.com/..%2F/ebi-x/pull/1"},
		"not a url":   {raw: "PR #15"},
		"zero number": {raw: "https://github.com/o/r/pull/0"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := client.ParsePullURL(test.raw)
			if ok != test.ok || got != test.want {
				t.Fatalf("ParsePullURL(%q) = (%v, %v), want (%v, %v)", test.raw, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestEnterpriseWebHost(t *testing.T) {
	client, err := NewClient("https://ghe.example.com/api/v3", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.ParsePullURL("https://ghe.example.com/o/r/pull/3"); !ok {
		t.Fatal("GitHub Enterprise pull request URL was not accepted")
	}
	if _, err := NewClient("http://ghe.example.com/api/v3", "token"); err == nil {
		t.Fatal("NewClient accepted a plain HTTP API URL")
	}
}

func TestPullAndChecks(t *testing.T) {
	ref := PullRef{"o", "r", 1}
	client := testClient(t, map[string]string{
		"/repos/o/r/pulls/1":                    `{"title":"T","html_url":"https://github.com/o/r/pull/1","state":"open","merged":false,"head":{"sha":"abc"}}`,
		"/repos/o/r/commits/abc/check-runs":     `{"total_count":2,"check_runs":[{"name":"test","status":"completed","conclusion":"failure"},{"name":"lint","status":"completed","conclusion":"success"}]}`,
		"/repos/o/r/commits/abc/status":         `{"statuses":[{"context":"ci/legacy","state":"error"}]}`,
		"/repos/o/r/commits/def/check-runs":     `{"total_count":1,"check_runs":[{"name":"test","status":"in_progress","conclusion":null}]}`,
		"/repos/o/r/commits/def/status":         `{"statuses":[]}`,
		"/repos/o/r/commits/none/check-runs":    `{"total_count":0,"check_runs":[]}`,
		"/repos/o/r/commits/none/status":        `{"statuses":[]}`,
		"/repos/o/r/commits/green/check-runs":   `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`,
		"/repos/o/r/commits/green/status":       `{"statuses":[{"context":"x","state":"success"}]}`,
		"/repos/o/r/commits/skipped/check-runs": `{"total_count":1,"check_runs":[{"name":"opt","status":"completed","conclusion":"skipped"}]}`,
		"/repos/o/r/commits/skipped/status":     `{"statuses":[]}`,
	})
	pull, err := client.Pull(context.Background(), ref)
	if err != nil || pull.HeadSHA != "abc" || pull.State != "open" || pull.Title != "T" {
		t.Fatalf("Pull() = %+v, %v", pull, err)
	}
	tests := map[string]Checks{
		"abc":     {State: ChecksFailure, Failed: []string{"test", "ci/legacy"}},
		"def":     {State: ChecksPending},
		"none":    {State: ChecksNone},
		"green":   {State: ChecksSuccess},
		"skipped": {State: ChecksSuccess},
	}
	for sha, want := range tests {
		got, err := client.Checks(context.Background(), ref, sha)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Checks(%s) = %+v, %v; want %+v", sha, got, err, want)
		}
	}
}

func TestActivitySince(t *testing.T) {
	ref := PullRef{"o", "r", 1}
	client := testClient(t, map[string]string{
		"/repos/o/r/pulls/1/reviews":   `[{"id":5,"user":{"login":"alice"},"state":"CHANGES_REQUESTED","body":"直して"},{"id":6,"user":{"login":"bob"},"state":"PENDING","body":"draft"}]`,
		"/repos/o/r/pulls/1/comments":  `[{"id":10,"user":{"login":"alice"},"body":"ここ","path":"a.go"},{"id":11,"user":{"login":"EBIX-BOT"},"body":"返信"}]`,
		"/repos/o/r/issues/1/comments": `[{"id":20,"user":{"login":"carol"},"body":"old"}]`,
	})
	activity, cursor, err := client.ActivitySince(context.Background(), ref, Cursor{Comment: 20}, "ebix-bot")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Cursor{Review: 6, ReviewComment: 11, Comment: 20}); cursor != want {
		t.Fatalf("cursor = %+v, want %+v", cursor, want)
	}
	var got []string
	for _, item := range activity {
		got = append(got, item.Kind+":"+item.Author)
	}
	if strings.Join(got, ",") != "review:alice,review_comment:alice" {
		t.Fatalf("activity = %v, want alice's review and comment only", got)
	}
}

func TestErrorStatus(t *testing.T) {
	client := testClient(t, map[string]string{})
	if _, err := client.Pull(context.Background(), PullRef{"o", "r", 9}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Pull() error = %v, want a 404 error", err)
	}
}
