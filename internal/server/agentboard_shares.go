package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Public artifact sharing.
//
// A share is an explicit capability: the public URL is /s/{token} and that
// route sits outside the gateway auth. The token is never stored. Instead:
//
//   - each share gets a random 128-bit id (hex),
//   - token = base64url(idBytes || HMAC-SHA256(key, idBytes)[:16]),
//   - the DB stores only SHA-256(token),
//   - the HMAC key lives in <data_dir>/artifact-share.key (0600).
//
// The key lets the authenticated management panel rebuild the URL from the
// share id (so copy works across restarts/browsers) while a database leak
// still does not reveal usable links. Revocation and expiry are checked on
// every public request and public responses are no-store, so they take effect
// immediately.

const (
	shareTokenMaxLen = 256
	shareKeyBytes    = 32
)

func newArtifactShareID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// artifactShareKey returns the per-data-dir HMAC key, creating it on first
// use. It is deliberately not derived from gateway_token: rotating the login
// token must not silently invalidate every already-published share.
func (s *Server) artifactShareKey() ([]byte, error) {
	s.shareKeyMu.Lock()
	defer s.shareKeyMu.Unlock()
	if len(s.shareKey) > 0 {
		return s.shareKey, nil
	}
	path := filepath.Join(s.cfg().DataDir, "artifact-share.key")
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) < shareKeyBytes {
			return nil, fmt.Errorf("分享密钥文件损坏（长度 %d）", len(data))
		}
		s.shareKey = data
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, shareKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	s.shareKey = key
	return key, nil
}

// artifactShareToken derives the public capability token for a share id.
func (s *Server) artifactShareToken(id string) (string, error) {
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) < 16 {
		return "", fmt.Errorf("invalid share id")
	}
	key, err := s.artifactShareKey()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("artifact-share:v1\x00"))
	mac.Write(raw)
	payload := make([]byte, 0, len(raw)+16)
	payload = append(payload, raw...)
	payload = append(payload, mac.Sum(nil)[:16]...)
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func artifactShareTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// artifactShareStatus is the single source of truth for active/expired/
// revoked. A malformed expires_at fails closed (treated as expired).
func artifactShareStatus(expiresAt, revokedAt string, now time.Time) string {
	if strings.TrimSpace(revokedAt) != "" {
		return "revoked"
	}
	expiresAt = strings.TrimSpace(expiresAt)
	if expiresAt == "" {
		return "active"
	}
	at, err := time.ParseInLocation("2006-01-02 15:04:05", expiresAt, time.Local)
	if err != nil || !now.Before(at) {
		return "expired"
	}
	return "active"
}

func shareExpiryTimestamp(days int) (string, bool) {
	switch days {
	case 0:
		return "", true
	case 1, 3, 7:
		return time.Now().Add(time.Duration(days) * 24 * time.Hour).Format("2006-01-02 15:04:05"), true
	default:
		return "", false
	}
}

func (s *Server) artifactShareURL(r *http.Request, token string) (string, string) {
	base, source := s.artifactBaseURL(r)
	return base + "/s/" + url.PathEscape(token), source
}

// handleAgentBoardCreateShare creates one public link for an artifact. An
// omitted/null expiry_days means permanent (0), otherwise 1/3/7 days.
func (s *Server) handleAgentBoardCreateShare(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	artifact, err := st.GetArtifact(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "产物不存在: "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取产物失败: "+err.Error())
		return
	}
	var body struct {
		ExpiryDays *int   `json:"expiry_days"`
		CreatedBy  string `json:"created_by"`
	}
	if err := decode(r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	days := 0
	if body.ExpiryDays != nil {
		days = *body.ExpiryDays
	}
	expiresAt, valid := shareExpiryTimestamp(days)
	if !valid {
		writeErr(w, http.StatusBadRequest, "expiry_days 只支持 1 / 3 / 7 / null（永久）")
		return
	}
	id, err := newArtifactShareID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成分享 id 失败")
		return
	}
	token, err := s.artifactShareToken(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成分享链接失败: "+err.Error())
		return
	}
	createdBy := strings.TrimSpace(body.CreatedBy)
	if createdBy == "" {
		createdBy = "local"
	}
	share, err := st.CreateShare(map[string]any{
		"id": id, "artifact_id": artifact.ID, "token_hash": artifactShareTokenHash(token),
		"expires_at": expiresAt, "created_by": createdBy,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "创建分享失败: "+err.Error())
		return
	}
	share["artifact_title"] = artifact.Title
	share["artifact_kind"] = artifact.Kind
	share["artifact_filename"] = artifact.Filename
	share["task_id"] = artifact.TaskID
	share["status"] = "active"
	share["active"] = true
	if link, source := s.artifactShareURL(r, token); link != "" {
		share["url"] = link
		share["url_source"] = source
		share["url_local_only"] = source == "listen"
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "share": share})
}

// handleAgentBoardListShares lists every share (including revoked/expired, so
// management can show what happened to each link) with a rebuilt public URL
// for links that are still active.
func (s *Server) handleAgentBoardListShares(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	shares, err := st.ListShares()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取分享列表失败: "+err.Error())
		return
	}
	now := time.Now()
	for _, share := range shares {
		status := artifactShareStatus(stringArg(share, "expires_at"), stringArg(share, "revoked_at"), now)
		share["status"] = status
		share["active"] = status == "active"
		if status != "active" {
			continue
		}
		id, _ := share["id"].(string)
		token, err := s.artifactShareToken(id)
		if err != nil {
			continue
		}
		if link, source := s.artifactShareURL(r, token); link != "" {
			share["url"] = link
			share["url_source"] = source
			share["url_local_only"] = source == "listen"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "shares": shares})
}

func (s *Server) handleAgentBoardRevokeShare(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	share, err := st.RevokeShare(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "分享不存在: "+id)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "取消分享失败: "+err.Error())
		return
	}
	share["status"] = "revoked"
	share["active"] = false
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "share": share})
}

// handleArtifactShare is the unauthenticated public route. It only ever serves
// the single shared artifact: no task metadata, no board listing, no token
// hints on errors.
func (s *Server) handleArtifactShare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	w.Header().Set("Cache-Control", "no-store")
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	token := strings.TrimSpace(r.PathValue("token"))
	if token == "" || len(token) > shareTokenMaxLen {
		writeErr(w, http.StatusNotFound, "分享链接不存在或已失效")
		return
	}
	share, err := st.GetShareByTokenHash(artifactShareTokenHash(token))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "分享链接不存在或已失效")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取分享失败")
		return
	}
	switch artifactShareStatus(share.ExpiresAt, share.RevokedAt, time.Now()) {
	case "revoked":
		writeErr(w, http.StatusGone, "分享链接已取消")
		return
	case "expired":
		writeErr(w, http.StatusGone, "分享链接已过期")
		return
	}
	artifact, err := st.GetArtifact(share.ArtifactID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "分享链接不存在或已失效")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取产物失败")
		return
	}
	if artifact.Kind == "url" {
		if strings.TrimSpace(artifact.URL) == "" {
			writeErr(w, http.StatusNotFound, "分享链接不存在或已失效")
			return
		}
		http.Redirect(w, r, artifact.URL, http.StatusFound)
		return
	}
	s.serveArtifactFile(w, r, artifact, true)
}
