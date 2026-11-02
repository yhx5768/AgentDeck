#!/usr/bin/env bash
# Recreate the workspace fixture the film's "工作区面板" scene points at.
#
# It lives OUTSIDE this repository (/home/soar/Projects/agentdeck-demo) on
# purpose: the workspace panel renders `git diff`, so the shot needs a real
# repository with real uncommitted changes — and faking those inside the
# cc-webui worktree would leave the tree dirty for the final merge.
#
# Idempotent: wipes and rebuilds the fixture.
set -euo pipefail

D=${DEMO_PROJECT_DIR:-/home/soar/Projects/agentdeck-demo}
rm -rf "$D"
mkdir -p "$D"/{cmd/gateway,internal/router,web}
cd "$D"

cat > go.mod <<'EOF'
module agentdeck-demo

go 1.22
EOF

cat > README.md <<'EOF'
# agentdeck-demo

A tiny gateway used as the workspace fixture for the AgentDeck intro film.
EOF

cat > config.example.yaml <<'EOF'
listen: "0.0.0.0:8080"
upstream: "http://127.0.0.1:9000"
timeout: "30s"
routes:
  - prefix: "/v1/chat"
    upstream: "http://127.0.0.1:9100"
EOF

cat > cmd/gateway/main.go <<'EOF'
package main

import (
	"log"
	"net/http"
	"time"

	"agentdeck-demo/internal/router"
)

func main() {
	rt := router.New()
	rt.Add("/v1/chat", "http://127.0.0.1:9100")

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           rt,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Println("gateway listening on :8080")
	log.Fatal(srv.ListenAndServe())
}
EOF

cat > internal/router/router.go <<'EOF'
// Package router is a prefix-matching reverse proxy table.
package router

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
)

type route struct {
	prefix   string
	upstream string
	proxy    *httputil.ReverseProxy
}

// Router dispatches by longest matching path prefix.
type Router struct {
	routes []route
}

func New() *Router { return &Router{} }

func (r *Router) Add(prefix, upstream string) {
	u, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	r.routes = append(r.routes, route{prefix: prefix, upstream: upstream, proxy: httputil.NewSingleHostReverseProxy(u)})
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	for _, rt := range r.routes {
		if strings.HasPrefix(req.URL.Path, rt.prefix) {
			rt.proxy.ServeHTTP(w, req)
			return
		}
	}
	http.NotFound(w, req)
}

func (r *Router) Prefixes() []string {
	out := make([]string, 0, len(r.routes))
	for _, rt := range r.routes {
		out = append(out, rt.prefix)
	}
	sort.Strings(out)
	return out
}
EOF

cat > web/dashboard.js <<'EOF'
// Minimal upstream health board for the demo gateway.
const REFRESH_MS = 5000;

async function tick() {
  const r = await fetch('/__routes');
  const rows = await r.json();
  render(rows);
}

function render(rows) {
  const el = document.querySelector('#routes');
  el.innerHTML = rows.map((r) => `<tr><td>${r.prefix}</td><td>${r.upstream}</td></tr>`).join('');
}

setInterval(tick, REFRESH_MS);
tick();
EOF

git init -q .
git add -A
git -c user.email=demo@local -c user.name=demo commit -qm "initial gateway"

# The uncommitted work the video shows as a diff: modify router.go, add a test.
python3 - <<'PY'
import pathlib

p = pathlib.Path('internal/router/router.go')
s = p.read_text()
s = s.replace(
"""func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	for _, rt := range r.routes {
		if strings.HasPrefix(req.URL.Path, rt.prefix) {""",
"""// match picks the longest matching prefix, so /v1/chat/stream wins over
// /v1/chat regardless of registration order. The old first-match loop made
// behaviour depend on the order Add() happened to be called in.
func (r *Router) match(path string) (route, bool) {
	var best route
	found := false
	for _, rt := range r.routes {
		if !strings.HasPrefix(path, rt.prefix) {
			continue
		}
		if !found || len(rt.prefix) > len(best.prefix) {
			best, found = rt, true
		}
	}
	return best, found
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if rt, ok := r.match(req.URL.Path); ok {""")
s = s.replace(
"""			rt.proxy.ServeHTTP(w, req)
			return
		}
	}
	http.NotFound(w, req)
}""",
"""		rt.proxy.ServeHTTP(w, req)
		return
	}
	http.NotFound(w, req)
}""")
p.write_text(s)

pathlib.Path('internal/router/router_test.go').write_text('''package router

import "testing"

func TestMatchPrefersLongestPrefix(t *testing.T) {
	r := New()
	r.Add("/v1/chat", "http://127.0.0.1:9100")
	r.Add("/v1/chat/stream", "http://127.0.0.1:9101")

	got, ok := r.match("/v1/chat/stream/abc")
	if !ok || got.prefix != "/v1/chat/stream" {
		t.Fatalf("got %q ok=%v, want /v1/chat/stream", got.prefix, ok)
	}
	if _, ok := r.match("/nope"); ok {
		t.Fatal("expected no match")
	}
}
''')
PY

echo "fixture ready: $D"
git status --short
