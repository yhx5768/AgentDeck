package opencodeapi

import (
	"context"
	"fmt"
	"net/http"
)

// PermissionRequest is one outstanding approval prompt as opencode models it.
// Note the payload lives under `properties.data` on the SSE event but is the
// bare object in the REST list response — see EventPayload.
type PermissionRequest struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Permission string         `json:"permission"`
	Patterns   []string       `json:"patterns"`
	CallID     string         `json:"callID"`
	Message    string         `json:"message"`
	Time       int64          `json:"time"`
	Title      string         `json:"title"`
	Metadata   map[string]any `json:"metadata"`
	// Type/PluginID/Tool carry the tool identity for rule matching. Varies by
	// permission kind, so kept loose.
	Type     string         `json:"type"`
	PluginID string         `json:"pluginID"`
	Tool     map[string]any `json:"tool"`
}

// QuestionRequest is one outstanding "the model is asking you" prompt.
type QuestionRequest struct {
	ID        string           `json:"id"`
	SessionID string           `json:"sessionID"`
	Questions []QuestionPrompt `json:"questions"`
	Tool      map[string]any   `json:"tool"`
	Time      int64            `json:"time"`
}

// QuestionPrompt is one question in a QuestionRequest. This is the wire shape
// of opencode's built-in `question` tool and lines up almost 1:1 with Claude's
// AskUserQuestion input, so cc-webui's existing card renders it unchanged.
type QuestionPrompt struct {
	Question string           `json:"question"`
	Header   string           `json:"header,omitempty"`
	Options  []QuestionOption `json:"options,omitempty"`
	Multiple bool             `json:"multiple,omitempty"`
	Custom   bool             `json:"custom,omitempty"`
}

// QuestionOption is one selectable answer.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// PermissionReply is the body of the permission reply endpoint.
type PermissionReply struct {
	Reply   string `json:"reply,omitempty"` // "once" | "always" | "reject"
	Message string `json:"message,omitempty"`
}

// QuestionReply is the body of the question reply endpoint. `answers` is one
// array of labels per question, in the same order as the questions.
type QuestionReply struct {
	Answers [][]string `json:"answers"`
}

// ListPermissionRequests returns pending approvals across all sessions — the
// reconnect catch-up that mirrors cc-webui's own /permissions/pending.
func (s *Server) ListPermissionRequests(ctx context.Context) ([]PermissionRequest, error) {
	var out []PermissionRequest
	err := s.do(ctx, http.MethodGet, "/api/permission/request", nil, &out)
	return out, err
}

// ListSessionPermissionRequests returns pending approvals for one session.
func (s *Server) ListSessionPermissionRequests(ctx context.Context, sessionID string) ([]PermissionRequest, error) {
	var out []PermissionRequest
	err := s.do(ctx, http.MethodGet,
		queryPath(fmt.Sprintf("/api/session/%s/permission", sessionID), nil), nil, &out)
	return out, err
}

// ReplyPermission answers a pending approval.
//
// The requests cc-webui bridges are the v1 `permission.asked` events, whose
// reply endpoint is the flat /permission route; the v2
// /api/session/{id}/permission/{rid}/reply route 404s a v1 request (verified
// live: v2 → 404, v1 → 200 and the tool resumes). `directory` scopes the reply
// to the session's project exactly as create-session does.
//
// Caveat worth knowing before using "always": opencode appends the rule to
// the running server process's in-memory approved list. It is NOT persisted,
// it is scoped to the whole server instance for that directory rather than to
// one session, and the pattern comes from the tool, not from us. Persisting a
// durable grant goes through /api/permission/saved instead.
func (s *Server) ReplyPermission(ctx context.Context, directory, requestID string, r PermissionReply) error {
	path := queryPath(fmt.Sprintf("/permission/%s/reply", requestID),
		map[string]any{"directory": directory})
	return s.do(ctx, http.MethodPost, path, r, nil)
}

// ListQuestionRequests returns pending user questions across all sessions.
func (s *Server) ListQuestionRequests(ctx context.Context) ([]QuestionRequest, error) {
	var out []QuestionRequest
	err := s.do(ctx, http.MethodGet, "/api/question/request", nil, &out)
	return out, err
}

// ReplyQuestion delivers answers to a pending question and unblocks the turn.
// Like ReplyPermission, the question.asked events cc-webui handles are the v1
// kind, so the reply goes to the flat /question route.
func (s *Server) ReplyQuestion(ctx context.Context, directory, requestID string, r QuestionReply) error {
	path := queryPath(fmt.Sprintf("/question/%s/reply", requestID),
		map[string]any{"directory": directory})
	return s.do(ctx, http.MethodPost, path, r, nil)
}

// RejectQuestion refuses to answer; the tool returns "Unanswered" and the
// model decides what to do next.
func (s *Server) RejectQuestion(ctx context.Context, directory, requestID string) error {
	path := queryPath(fmt.Sprintf("/question/%s/reject", requestID),
		map[string]any{"directory": directory})
	return s.do(ctx, http.MethodPost, path, struct{}{}, nil)
}

// ---- revert (rewind) ----

// RevertInput is the body of POST /api/session/{id}/revert/stage.
type RevertInput struct {
	MessageID string `json:"messageID,omitempty"`
	Files     bool   `json:"files,omitempty"`
}

// RevertState is what staging a revert returns: the snapshot identifier, the
// unified diff that will be applied, and the per-file change set. This is what
// makes opencode sessions rewindable on both axes (messages and files), where
// the old `run`-based engine had neither.
type RevertState struct {
	Snapshot string         `json:"snapshot"`
	Diff     string         `json:"diff"`
	Files    []RevertFile   `json:"files"`
	Revert   bool           `json:"revert"`
	Meta     map[string]any `json:"meta"`
}

// RevertFile is one file touched by a staged revert.
type RevertFile struct {
	Path   string `json:"path"`
	Before string `json:"before"`
	After  string `json:"after"`
	Hash   string `json:"hash"`
	Time   int64  `json:"time"`
}

// StageRevert previews a rewind without applying it.
func (s *Server) StageRevert(ctx context.Context, sessionID string, in RevertInput) (*RevertState, error) {
	var out RevertState
	err := s.do(ctx, http.MethodPost,
		queryPath(fmt.Sprintf("/api/session/%s/revert/stage", sessionID), nil), in, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CommitRevert applies the staged rewind.
func (s *Server) CommitRevert(ctx context.Context, sessionID string) error {
	return s.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/session/%s/revert/commit", sessionID), struct{}{}, nil)
}

// ClearRevert drops a staged rewind without applying it.
func (s *Server) ClearRevert(ctx context.Context, sessionID string) error {
	return s.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/session/%s/revert/clear", sessionID), struct{}{}, nil)
}
