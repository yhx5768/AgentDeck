package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func publishShareTestArtifact(t *testing.T, s *Server, taskID string, payload map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(payload)
	req := artifactReq(http.MethodPost, "http://deck.local/api/agent-board/tasks/"+taskID+"/artifacts", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", taskID)
	rec := httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish artifact = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Artifact map[string]any `json:"artifact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Artifact
}

func createShareTestLink(t *testing.T, s *Server, artifactID string, expiry any, want int) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"expiry_days": expiry, "created_by": "tester"})
	req := artifactReq(http.MethodPost, "http://deck.local/api/agent-board/artifacts/"+artifactID+"/shares", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", artifactID)
	rec := httptest.NewRecorder()
	s.handleAgentBoardCreateShare(rec, req)
	if rec.Code != want {
		t.Fatalf("create share = %d body=%s", rec.Code, rec.Body.String())
	}
	if want != http.StatusCreated {
		return nil
	}
	var out struct {
		Share map[string]any `json:"share"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Share
}

func publicShareRequest(token string) *http.Request {
	req := artifactReq(http.MethodGet, "http://deck.local/s/"+token, nil)
	req.SetPathValue("token", token)
	return req
}

func shareTokenFromURL(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Path == "" {
		t.Fatalf("bad share url %q: %v", raw, err)
	}
	token := path.Base(parsed.Path)
	if len(token) < 43 || strings.ContainsAny(token, "=+/") {
		t.Fatalf("share token is not URL-safe 128-bit+ base64url: %q", token)
	}
	return token
}

func TestArtifactShareCreateListPublicAndRevoke(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)
	artifact := publishShareTestArtifact(t, s, "T-1", map[string]any{
		"title": "公开验收报告", "kind": "html", "filename": "share.html",
		"content": "<!doctype html><html><body><h1>shared</h1></body></html>",
	})
	artifactID, _ := artifact["id"].(string)

	// 1 day expiry: create returns a copyable public URL.
	share := createShareTestLink(t, s, artifactID, 1, http.StatusCreated)
	if share["status"] != "active" || share["active"] != true {
		t.Fatalf("share status = %+v", share)
	}
	if share["created_by"] != "tester" {
		t.Fatalf("created_by = %v", share["created_by"])
	}
	if exp, _ := share["expires_at"].(string); exp == "" {
		t.Fatalf("expires_at empty for 1-day share: %+v", share)
	}
	link, _ := share["url"].(string)
	if !strings.HasPrefix(link, "http://deck.local/s/") {
		t.Fatalf("share url = %q", link)
	}
	token := shareTokenFromURL(t, link)

	// The HMAC key is private to the data dir (0600).
	keyInfo, err := os.Stat(filepath.Join(s.cfg().DataDir, "artifact-share.key"))
	if err != nil {
		t.Fatalf("share key: %v", err)
	}
	if keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("share key mode = %v", keyInfo.Mode().Perm())
	}

	// Public route: no auth, serves the artifact with sandbox/noindex/no-store.
	rec := httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("public share = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<h1>shared</h1>") {
		t.Fatalf("public body = %s", rec.Body.String())
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("public csp = %q", csp)
	}
	if rec.Header().Get("X-Robots-Tag") != "noindex, nofollow, noarchive" {
		t.Fatalf("robots = %q", rec.Header().Get("X-Robots-Tag"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}

	// Download variant.
	dl := publicShareRequest(token)
	dl.URL.RawQuery = "download=1"
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, dl)
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("public download disposition = %q", cd)
	}

	// Authenticated management list rebuilds the same URL.
	req := artifactReq(http.MethodGet, "http://deck.local/api/agent-board/shares", nil)
	rec = httptest.NewRecorder()
	s.handleAgentBoardListShares(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list shares = %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Shares []map[string]any `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Shares) != 1 {
		t.Fatalf("shares = %+v", listed.Shares)
	}
	got := listed.Shares[0]
	if got["url"] != link || got["status"] != "active" || got["artifact_title"] != "公开验收报告" {
		t.Fatalf("listed share = %+v", got)
	}
	if got["task_id"] != "T-1" {
		t.Fatalf("listed task id = %v", got["task_id"])
	}

	// Revoke: immediate 410 and the list keeps the row as revoked without URL.
	id, _ := share["id"].(string)
	req = artifactReq(http.MethodDelete, "http://deck.local/api/agent-board/shares/"+id, nil)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	s.handleAgentBoardRevokeShare(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(token))
	if rec.Code != http.StatusGone {
		t.Fatalf("revoked public share = %d", rec.Code)
	}
	req = artifactReq(http.MethodGet, "http://deck.local/api/agent-board/shares", nil)
	rec = httptest.NewRecorder()
	s.handleAgentBoardListShares(rec, req)
	var revokedList struct {
		Shares []map[string]any `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &revokedList); err != nil {
		t.Fatal(err)
	}
	if len(revokedList.Shares) != 1 || revokedList.Shares[0]["status"] != "revoked" ||
		revokedList.Shares[0]["active"] != false || revokedList.Shares[0]["url"] != nil {
		t.Fatalf("revoked list = %+v", revokedList.Shares)
	}

	// Unknown tokens and ids never leak board data.
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token = %d", rec.Code)
	}
	req = artifactReq(http.MethodDelete, "http://deck.local/api/agent-board/shares/nope", nil)
	req.SetPathValue("id", "nope")
	rec = httptest.NewRecorder()
	s.handleAgentBoardRevokeShare(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown revoke = %d", rec.Code)
	}
}

func TestArtifactSharePermanentExpiredAndURLKinds(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)
	artifact := publishShareTestArtifact(t, s, "T-1", map[string]any{
		"title": "永久报告", "kind": "markdown", "filename": "perm.md", "content": "# perm\n",
	})
	artifactID, _ := artifact["id"].(string)

	// null expiry = permanent (no expires_at).
	share := createShareTestLink(t, s, artifactID, nil, http.StatusCreated)
	if exp, _ := share["expires_at"].(string); exp != "" {
		t.Fatalf("permanent expires_at = %q", exp)
	}
	token := shareTokenFromURL(t, share["url"].(string))
	rec := httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(token))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "# perm") {
		t.Fatalf("permanent public share = %d %q", rec.Code, rec.Body.String())
	}

	// Expired share: seed the row with a past expires_at through the store.
	id, _ := newArtifactShareID()
	expiredToken, err := s.artifactShareToken(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.boardDB.CreateShare(map[string]any{
		"id": id, "artifact_id": artifactID, "token_hash": artifactShareTokenHash(expiredToken),
		"expires_at": "2020-01-01 00:00:00", "created_by": "tester",
	}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(expiredToken))
	if rec.Code != http.StatusGone {
		t.Fatalf("expired public share = %d %s", rec.Code, rec.Body.String())
	}

	// Invalid expiry values are rejected.
	createShareTestLink(t, s, artifactID, 2, http.StatusBadRequest)
	createShareTestLink(t, s, artifactID, -1, http.StatusBadRequest)

	// External URL artifacts redirect on the public route.
	urlArtifact := publishShareTestArtifact(t, s, "T-1", map[string]any{
		"title": "外部链接", "kind": "url", "filename": "link", "url": "https://example.com/design",
	})
	urlID, _ := urlArtifact["id"].(string)
	urlShare := createShareTestLink(t, s, urlID, 7, http.StatusCreated)
	urlToken := shareTokenFromURL(t, urlShare["url"].(string))
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(urlToken))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://example.com/design" {
		t.Fatalf("url share = %d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("X-Robots-Tag") == "" {
		t.Fatal("url share missing noindex")
	}
}

func TestPublicShareRouteBypassesAuth(t *testing.T) {
	s, _ := boardMCPServer(t)
	s.cm.Get().GatewayToken = "secret"
	s.mux = http.NewServeMux()
	s.routes()

	// /s/{token} is outside the gateway auth: an unknown token reaches the
	// share handler and returns 404 (not 401).
	req := httptest.NewRequest(http.MethodGet, "http://deck.local/s/unknown-token", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("public unknown share = %d body=%s", rec.Code, rec.Body.String())
	}

	// Neighbouring board API routes remain authenticated.
	req = httptest.NewRequest(http.MethodGet, "http://deck.local/api/agent-board/shares", nil)
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected shares list = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestArtifactShareRevokeIsolatedAndListProject(t *testing.T) {
	s, _ := boardMCPServer(t)
	if _, err := s.boardDB.CreateTask(map[string]any{
		"id": "T-P", "title": "项目任务", "project": "演示项目",
	}); err != nil {
		t.Fatal(err)
	}
	artifact := publishShareTestArtifact(t, s, "T-P", map[string]any{
		"title": "项目产物", "kind": "html", "filename": "p.html", "content": "<p>p</p>",
	})
	artifactID, _ := artifact["id"].(string)
	first := createShareTestLink(t, s, artifactID, nil, http.StatusCreated)
	second := createShareTestLink(t, s, artifactID, 7, http.StatusCreated)

	// Revoking one link must not touch another link on the same artifact.
	id, _ := first["id"].(string)
	req := artifactReq(http.MethodDelete, "http://deck.local/api/agent-board/shares/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleAgentBoardRevokeShare(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	secondToken := shareTokenFromURL(t, second["url"].(string))
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(secondToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("second share after revoking first = %d %s", rec.Code, rec.Body.String())
	}

	// Management rows expose the task's project for the page filter.
	req = artifactReq(http.MethodGet, "http://deck.local/api/agent-board/shares", nil)
	rec = httptest.NewRecorder()
	s.handleAgentBoardListShares(rec, req)
	var listed struct {
		Shares []map[string]any `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Shares) != 2 {
		t.Fatalf("shares = %+v", listed.Shares)
	}
	for _, share := range listed.Shares {
		if share["project"] != "演示项目" || share["task_id"] != "T-P" {
			t.Fatalf("listed share = %+v", share)
		}
	}
}
