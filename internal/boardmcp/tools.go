package boardmcp

// Tool is one MCP tool definition (the subset the stdio server advertises).
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func schema(required []string, properties map[string]any) map[string]any {
	out := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integer(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func boolean(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func stringArray(description string) map[string]any {
	return map[string]any{
		"type": "array", "items": map[string]any{"type": "string"},
		"description": description,
	}
}

var taskCreateProperties = map[string]any{
	"id":          str("Stable task ID (optional; generated when omitted)"),
	"display_id":  str("Human-facing/card ID"),
	"icafe_id":    str("Optional external card ID; also fills display_id"),
	"url":         str("Optional external card URL"),
	"title":       str("Task title"),
	"description": str("Task description"),
	"status":      str("Initial status (default 新建)"),
	"project":     str("Project name"),
	"owner":       str("Owner"),
	"worker":      str("Worker/assignee peer name"),
	"session":     str("Worker session id"),
	"cwd":         str("Worker working directory"),
	"source":      str("Source label (default local)"),
	"tags":        stringArray("Tag list"),
	"priority":    integer("Priority (default 2)"),
	"progress":    str("Progress summary"),
	"last_event":  str("Last event label"),
	"report_file": str("Report file path"),
}

var taskUpdateProperties = map[string]any{
	"task_id":         str("Task ID"),
	"title":           str("New title"),
	"status":          str("New status"),
	"status_reason":   str("Reason shown in the board overview"),
	"project":         str("Project name"),
	"owner":           str("Owner"),
	"description":     str("Task description"),
	"progress":        str("Progress summary"),
	"worker":          str("Worker/assignee peer name"),
	"session":         str("Worker session id"),
	"cwd":             str("Worker working directory"),
	"report_file":     str("Report file path"),
	"last_event":      str("Last event label"),
	"tags":            stringArray("Replace the tag list"),
	"priority":        integer("Priority"),
	"archived":        boolean("Archive the task"),
	"progress_locked": boolean("Protect progress from automatic overwrites"),
}

var Tools = []Tool{
	{
		Name:        "board_task_create",
		Description: "Create one task on the AgentDeck task board.",
		InputSchema: schema([]string{"title"}, taskCreateProperties),
	},
	{
		Name:        "board_task_update",
		Description: "Update a task's status, progress, owner or routing metadata on the AgentDeck task board.",
		InputSchema: schema([]string{"task_id"}, taskUpdateProperties),
	},
	{
		Name:        "board_task_event",
		Description: "Append an event to a task timeline. Repeated kind+at+text events are idempotent.",
		InputSchema: schema([]string{"task_id", "kind", "text"}, map[string]any{
			"task_id": str("Task ID"),
			"kind":    str("Event kind, e.g. 用户消息/主 Agent 回复/A2A 发/A2A 收/最近回报"),
			"text":    str("Event text"),
			"at":      str("Event time (YYYY-MM-DD HH:MM:SS; defaults to now)"),
			"source":  str("Event source (default mcp)"),
		}),
	},
	{
		Name:        "board_task_bind",
		Description: "Bind a worker peer, session id and cwd to a task.",
		InputSchema: schema([]string{"task_id", "worker"}, map[string]any{
			"task_id": str("Task ID"),
			"worker":  str("Worker/assignee peer name"),
			"session": str("Worker session id"),
			"cwd":     str("Worker working directory"),
		}),
	},
	{
		Name:        "board_task_get",
		Description: "Read one task, including its timeline.",
		InputSchema: schema([]string{"task_id"}, map[string]any{
			"task_id": str("Task ID"),
		}),
	},
	{
		Name:        "board_task_list",
		Description: "List tasks, optionally filtered by status, project or worker; use summary for counts only.",
		InputSchema: schema(nil, map[string]any{
			"status":  str("Filter by status"),
			"project": str("Filter by project"),
			"worker":  str("Filter by worker/assignee"),
			"summary": boolean("Return only task count/counts/projects"),
			"limit":   integer("Maximum tasks to return"),
		}),
	},
	{
		Name:        "board_worker_list",
		Description: "List workers and their bound session/cwd metadata.",
		InputSchema: schema(nil, map[string]any{}),
	},
	{
		Name:        "board_snapshot",
		Description: "Return the full AgentDeck board snapshot (tasks, counts, projects).",
		InputSchema: schema(nil, map[string]any{}),
	},
	{
		Name:        "board_backup",
		Description: "Create a server-side SQLite backup of the AgentDeck board.",
		InputSchema: schema(nil, map[string]any{}),
	},
}
