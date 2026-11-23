package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sentinel errors let the HTTP layer distinguish an empty file from a missing
// or malformed one without treating all of them as hard failures.
var (
	ErrHarnessNotExist    = errors.New("harness 文件不存在")
	ErrHarnessInvalidJSON = errors.New("harness 文件不是合法 JSON")
)

// HarnessTask is one entry in Claude Code's .claude/scheduled_tasks.json
// (CronCreate). cc-webui treats that file as read-only: it can list and import
// entries, but never edits or deletes them.
type HarnessTask struct {
	ID        string    `json:"id"`
	Cron      string    `json:"cron"`
	Prompt    string    `json:"prompt"`
	Recurring bool      `json:"recurring"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// HarnessFileName is the only file name the harness reader will open. The path
// is user-configurable (the file lives in each project's .claude/), but
// restricting the basename keeps the authenticated endpoint from becoming an
// arbitrary-file reader.
const HarnessFileName = "scheduled_tasks.json"

type harnessJSONTask struct {
	ID        string `json:"id"`
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	Recurring bool   `json:"recurring"`
	CreatedAt int64  `json:"createdAt"`
}

type harnessJSONFile struct {
	Tasks []harnessJSONTask `json:"tasks"`
}

// ParseHarness reads both the documented {"tasks":[…]} shape and a bare array.
// Missing `recurring` means one-shot, matching CronCreate's own default.
func ParseHarness(data []byte) ([]HarnessTask, error) {
	var f harnessJSONFile
	if err := json.Unmarshal(data, &f); err == nil && f.Tasks != nil {
		return convertHarness(f.Tasks), nil
	}
	var arr []harnessJSONTask
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("%w：%v", ErrHarnessInvalidJSON, err)
	}
	return convertHarness(arr), nil
}

func convertHarness(in []harnessJSONTask) []HarnessTask {
	out := make([]HarnessTask, 0, len(in))
	for _, t := range in {
		ht := HarnessTask{
			ID: strings.TrimSpace(t.ID), Cron: strings.TrimSpace(t.Cron),
			Prompt: strings.TrimSpace(t.Prompt), Recurring: t.Recurring,
		}
		if t.CreatedAt > 0 {
			ht.CreatedAt = time.UnixMilli(t.CreatedAt)
		}
		out = append(out, ht)
	}
	return out
}

// ReadHarnessFile reads and parses one scheduled_tasks.json. The basename is
// enforced here as well as at the HTTP layer so no caller can point it at an
// arbitrary file.
func ReadHarnessFile(path string) ([]HarnessTask, error) {
	if filepath.Base(path) != HarnessFileName {
		return nil, fmt.Errorf("只支持读取名为 %s 的文件", HarnessFileName)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w：%s", ErrHarnessNotExist, path)
		}
		return nil, err
	}
	return ParseHarness(raw)
}

// HarnessName derives a short display name from the prompt's first line. The
// harness file has no name field, and its prompts often start with a long
// instruction, so cut at the first sentence/clause boundary.
func HarnessName(t HarnessTask) string {
	var line string
	for _, ln := range strings.Split(t.Prompt, "\n") {
		if strings.TrimSpace(ln) != "" {
			line = strings.TrimSpace(ln)
			break
		}
	}
	if line != "" {
		for _, sep := range []string{"。", "；", ";", "：", ":", "，", ",", "（", "("} {
			if i := strings.Index(line, sep); i > 0 {
				line = strings.TrimSpace(line[:i])
			}
		}
	}
	if line != "" {
		r := []rune(line)
		if len(r) > 40 {
			r = r[:40]
		}
		return string(r)
	}
	if t.ID != "" {
		return "Harness 任务 " + t.ID
	}
	return "Harness 定时任务"
}

// WorkspacePaths is one project's harness file, grouped by workspace root so
// the UI can show "this project · N tasks".
type WorkspacePaths struct {
	Workspace string   `json:"workspace"`
	Paths     []string `json:"paths"`
}

// WorkspaceRoot maps a .claude/scheduled_tasks.json path to the workspace it
// belongs to (the directory containing .claude).
func WorkspaceRoot(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == ".claude" {
		return filepath.Dir(dir)
	}
	return dir
}

// DiscoverHarnessGroups finds existing harness files reachable from this
// process: CC_WEBUI_HARNESS_TASKS (a filepath.ListSeparator-separated list),
// the process cwd, every known session cwd, and ~/.claude. Results are grouped
// by workspace root, paths deduplicated, and only regular files are returned.
// Imported tasks' source paths are fed in by the server so their workspace
// groups survive even when the session cwd is gone.
func DiscoverHarnessGroups(sessionCwds, storedPaths []string) []WorkspacePaths {
	var out []WorkspacePaths
	idx := map[string]int{}   // workspace -> index in out
	seen := map[string]bool{} // absolute file path
	add := func(workspace, p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return
			}
			p = filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
		abs, err := filepath.Abs(p)
		if err != nil || seen[abs] || filepath.Base(abs) != HarnessFileName {
			return
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return
		}
		seen[abs] = true
		if workspace == "" {
			workspace = WorkspaceRoot(abs)
		}
		workspace = filepath.Clean(workspace)
		i, ok := idx[workspace]
		if !ok {
			i = len(out)
			idx[workspace] = i
			out = append(out, WorkspacePaths{Workspace: workspace})
		}
		out[i].Paths = append(out[i].Paths, abs)
	}
	for _, p := range filepath.SplitList(os.Getenv("CC_WEBUI_HARNESS_TASKS")) {
		add("", p)
	}
	if cwd, err := os.Getwd(); err == nil {
		add(cwd, filepath.Join(cwd, ".claude", HarnessFileName))
		add(cwd, filepath.Join(cwd, HarnessFileName))
	}
	for _, cwd := range sessionCwds {
		if strings.TrimSpace(cwd) == "" {
			continue
		}
		add(cwd, filepath.Join(cwd, ".claude", HarnessFileName))
		add(cwd, filepath.Join(cwd, HarnessFileName))
	}
	for _, p := range storedPaths {
		add("", p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add("", filepath.Join(home, ".claude", HarnessFileName))
	}
	return out
}

// DiscoverHarnessPaths flattens groups into a path list (kept for callers that
// only need the first file).
func DiscoverHarnessPaths(sessionCwds []string) []string {
	var out []string
	for _, g := range DiscoverHarnessGroups(sessionCwds, nil) {
		out = append(out, g.Paths...)
	}
	return out
}
