package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/opencodeapi"
	"cc-webui/internal/store"
)

// RewindResult describes what a rewind actually changed, in the shape the
// existing claude rewind endpoint already returns to the frontend.
type RewindResult struct {
	// FilesChanged is the number of files opencode restored.
	FilesChanged int `json:"files_changed"`
	// Files is a per-file change list (path + added/removed line counts) for
	// the "what did the rewind touch" panel.
	Files []RewindFile `json:"files"`
}

// RewindFile is one file touched by an opencode revert.
type RewindFile struct {
	Path    string `json:"path"`
	Hash    string `json:"hash,omitempty"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Time    int64  `json:"time,omitempty"`
}

// RewindOpenCode rolls an opencode session back to a message, restoring files
// through opencode's own revert surface (stage → commit).
//
// This is what the old one-shot engine could not do at all: `opencode run`
// exposed no rollback command, so opencode sessions were not rewindable on
// either axis. The serve surface has both — POST /api/session/{id}/revert/stage
// previews the revert and reports the affected file set, and
// /revert/commit applies it.
//
// messageID is the opencode message id (msg_…) of the turn to rewind to.
// Passing an empty id rewinds to the session's current start.
func (m *Manager) RewindOpenCode(sess *store.Session, cli *config.CLI, prov *config.Provider, messageID string) (*RewindResult, error) {
	if sess.ClaudeSessionID == "" {
		return nil, errors.New("会话还没有 opencode session id")
	}
	model := prov.EffectiveModel(sess.ModelOverride)

	// A rewind runs outside any turn, so it acquires a serve process the same
	// way a turn does and releases it right after.
	srv, _, release, err := m.ocPool.acquire(context.Background(), cli, prov, model, sess.Cwd)
	if err != nil {
		return nil, err
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Stage first: it is the read-only half and tells us whether a revert is
	// even possible before anything on disk changes.
	staged, err := srv.StageRevert(ctx, sess.ClaudeSessionID, opencodeapi.RevertInput{
		MessageID: messageID,
		Files:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("opencode 预览回滚失败：%w", err)
	}
	if err := srv.CommitRevert(ctx, sess.ClaudeSessionID); err != nil {
		return nil, fmt.Errorf("opencode 执行回滚失败：%w", err)
	}
	res := &RewindResult{FilesChanged: len(staged.Files)}
	for _, f := range staged.Files {
		res.Files = append(res.Files, RewindFile{
			Path: f.Path, Hash: f.Hash, Time: f.Time,
		})
	}
	return res, nil
}

// OpenCodeRewindSupported reports whether an engine can rewind at all. Used
// by the server to refuse up front and by the frontend to decide whether to
// offer the action.
func OpenCodeRewindSupported(engine string) bool {
	return engine == config.KindClaude || engine == config.KindOpenCode
}
