package codex

type ActivityKind uint8

const (
	ActivityCommand ActivityKind = iota + 1
	ActivityFileChange
	ActivityWebSearch
	ActivityToolCall
	ActivityPlan
)

// The fields are model or command output and are only fit for display.
type Activity struct {
	Kind        ActivityKind
	Command     string
	Paths       []string
	Query       string
	Tool        string
	Done, Total int
	Current     string
}

// File changes only arrive completed, so they cannot be reported when they
// start.
func activityFromItem(eventType string, item *eventItem) (Activity, bool) {
	switch item.Type {
	case "command_execution":
		if eventType == "item.started" && item.Command != "" {
			return Activity{Kind: ActivityCommand, Command: item.Command}, true
		}
	case "web_search":
		if eventType == "item.started" && item.Query != "" {
			return Activity{Kind: ActivityWebSearch, Query: item.Query}, true
		}
	case "mcp_tool_call":
		if eventType == "item.started" && item.Tool != "" {
			tool := item.Tool
			if item.Server != "" {
				tool = item.Server + "." + tool
			}
			return Activity{Kind: ActivityToolCall, Tool: tool}, true
		}
	case "file_change":
		if eventType == "item.completed" && len(item.Changes) > 0 {
			paths := make([]string, 0, len(item.Changes))
			for _, change := range item.Changes {
				paths = append(paths, change.Path)
			}
			return Activity{Kind: ActivityFileChange, Paths: paths}, true
		}
	case "todo_list":
		if len(item.Items) == 0 {
			return Activity{}, false
		}
		activity := Activity{Kind: ActivityPlan, Total: len(item.Items)}
		for _, todo := range item.Items {
			if todo.Completed {
				activity.Done++
			} else if activity.Current == "" {
				activity.Current = todo.Text
			}
		}
		return activity, true
	}
	return Activity{}, false
}
