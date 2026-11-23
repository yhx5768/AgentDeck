package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
		return nil, fmt.Errorf("解析 %s 失败：%w", HarnessFileName, err)
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

// DiscoverHarnessPaths returns existing scheduled_tasks.json files reachable
// from this process: the explicit env override, the process cwd, every known
// session cwd, and ~/.claude. The HTTP layer shows the list so a file in an
// unrelated project can be selected manually.
func DiscoverHarnessPaths(sessionCwds []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, strings.TrimPrefix(p, "~/"))
			}
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return
		}
		if seen[abs] {
			return
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return
		}
		seen[abs] = true
		out = append(out, abs)
	}
	add(os.Getenv("CC_WEBUI_HARNESS_TASKS"))
	if cwd, err := os.Getwd(); err == nil {
		add(filepath.Join(cwd, ".claude", HarnessFileName))
		add(filepath.Join(cwd, HarnessFileName))
	}
	for _, cwd := range sessionCwds {
		add(filepath.Join(cwd, ".claude", HarnessFileName))
		add(filepath.Join(cwd, HarnessFileName))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude", HarnessFileName))
	}
	return out
}
