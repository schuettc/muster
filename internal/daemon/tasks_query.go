package daemon

import (
	"fmt"

	"github.com/schuettc/muster/internal/store"
)

var defaultTaskStatuses = []string{"open", "claimed", "needs_info", "blocked"}

type listTasksRequest struct {
	Query   store.TaskQuery
	Project string
	Limit   int
}

func parseListTasksArgs(args map[string]any) (listTasksRequest, error) {
	req := listTasksRequest{
		Query: store.TaskQuery{
			FromAgent: str(args, "from"),
			ToKind:    str(args, "to_kind"),
			ToTarget:  str(args, "to_target"),
		},
		Project: str(args, "project"),
		Limit:   100,
	}
	if raw, exists := args["statuses"]; exists {
		switch values := raw.(type) {
		case []any:
			for _, value := range values {
				status, ok := value.(string)
				if !ok {
					return listTasksRequest{}, fmt.Errorf("list_tasks: statuses must contain strings")
				}
				req.Query.Statuses = append(req.Query.Statuses, status)
			}
		case []string:
			req.Query.Statuses = append(req.Query.Statuses, values...)
		default:
			return listTasksRequest{}, fmt.Errorf("list_tasks: statuses must be an array")
		}
	}
	if len(req.Query.Statuses) == 0 {
		req.Query.Statuses = append([]string(nil), defaultTaskStatuses...)
	}
	for _, status := range req.Query.Statuses {
		if !store.TaskStates[status] {
			return listTasksRequest{}, fmt.Errorf("list_tasks: invalid status %q", status)
		}
	}
	if req.Query.ToKind != "" && req.Query.ToKind != "agent" && req.Query.ToKind != "role" && req.Query.ToKind != "broadcast" {
		return listTasksRequest{}, fmt.Errorf("list_tasks: invalid to_kind %q", req.Query.ToKind)
	}
	if req.Query.ToTarget != "" && req.Query.ToKind == "" {
		return listTasksRequest{}, fmt.Errorf("list_tasks: to_target requires to_kind")
	}
	if _, exists := args["limit"]; exists {
		req.Limit = int(i64(args, "limit"))
		if req.Limit < 1 || req.Limit > 500 {
			return listTasksRequest{}, fmt.Errorf("list_tasks: limit must be between 1 and 500")
		}
	}
	return req, nil
}

func finishTaskList(tasks []store.Thread, agents []store.Agent, req listTasksRequest) ([]store.Thread, bool) {
	if req.Project != "" {
		aliasProjects := make(map[string]string, len(agents))
		for _, agent := range agents {
			aliasProjects[agent.Alias] = agent.Project
		}
		filtered := make([]store.Thread, 0, len(tasks))
		for _, task := range tasks {
			if taskTouchesProject(task, aliasProjects, req.Project) {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
	}
	truncated := len(tasks) > req.Limit
	if truncated {
		tasks = tasks[:req.Limit]
	}
	if tasks == nil {
		tasks = []store.Thread{}
	}
	return tasks, truncated
}

func taskTouchesProject(task store.Thread, aliasProjects map[string]string, project string) bool {
	if task.OriginProject == project || aliasProjects[task.FromAgent] == project || aliasProjects[task.LastFrom] == project {
		return true
	}
	return task.ToKind == "agent" && aliasProjects[task.ToTarget] == project
}
