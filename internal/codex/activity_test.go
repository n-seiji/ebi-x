package codex

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseJSONLReportsActivity(t *testing.T) {
	output := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"item.started","item":{"id":"1","type":"todo_list","items":[{"text":"調べる","completed":false},{"text":"直す","completed":false}]}}`,
		`{"type":"item.started","item":{"id":"2","type":"command_execution","command":"bash -lc 'go test ./...'","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"2","type":"command_execution","command":"bash -lc 'go test ./...'","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"3","type":"file_change","changes":[{"path":"/w/a.go","kind":"update"},{"path":"/w/b.go","kind":"add"}],"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"4","type":"web_search","query":"slack api"}}`,
		`{"type":"item.started","item":{"id":"5","type":"mcp_tool_call","server":"gh","tool":"search","status":"in_progress"}}`,
		`{"type":"item.updated","item":{"id":"1","type":"todo_list","items":[{"text":"調べる","completed":true},{"text":"直す","completed":false}]}}`,
		`{"type":"item.started","item":{"id":"6","type":"reasoning","text":"thinking"}}`,
		`{"type":"item.completed","item":{"id":"7","type":"agent_message","text":"done"}}`,
		`{"type":"turn.completed"}`,
	}, "\n")
	var got TurnResult
	var activities []Activity
	if err := parseJSONL(strings.NewReader(output), &got, nil, func(activity Activity) {
		activities = append(activities, activity)
	}); err != nil {
		t.Fatalf("parseJSONL() error = %v", err)
	}
	want := []Activity{
		{Kind: ActivityPlan, Total: 2, Current: "調べる"},
		{Kind: ActivityCommand, Command: "bash -lc 'go test ./...'"},
		{Kind: ActivityFileChange, Paths: []string{"/w/a.go", "/w/b.go"}},
		{Kind: ActivityWebSearch, Query: "slack api"},
		{Kind: ActivityToolCall, Tool: "gh.search"},
		{Kind: ActivityPlan, Done: 1, Total: 2, Current: "直す"},
	}
	if !reflect.DeepEqual(activities, want) {
		t.Fatalf("activities = %#v, want %#v", activities, want)
	}
	if !got.Completed || !reflect.DeepEqual(got.Messages, []string{"done"}) {
		t.Fatalf("result = %#v", got)
	}
}
