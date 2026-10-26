// Package opencodever detects which major version an opencode binary is, so
// cc-webui can route to the matching engine implementation. OpenCode 2 is a
// breaking server-API rewrite with a new CLI surface; the two are not
// installed side by side (the v2 installer replaces the v1 binary at the same
// path), so the only thing that distinguishes them at runtime is the binary
// itself.
package opencodever

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is the detected major version of an opencode binary.
type Version int

const (
	// Unknown means the binary could not be run or its output could not be
	// parsed. Callers treat it as v1 (the historical default) so an
	// unrecognized build keeps working the way it used to.
	Unknown Version = 0
	V1      Version = 1
	V2      Version = 2
)

// String renders the version for logs.
func (v Version) String() string {
	switch v {
	case V1:
		return "v1"
	case V2:
		return "v2"
	default:
		return "unknown"
	}
}

const probeTimeout = 10 * time.Second

// verRe extracts the first major version number from `opencode --version`.
// v1 prints "1.18.35"; v2 prints "opencode v2.1.4".
var verRe = regexp.MustCompile(`(\d+)\.`)

type cacheEntry struct {
	ver    Version
	mtime  int64
	size   int64
	loaded time.Time
}

var (
	mu    sync.Mutex
	cache = map[string]cacheEntry{}
)

// Detect returns the major version of the opencode binary at path. The result
// is cached keyed by (path, mtime, size): upgrading the binary changes the
// stat and re-probes, so a machine that upgrades from v1 to v2 is picked up
// without restarting cc-webui. Unknown is returned when the binary cannot be
// run.
func Detect(path string) Version {
	if path == "" {
		return Unknown
	}
	abs := path
	if p, err := filepath.Abs(path); err == nil {
		abs = p
	}

	var mtime, size int64
	if st, err := os.Stat(abs); err == nil {
		mtime = st.ModTime().UnixNano()
		size = st.Size()
	}

	mu.Lock()
	if e, ok := cache[abs]; ok && e.mtime == mtime && e.size == size {
		mu.Unlock()
		return e.ver
	}
	mu.Unlock()

	ver := probe(abs)

	mu.Lock()
	cache[abs] = cacheEntry{ver: ver, mtime: mtime, size: size, loaded: time.Now()}
	mu.Unlock()
	return ver
}

// probe runs `<path> --version` and parses the major version.
func probe(path string) Version {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return Unknown
	}
	return parse(out.String())
}

// parse pulls the major version out of the CLI's version banner. Exported for
// tests (the real binary is not needed to pin the parsing rules).
func parse(s string) Version {
	m := verRe.FindStringSubmatch(strings.TrimSpace(s))
	if len(m) < 2 {
		return Unknown
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return Unknown
	}
	switch n {
	case 1:
		return V1
	case 2:
		return V2
	default:
		return Unknown
	}
}

// Resolve returns the effective version for a binary, honoring an explicit
// config override. override may be "", "auto", "v1"/"1", or "v2"/"2";
// anything else falls back to auto-detection.
func Resolve(path, override string) Version {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "v1", "1":
		return V1
	case "v2", "2":
		return V2
	}
	return Detect(path)
}

// IsV2 reports whether the binary at path is OpenCode 2, applying override.
func IsV2(path, override string) bool { return Resolve(path, override) == V2 }
