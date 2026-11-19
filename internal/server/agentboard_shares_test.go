package server

import (
	"bytes"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	req = httptest.NewRequest(http.MethodPost, "http://deck.local/api/agent-board/shares/abc/renew", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected share renew = %d body=%s", rec.Code, rec.Body.String())
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

func visitorCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == shareVisitorCookie {
			return cookie
		}
	}
	return nil
}

func listedShare(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	req := artifactReq(http.MethodGet, "http://deck.local/api/agent-board/shares", nil)
	rec := httptest.NewRecorder()
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
	for _, share := range listed.Shares {
		if share["id"] == id {
			return share
		}
	}
	t.Fatalf("share %s not listed: %+v", id, listed.Shares)
	return nil
}

func sharePVUV(t *testing.T, share map[string]any) (int, int) {
	t.Helper()
	pv, ok := share["pv"].(float64)
	if !ok {
		t.Fatalf("share pv missing/not a number: %+v", share)
	}
	uv, ok := share["uv"].(float64)
	if !ok {
		t.Fatalf("share uv missing/not a number: %+v", share)
	}
	return int(pv), int(uv)
}

func wantSharePVUV(t *testing.T, s *Server, id string, pv, uv int) {
	t.Helper()
	gotPV, gotUV := sharePVUV(t, listedShare(t, s, id))
	if gotPV != pv || gotUV != uv {
		t.Fatalf("share pv/uv = %d/%d, want %d/%d", gotPV, gotUV, pv, uv)
	}
}

// TestArtifactShareVisitPVUV covers the public-route counting contract:
// successful opens (including URL redirects) count PV; distinct anonymous
// visitors count UV; 404/410 responses count nothing; only salted hashes are
// persisted.
func TestArtifactShareVisitPVUV(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)
	artifact := publishShareTestArtifact(t, s, "T-1", map[string]any{
		"title": "PV 报告", "kind": "html", "filename": "pvuv.html",
		"content": "<!doctype html><html><body><h1>pvuv</h1></body></html>",
	})
	artifactID, _ := artifact["id"].(string)
	share := createShareTestLink(t, s, artifactID, nil, http.StatusCreated)
	id, _ := share["id"].(string)
	token := shareTokenFromURL(t, share["url"].(string))

	// A fresh share reports 0/0 in both the create response and the list.
	if pv, uv := sharePVUV(t, share); pv != 0 || uv != 0 {
		t.Fatalf("new share pv/uv = %d/%d, want 0/0", pv, uv)
	}
	wantSharePVUV(t, s, id, 0, 0)

	// First open: no cookie yet, so the request falls back to the IP+UA bucket
	// and gets a one-year visitor cookie. PV and UV both go to 1.
	req := publicShareRequest(token)
	req.Header.Set("User-Agent", "PVUV-test-agent")
	rec := httptest.NewRecorder()
	s.handleArtifactShare(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first open = %d %s", rec.Code, rec.Body.String())
	}
	cookie := visitorCookieFrom(t, rec)
	if cookie == nil {
		t.Fatal("first open did not set the visitor cookie")
	}
	if !cookie.HttpOnly || cookie.Path != "/s/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("visitor cookie attrs = %+v", cookie)
	}
	if cookie.MaxAge != shareVisitorMaxAge {
		t.Fatalf("visitor cookie max-age = %d, want %d", cookie.MaxAge, shareVisitorMaxAge)
	}
	if cookie.Secure {
		t.Fatal("visitor cookie is Secure on a plain-HTTP request")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(cookie.Value); err != nil || len(raw) != shareVisitorIDBytes {
		t.Fatalf("visitor cookie value = %q err=%v", cookie.Value, err)
	}
	wantSharePVUV(t, s, id, 1, 1)

	// Second open with the issued cookie: PV grows. The cookie-less first hit
	// landed in the fallback bucket, so this is a second distinct hash.
	req = publicShareRequest(token)
	req.AddCookie(&http.Cookie{Name: shareVisitorCookie, Value: cookie.Value})
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie open = %d %s", rec.Code, rec.Body.String())
	}
	if visitorCookieFrom(t, rec) != nil {
		t.Fatal("valid visitor cookie should not be reissued")
	}
	wantSharePVUV(t, s, id, 2, 2)

	// The same cookie again: PV grows, UV does not.
	req = publicShareRequest(token)
	req.AddCookie(&http.Cookie{Name: shareVisitorCookie, Value: cookie.Value})
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, req)
	wantSharePVUV(t, s, id, 3, 2)

	// A different cookie is a different visitor: both counters grow.
	secondCookie, err := newShareVisitorID()
	if err != nil {
		t.Fatal(err)
	}
	req = publicShareRequest(token)
	req.AddCookie(&http.Cookie{Name: shareVisitorCookie, Value: secondCookie})
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, req)
	wantSharePVUV(t, s, id, 4, 3)

	// A malformed cookie is ignored (treated as no cookie), which falls back to
	// the same IP+UA bucket as the first open: PV grows, UV stays.
	req = publicShareRequest(token)
	req.AddCookie(&http.Cookie{Name: shareVisitorCookie, Value: "not-base64!"})
	req.Header.Set("User-Agent", "PVUV-test-agent")
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, req)
	if visitorCookieFrom(t, rec) == nil {
		t.Fatal("malformed visitor cookie was not replaced")
	}
	wantSharePVUV(t, s, id, 5, 3)

	// Unknown tokens, expired shares and revoked shares must not count.
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token = %d", rec.Code)
	}
	expiredID, _ := newArtifactShareID()
	expiredToken, err := s.artifactShareToken(expiredID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.boardDB.CreateShare(map[string]any{
		"id": expiredID, "artifact_id": artifactID, "token_hash": artifactShareTokenHash(expiredToken),
		"expires_at": "2020-01-01 00:00:00", "created_by": "tester",
	}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(expiredToken))
	if rec.Code != http.StatusGone {
		t.Fatalf("expired token = %d", rec.Code)
	}
	wantSharePVUV(t, s, expiredID, 0, 0)

	revokeReq := artifactReq(http.MethodDelete, "http://deck.local/api/agent-board/shares/"+id, nil)
	revokeReq.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	s.handleAgentBoardRevokeShare(rec, revokeReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(token))
	if rec.Code != http.StatusGone {
		t.Fatalf("revoked token = %d", rec.Code)
	}
	wantSharePVUV(t, s, id, 5, 3)

	// kind=url shares count the 302 open.
	urlArtifact := publishShareTestArtifact(t, s, "T-1", map[string]any{
		"title": "外链", "kind": "url", "filename": "link", "url": "https://example.com/pvuv",
	})
	urlShare := createShareTestLink(t, s, urlArtifact["id"].(string), nil, http.StatusCreated)
	urlShareID, _ := urlShare["id"].(string)
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(shareTokenFromURL(t, urlShare["url"].(string))))
	if rec.Code != http.StatusFound {
		t.Fatalf("url share = %d", rec.Code)
	}
	wantSharePVUV(t, s, urlShareID, 1, 1)

	// Secure is added when the request itself is TLS or a proxy says it is.
	secureShare := createShareTestLink(t, s, artifactID, nil, http.StatusCreated)
	secureReq := publicShareRequest(shareTokenFromURL(t, secureShare["url"].(string)))
	secureReq.TLS = &tls.ConnectionState{}
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, secureReq)
	if cookie := visitorCookieFrom(t, rec); cookie == nil || !cookie.Secure {
		t.Fatalf("TLS visitor cookie = %+v, want Secure", cookie)
	}
	proxyShare := createShareTestLink(t, s, artifactID, nil, http.StatusCreated)
	proxyReq := publicShareRequest(shareTokenFromURL(t, proxyShare["url"].(string)))
	proxyReq.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, proxyReq)
	if cookie := visitorCookieFrom(t, rec); cookie == nil || !cookie.Secure {
		t.Fatalf("proxied visitor cookie = %+v, want Secure", cookie)
	}

	// Storage contract: only the five aggregate columns exist, every visitor
	// key is a 64-char HMAC hex string, and raw IP/UA never appear. PV/UV must
	// equal SUM(hits)/COUNT(DISTINCT visitor_hash).
	db, err := sql.Open("sqlite", filepath.Join(s.cfg().DataDir, "agent-board.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// One connection so the foreign_keys pragma below sticks to the connection
	// that runs the DELETE.
	db.SetMaxOpenConns(1)
	colRows, err := db.Query(`PRAGMA table_info(artifact_share_visitors)`)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for colRows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             any
		)
		if err := colRows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			colRows.Close()
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	colRows.Close()
	if strings.Join(columns, ",") != "share_id,visitor_hash,hits,first_seen,last_seen" {
		t.Fatalf("visitor table columns = %v", columns)
	}
	visitorRows, err := db.Query(`SELECT share_id,visitor_hash,hits FROM artifact_share_visitors`)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for visitorRows.Next() {
		var shareID, hash string
		var hits int
		if err := visitorRows.Scan(&shareID, &hash, &hits); err != nil {
			visitorRows.Close()
			t.Fatal(err)
		}
		seen++
		if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
			visitorRows.Close()
			t.Fatalf("visitor hash is not a 64-char hex digest: %q", hash)
		}
		if hits < 1 {
			visitorRows.Close()
			t.Fatalf("visitor hits = %d, want >= 1", hits)
		}
	}
	if err := visitorRows.Err(); err != nil {
		visitorRows.Close()
		t.Fatal(err)
	}
	visitorRows.Close()
	if seen == 0 {
		t.Fatal("no visitor rows were recorded")
	}
	var sumHits, distinct int
	if err := db.QueryRow(`SELECT COALESCE(SUM(hits),0), COUNT(DISTINCT visitor_hash)
		FROM artifact_share_visitors WHERE share_id=?`, id).Scan(&sumHits, &distinct); err != nil {
		t.Fatal(err)
	}
	if sumHits != 5 || distinct != 3 {
		t.Fatalf("stored PV/UV = %d/%d, want 5/3", sumHits, distinct)
	}

	// Deleting the share cascades to its visitor rows.
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM artifact_shares WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	var leftovers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifact_share_visitors WHERE share_id=?`, id).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("visitor rows survived share delete: %d", leftovers)
	}
}

func renewShareTestRequest(s *Server, id string, body []byte) *httptest.ResponseRecorder {
	req := artifactReq(http.MethodPost,
		"http://deck.local/api/agent-board/shares/"+url.PathEscape(id)+"/renew", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleAgentBoardRenewShare(rec, req)
	return rec
}

func decodeRenewedShare(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out struct {
		Share map[string]any `json:"share"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode renewed share: %v (%s)", err, rec.Body.String())
	}
	return out.Share
}

// TestArtifactShareRenewKeepsURLAndStats covers the renewal contract: moving
// expires_at must revive the very same URL (share id/token hash untouched)
// while keeping PV/UV, the list must already hand out the URL for expired rows
// so the UI can offer 续期, and revocation stays final.
func TestArtifactShareRenewKeepsURLAndStats(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)
	artifact := publishShareTestArtifact(t, s, "T-1", map[string]any{
		"title": "续期报告", "kind": "html", "filename": "renew.html",
		"content": "<!doctype html><html><body><h1>renewed</h1></body></html>",
	})
	artifactID, _ := artifact["id"].(string)

	// Seed an expired-but-not-revoked share with two known visitors (2 + 1
	// opens), so a renewal that cleared the counters would be visible.
	id, _ := newArtifactShareID()
	token, err := s.artifactShareToken(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.boardDB.CreateShare(map[string]any{
		"id": id, "artifact_id": artifactID, "token_hash": artifactShareTokenHash(token),
		"expires_at": "2020-01-01 00:00:00", "created_by": "tester",
	}); err != nil {
		t.Fatal(err)
	}
	visitorA, _ := newShareVisitorID()
	visitorB, _ := newShareVisitorID()
	hashA, err := s.artifactShareVisitorHash(id, "cookie\x00"+visitorA)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := s.artifactShareVisitorHash(id, "cookie\x00"+visitorB)
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{hashA, hashA, hashB} {
		if err := s.boardDB.RecordShareVisit(id, hash); err != nil {
			t.Fatal(err)
		}
	}

	// Expired rows are already listed with their rebuilt URL and renewable=true
	// — that is what lets the management UI offer 续期 without minting a link.
	before := listedShare(t, s, id)
	if before["status"] != "expired" || before["active"] != false || before["renewable"] != true {
		t.Fatalf("expired share before renew = %+v", before)
	}
	beforeURL, _ := before["url"].(string)
	if beforeURL == "" {
		t.Fatalf("expired share has no rebuilt url: %+v", before)
	}
	if got := shareTokenFromURL(t, beforeURL); got != token {
		t.Fatalf("rebuilt token = %q, want %q", got, token)
	}
	if pv, uv := sharePVUV(t, before); pv != 3 || uv != 2 {
		t.Fatalf("seeded pv/uv = %d/%d, want 3/2", pv, uv)
	}
	rec := httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(token))
	if rec.Code != http.StatusGone {
		t.Fatalf("expired public share = %d", rec.Code)
	}
	original, err := s.boardDB.GetShare(id)
	if err != nil {
		t.Fatal(err)
	}

	// Renew 7 days: same id/token/URL, counters intact, link active again.
	rec = renewShareTestRequest(s, id, []byte(`{"expiry_days":7}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("renew = %d %s", rec.Code, rec.Body.String())
	}
	renewedShare := decodeRenewedShare(t, rec)
	if renewedShare["id"] != id {
		t.Fatalf("renewed id = %v, want %v", renewedShare["id"], id)
	}
	if renewedShare["status"] != "active" || renewedShare["active"] != true || renewedShare["renewable"] != true {
		t.Fatalf("renewed share = %+v", renewedShare)
	}
	if renewedShare["url"] != beforeURL {
		t.Fatalf("renewed url = %v, want unchanged %v", renewedShare["url"], beforeURL)
	}
	if pv, uv := sharePVUV(t, renewedShare); pv != 3 || uv != 2 {
		t.Fatalf("renew reset pv/uv to %d/%d, want 3/2", pv, uv)
	}
	expiresAt, err := time.ParseInLocation("2006-01-02 15:04:05", stringArg(renewedShare, "expires_at"), time.Local)
	if err != nil || !expiresAt.After(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("renewed expires_at = %v (err=%v)", renewedShare["expires_at"], err)
	}
	renewedRow, err := s.boardDB.GetShare(id)
	if err != nil {
		t.Fatal(err)
	}
	if renewedRow.TokenHash != original.TokenHash || renewedRow.ArtifactID != original.ArtifactID ||
		renewedRow.CreatedBy != original.CreatedBy || renewedRow.CreatedAt != original.CreatedAt ||
		renewedRow.RevokedAt != "" {
		t.Fatalf("renew changed share identity: before=%+v after=%+v", original, renewedRow)
	}
	after := listedShare(t, s, id)
	if after["status"] != "active" || after["expires_at"] != renewedShare["expires_at"] ||
		after["url"] != beforeURL {
		t.Fatalf("listed after renew = %+v", after)
	}

	// The original URL works again and keeps accumulating stats. Using
	// visitorA's cookie hits the seeded hash: PV grows, UV stays 2.
	rec = httptest.NewRecorder()
	openReq := publicShareRequest(token)
	openReq.AddCookie(&http.Cookie{Name: shareVisitorCookie, Value: visitorA})
	s.handleArtifactShare(rec, openReq)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h1>renewed</h1>") {
		t.Fatalf("renewed public share = %d %q", rec.Code, rec.Body.String())
	}
	wantSharePVUV(t, s, id, 4, 2)

	// Active shares can be renewed too — including to permanent.
	active := createShareTestLink(t, s, artifactID, 1, http.StatusCreated)
	activeID, _ := active["id"].(string)
	activeURL, _ := active["url"].(string)
	rec = renewShareTestRequest(s, activeID, []byte(`{"expiry_days":null}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("renew active to permanent = %d %s", rec.Code, rec.Body.String())
	}
	permanentShare := decodeRenewedShare(t, rec)
	if permanentShare["status"] != "active" || permanentShare["active"] != true {
		t.Fatalf("permanent renewed share = %+v", permanentShare)
	}
	if exp, _ := permanentShare["expires_at"].(string); exp != "" {
		t.Fatalf("permanent renewed expires_at = %q", exp)
	}
	if permanentShare["url"] != activeURL {
		t.Fatalf("permanent renewed url = %v, want %v", permanentShare["url"], activeURL)
	}

	// Invalid expiry values are rejected before touching the row.
	for _, body := range []string{`{"expiry_days":2}`, `{"expiry_days":-1}`, `not json`} {
		rec = renewShareTestRequest(s, id, []byte(body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("renew with %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if got := listedShare(t, s, id); got["status"] != "active" || got["url"] != beforeURL {
		t.Fatalf("invalid renew changed the share: %+v", got)
	}

	// Revocation is final: no URL in the list, renewal rejected with a clear
	// error, and the public route stays 410.
	revoked := createShareTestLink(t, s, artifactID, nil, http.StatusCreated)
	revokedID, _ := revoked["id"].(string)
	revokedToken := shareTokenFromURL(t, revoked["url"].(string))
	req := artifactReq(http.MethodDelete, "http://deck.local/api/agent-board/shares/"+revokedID, nil)
	req.SetPathValue("id", revokedID)
	rec = httptest.NewRecorder()
	s.handleAgentBoardRevokeShare(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	rec = renewShareTestRequest(s, revokedID, []byte(`{"expiry_days":7}`))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "已取消") {
		t.Fatalf("renew revoked = %d %s", rec.Code, rec.Body.String())
	}
	revokedRow := listedShare(t, s, revokedID)
	if revokedRow["status"] != "revoked" || revokedRow["renewable"] != false || revokedRow["url"] != nil {
		t.Fatalf("revoked row after renew attempt = %+v", revokedRow)
	}
	rec = httptest.NewRecorder()
	s.handleArtifactShare(rec, publicShareRequest(revokedToken))
	if rec.Code != http.StatusGone {
		t.Fatalf("revoked public share = %d", rec.Code)
	}

	// Unknown ids are a 404, not a silent success.
	rec = renewShareTestRequest(s, "nope", []byte(`{"expiry_days":7}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("renew unknown = %d %s", rec.Code, rec.Body.String())
	}
}
