package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"cc-webui/internal/config"
)

var (
	ErrDisabled       = errors.New("浏览器功能未开启")
	ErrChromeMissing  = errors.New("未找到本地 Chrome/Chromium")
	ErrCapacity       = errors.New("浏览器并发已达上限")
	ErrProfileBusy    = errors.New("浏览器共享 profile 正在被另一个会话使用")
	ErrHumanControl   = errors.New("浏览器已被人工接管")
	errSessionStopped = errors.New("浏览器已关闭")
)

var safeID = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func profileKey(sessionID string) string {
	key := safeID.ReplaceAllString(sessionID, "_")
	if key == "" {
		return "session"
	}
	if len(key) > 80 {
		key = key[:80]
	}
	return key
}

type settings struct {
	Enabled       bool `json:"enabled"`
	SharedProfile bool `json:"shared_profile"`
}

// Manager owns the PoC's one-browser-per-session runtime. It is deliberately
// small: Chrome is local, each session gets an isolated user-data-dir, and
// every action is serialized through Session.mu.
type Manager struct {
	cm      *config.Manager
	dataDir string

	mu            sync.Mutex
	sessions      map[string]*Session
	profileOwners map[string]string // profile name -> session id (per-session mode)
	runtimes      map[string]*profileRuntime
	runtimeStarts map[string]*runtimeStart
	enabled       bool
	sharedProfile bool
	chrome        ChromeInfo

	settingsPath string
	lastErr      string
	closed       bool
}

type Session struct {
	id          string
	profileName string
	profile     string
	runtime     *profileRuntime
	targetID    target.ID
	ctx         context.Context
	cancel      context.CancelFunc
	allocCxl    context.CancelFunc

	actionMu   sync.Mutex
	mu         sync.Mutex
	started    bool
	closed     bool
	lastUsed   time.Time
	ready      chan struct{}
	readyOnce  sync.Once
	startErr   error
	url        string
	title      string
	control    string // agent|human
	viewers    map[chan Frame]struct{}
	screencast bool
	fps        int
	quality    int
	lastFrame  time.Time
}

type Frame struct {
	Data   string `json:"data"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// profileRuntime owns one Chrome process for a shared profile. Sessions attach
// to separate CDP targets in this browser; cookies and localStorage are shared
// because every target lives in the same default browser context.
type profileRuntime struct {
	key      string
	profile  string
	allocCtx context.Context
	rootCtx  context.Context
	browser  *chromedp.Browser
	cancel   context.CancelFunc
	allocCxl context.CancelFunc

	mu     sync.Mutex
	closed bool
	// refs is protected by Manager.mu, not by mu; it counts sessions using
	// this runtime.
	refs int
}

type runtimeStart struct {
	done chan struct{}
	rt   *profileRuntime
	err  error
}

func (r *profileRuntime) newTarget(ctx context.Context) (target.ID, error) {
	id, err := target.CreateTarget("about:blank").WithNewWindow(true).
		Do(cdp.WithExecutor(ctx, r.browser))
	if err != nil {
		// Older Chrome builds can lack NewWindow support in headless mode;
		// fall back to a tab in the same profile/context.
		id, err = target.CreateTarget("about:blank").WithNewWindow(false).
			Do(cdp.WithExecutor(ctx, r.browser))
	}
	return id, err
}

func (r *profileRuntime) closeTarget(ctx context.Context, id target.ID) error {
	if id == "" {
		return nil
	}
	return target.CloseTarget(id).Do(cdp.WithExecutor(ctx, r.browser))
}

func (r *profileRuntime) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	if r.allocCxl != nil {
		r.allocCxl()
	}
}

type Status struct {
	Enabled      bool         `json:"enabled"`
	Available    bool         `json:"available"`
	ChromePath   string       `json:"chrome_path,omitempty"`
	ChromeSource string       `json:"chrome_source,omitempty"`
	Running      int          `json:"running"`
	MaxBrowsers  int          `json:"max_browsers"`
	MaxWindows   int          `json:"max_windows,omitempty"`
	IdleMinutes  int          `json:"idle_minutes"`
	Reason       string       `json:"reason,omitempty"`
	ProfileMode  string       `json:"profile_mode,omitempty"`
	ProfileKey   string       `json:"profile_key,omitempty"`
	Install      *InstallPlan `json:"install,omitempty"`
}

type SessionStatus struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	Control string `json:"control"`
	Target  string `json:"target,omitempty"`
}

func NewManager(cm *config.Manager, dataDir string) *Manager {
	m := &Manager{
		cm:            cm,
		dataDir:       dataDir,
		sessions:      map[string]*Session{},
		profileOwners: map[string]string{},
		runtimes:      map[string]*profileRuntime{},
		runtimeStarts: map[string]*runtimeStart{},
		settingsPath:  filepath.Join(dataDir, "browser", "settings.json"),
	}
	if cfg := cm.Get(); cfg != nil {
		m.enabled = cfg.BrowserOn()
	}
	if raw, err := os.ReadFile(m.settingsPath); err == nil {
		var s settings
		if json.Unmarshal(raw, &s) == nil {
			m.enabled = s.Enabled
			m.sharedProfile = s.SharedProfile
		}
	}
	go m.reap()
	return m
}

func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

func (m *Manager) config() config.BrowserConfig {
	cfg := m.cm.Get()
	if cfg == nil {
		return config.BrowserConfig{MaxBrowsers: 2, MaxWindows: 4, IdleMinutes: 10, FPS: 8, Quality: 60}
	}
	return cfg.Browser
}

func (m *Manager) selfListen() string {
	if cfg := m.cm.Get(); cfg != nil {
		return cfg.Listen
	}
	return ""
}

func (m *Manager) persist() error {
	if err := os.MkdirAll(filepath.Dir(m.settingsPath), 0o700); err != nil {
		return err
	}
	raw, _ := json.Marshal(settings{Enabled: m.enabled, SharedProfile: m.sharedProfile})
	tmp := m.settingsPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.settingsPath)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	enabled := m.enabled
	shared := m.sharedProfile
	running := len(m.sessions)
	chrome := m.chrome
	m.mu.Unlock()
	if chrome.Path == "" {
		chrome = DetectChrome(m.config().ChromePath)
	}
	cfg := m.config()
	st := Status{
		Enabled:      enabled,
		Available:    chrome.Path != "",
		ChromePath:   chrome.Path,
		ChromeSource: chrome.Source,
		Running:      running,
		MaxBrowsers:  cfg.MaxBrowsers,
		MaxWindows:   cfg.MaxWindows,
		IdleMinutes:  cfg.IdleMinutes,
	}
	if shared {
		st.ProfileMode = "shared"
		st.ProfileKey = m.sharedKey()
	} else {
		st.ProfileMode = "session"
	}
	if chrome.Path == "" {
		plan := InstallPlanFor()
		st.Install = &plan
		st.Reason = plan.Summary
	}
	return st
}

// Enable detects Chrome and, when asked, runs the one whitelisted install
// command. It probes the binary before persisting the switch so a broken
// Chrome does not make the UI look ready.
func (m *Manager) Enable(ctx context.Context, install bool) (Status, error) {
	cfg := m.config()
	chrome := DetectChrome(cfg.ChromePath)
	if chrome.Path == "" && install {
		plan := InstallPlanFor()
		if !plan.Auto {
			m.mu.Lock()
			m.lastErr = plan.Summary
			m.mu.Unlock()
			return m.Status(), fmt.Errorf("%s", plan.Summary)
		}
		if _, err := Install(ctx, plan); err != nil {
			m.mu.Lock()
			m.lastErr = err.Error()
			m.mu.Unlock()
			return m.Status(), err
		}
		chrome = DetectChrome(cfg.ChromePath)
	}
	if chrome.Path == "" {
		plan := InstallPlanFor()
		m.mu.Lock()
		m.lastErr = plan.Summary
		m.mu.Unlock()
		return m.Status(), ErrChromeMissing
	}
	if err := probeChrome(ctx, chrome.Path); err != nil {
		m.mu.Lock()
		m.lastErr = err.Error()
		m.mu.Unlock()
		return m.Status(), fmt.Errorf("Chrome 启动失败: %w", err)
	}
	m.mu.Lock()
	m.chrome = chrome
	m.enabled = true
	m.lastErr = ""
	err := m.persist()
	m.mu.Unlock()
	if err != nil {
		return m.Status(), err
	}
	return m.Status(), nil
}

func (m *Manager) Disable() (Status, error) {
	m.mu.Lock()
	m.enabled = false
	err := m.persist()
	m.mu.Unlock()
	m.CloseAll()
	return m.Status(), err
}

func (m *Manager) Install(ctx context.Context) (Status, error) {
	return m.Enable(ctx, true)
}

// SetSharedProfile persists the Settings switch. Existing browsers are closed
// because their profile path changes; the next browser open uses the new mode.
func (m *Manager) SetSharedProfile(shared bool) (Status, error) {
	m.mu.Lock()
	m.sharedProfile = shared
	err := m.persist()
	m.mu.Unlock()
	m.CloseAll()
	return m.Status(), err
}

// sharedKey is the stable Chrome profile name used when the Settings switch
// is on. browser.profile_key only names it; it does not enable sharing.
func (m *Manager) sharedKey() string {
	key := strings.TrimSpace(m.config().ProfileKey)
	if key == "" {
		key = "user"
	}
	return key
}

// profileName is the logical login container: one per session by default, or
// one stable key shared by sessions while the Settings switch is on.
func (m *Manager) profileName(sessionID string) string {
	if !m.sharedProfile {
		return "session:" + sessionID
	}
	return "shared:" + profileKey(m.sharedKey())
}

func profileDirName(name string) string {
	if strings.HasPrefix(name, "shared:") {
		return "shared-" + profileKey(strings.TrimPrefix(name, "shared:"))
	}
	return profileKey(strings.TrimPrefix(name, "session:"))
}

func (m *Manager) getOrCreate(sessionID string) (*Session, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("服务器正在关闭")
	}
	if !m.enabled {
		m.mu.Unlock()
		return nil, ErrDisabled
	}
	if s := m.sessions[sessionID]; s != nil {
		m.mu.Unlock()
		<-s.ready
		if s.startErr != nil {
			return nil, s.startErr
		}
		return s, nil
	}
	cfg := m.config()
	shared := m.sharedProfile
	limit := cfg.MaxBrowsers
	if shared {
		limit = maxWindows(cfg)
	}
	if len(m.sessions) >= limit {
		m.mu.Unlock()
		return nil, ErrCapacity
	}
	chrome := DetectChrome(cfg.ChromePath)
	if chrome.Path == "" {
		m.mu.Unlock()
		return nil, ErrChromeMissing
	}
	profName := m.profileName(sessionID)
	profile := filepath.Join(m.dataDir, "browser", "profiles", profileDirName(profName))
	if err := os.MkdirAll(profile, 0o700); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	s := &Session{
		id:          sessionID,
		profileName: profName,
		profile:     profile,
		lastUsed:    time.Now(),
		control:     "agent",
		viewers:     map[chan Frame]struct{}{},
		ready:       make(chan struct{}),
	}
	m.sessions[sessionID] = s
	if !shared {
		m.profileOwners[profName] = sessionID
	}
	m.mu.Unlock()

	var err error
	if shared {
		var rt *profileRuntime
		rt, err = m.acquireRuntime(profName, profile, chrome.Path, cfg)
		if err == nil {
			var targetID target.ID
			targetID, err = rt.newTarget(context.Background())
			if err == nil {
				err = s.attach(rt, targetID, cfg)
				if err != nil {
					_ = rt.closeTarget(context.Background(), targetID)
				}
			}
			if err != nil {
				m.releaseRuntime(rt)
			}
		}
	} else {
		// Start outside the manager lock: a slow first Chrome must not block
		// status calls or other sessions. Concurrent callers wait on s.ready.
		err = s.start(chrome.Path, cfg)
	}
	s.finishStart(err)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, sessionID)
		if m.profileOwners[profName] == sessionID {
			delete(m.profileOwners, profName)
		}
		m.lastErr = err.Error()
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Lock()
	m.chrome = chrome
	m.mu.Unlock()
	return s, nil
}

func maxWindows(cfg config.BrowserConfig) int {
	if cfg.MaxWindows <= 0 {
		return 4
	}
	return cfg.MaxWindows
}

// acquireRuntime returns the shared Chrome runtime for a profile and increments
// its session reference count. Concurrent first callers wait for the same start
// instead of racing to open the same user-data-dir twice.
func (m *Manager) acquireRuntime(key, profile, chromePath string, cfg config.BrowserConfig) (*profileRuntime, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, errors.New("服务器正在关闭")
		}
		if rt := m.runtimes[key]; rt != nil {
			rt.mu.Lock()
			closed := rt.closed
			if !closed {
				rt.mu.Unlock()
				// Refcount is owned by Manager.mu.
				rt.refs++
				m.mu.Unlock()
				return rt, nil
			}
			rt.mu.Unlock()
			delete(m.runtimes, key)
		}
		if start := m.runtimeStarts[key]; start != nil {
			m.mu.Unlock()
			<-start.done
			if start.err != nil {
				return nil, start.err
			}
			continue
		}
		start := &runtimeStart{done: make(chan struct{})}
		m.runtimeStarts[key] = start
		m.mu.Unlock()

		rt, err := newProfileRuntime(key, profile, chromePath, cfg, m.handleTargetDestroyed)
		m.mu.Lock()
		delete(m.runtimeStarts, key)
		start.rt, start.err = rt, err
		if err == nil {
			rt.refs = 1
			m.runtimes[key] = rt
		}
		m.mu.Unlock()
		close(start.done)
		if err != nil {
			return nil, err
		}
		return rt, nil
	}
}

// releaseRuntime drops one session reference and closes Chrome when the last
// session for the profile is gone.
func (m *Manager) releaseRuntime(rt *profileRuntime) {
	if rt == nil {
		return
	}
	m.mu.Lock()
	if rt.refs > 0 {
		rt.refs--
	}
	closeRuntime := rt.refs == 0 && m.runtimes[rt.key] == rt
	if closeRuntime {
		delete(m.runtimes, rt.key)
	}
	m.mu.Unlock()
	if closeRuntime {
		rt.close()
	}
}

// handleTargetDestroyed cleans up the session whose window/tab was closed
// directly in Chrome. Without this, a manually closed window would keep a
// stale session entry and a runtime reference until the idle reaper ran.
func (m *Manager) handleTargetDestroyed(id target.ID) {
	if id == "" {
		return
	}
	m.mu.Lock()
	var gone *Session
	for sessionID, s := range m.sessions {
		if s.targetID == id {
			gone = s
			delete(m.sessions, sessionID)
			if m.profileOwners[s.profileName] == sessionID {
				delete(m.profileOwners, s.profileName)
			}
			break
		}
	}
	m.mu.Unlock()
	if gone != nil {
		// Never block the browser event loop: chromedp's cancel path sends
		// Target.detachFromTarget and would deadlock if called synchronously
		// from its listener. Drop the runtime reference and clear the session
		// state on a separate goroutine.
		go func() {
			m.releaseRuntime(gone.runtime)
			gone.closeGone()
			log.Printf("browser: target closed externally, dropped session %s", gone.id)
		}()
	}
}

func newProfileRuntime(key, profile, chromePath string, cfg config.BrowserConfig, onTargetDestroyed func(target.ID)) (*profileRuntime, error) {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(profile),
		chromedp.WindowSize(1280, 800),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	rootCtx, cancel := chromedp.NewContext(allocCtx)
	started := make(chan error, 1)
	go func() { started <- chromedp.Run(rootCtx) }()
	select {
	case err := <-started:
		if err != nil {
			cancel()
			allocCancel()
			return nil, fmt.Errorf("启动共享 Chrome: %w", err)
		}
	case <-time.After(30 * time.Second):
		cancel()
		allocCancel()
		return nil, fmt.Errorf("启动共享 Chrome 超时")
	}
	root := chromedp.FromContext(rootCtx)
	if root == nil || root.Browser == nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("共享 Chrome 未返回 browser context")
	}
	if onTargetDestroyed != nil {
		chromedp.ListenBrowser(rootCtx, func(ev interface{}) {
			if e, ok := ev.(*target.EventTargetDestroyed); ok {
				onTargetDestroyed(e.TargetID)
			}
		})
	}
	return &profileRuntime{
		key:      key,
		profile:  profile,
		allocCtx: allocCtx,
		rootCtx:  rootCtx,
		browser:  root.Browser,
		cancel:   cancel,
		allocCxl: allocCancel,
	}, nil
}

func (s *Session) attach(rt *profileRuntime, targetID target.ID, cfg config.BrowserConfig) error {
	ctx, cancel := chromedp.NewContext(rt.rootCtx, chromedp.WithTargetID(targetID))
	s.ctx = ctx
	s.cancel = cancel
	s.runtime = rt
	s.targetID = targetID
	s.fps = cfg.FPS
	s.quality = cfg.Quality
	if s.fps <= 0 {
		s.fps = 8
	}
	if s.quality <= 0 {
		s.quality = 60
	}
	chromedp.ListenTarget(ctx, s.onEvent)
	// Attach on the long-lived target context. A timeout-cancelled child would
	// tie the CDP target session to that child and break all later actions.
	attached := make(chan error, 1)
	go func() { attached <- chromedp.Run(ctx) }()
	select {
	case err := <-attached:
		if err != nil {
			cancel()
			return fmt.Errorf("附着共享 target: %w", err)
		}
	case <-time.After(10 * time.Second):
		cancel()
		return fmt.Errorf("附着共享 target 超时")
	}
	s.mu.Lock()
	s.started = true
	s.url = "about:blank"
	s.mu.Unlock()
	return nil
}

func (s *Session) finishStart(err error) {
	s.readyOnce.Do(func() {
		s.mu.Lock()
		s.startErr = err
		s.mu.Unlock()
		if s.ready != nil {
			close(s.ready)
		}
	})
}

func (s *Session) start(chromePath string, cfg config.BrowserConfig) error {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(s.profile),
		chromedp.WindowSize(1280, 800),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	s.ctx = ctx
	s.cancel = cancel
	s.allocCxl = allocCancel
	s.fps = cfg.FPS
	s.quality = cfg.Quality
	if s.fps <= 0 {
		s.fps = 8
	}
	if s.quality <= 0 {
		s.quality = 60
	}
	chromedp.ListenTarget(ctx, s.onEvent)
	started := make(chan error, 1)
	go func() { started <- chromedp.Run(ctx) }()
	select {
	case err := <-started:
		if err != nil {
			cancel()
			allocCancel()
			return fmt.Errorf("启动 Chrome: %w", err)
		}
	case <-time.After(30 * time.Second):
		cancel()
		allocCancel()
		return fmt.Errorf("启动 Chrome 超时")
	}
	s.mu.Lock()
	s.started = true
	s.url = "about:blank"
	s.mu.Unlock()
	return nil
}

func (s *Session) onEvent(ev interface{}) {
	frame, ok := ev.(*page.EventScreencastFrame)
	if !ok {
		return
	}
	f := Frame{Data: frame.Data}
	if frame.Metadata != nil {
		f.Width = int(frame.Metadata.DeviceWidth)
		f.Height = int(frame.Metadata.DeviceHeight)
	}
	s.mu.Lock()
	if s.fps > 0 && time.Since(s.lastFrame) < time.Second/time.Duration(s.fps) {
		s.mu.Unlock()
		go func(id int64) {
			_ = chromedp.Run(s.ctx, page.ScreencastFrameAck(id))
		}(frame.SessionID)
		return
	}
	s.lastFrame = time.Now()
	for ch := range s.viewers {
		select {
		case ch <- f:
		default: // drop a frame for a slow viewer
		}
	}
	s.mu.Unlock()
	// ListenTarget runs synchronously; a CDP action must go through a
	// separate goroutine or it can deadlock the target event loop.
	go func(id int64) {
		_ = chromedp.Run(s.ctx, page.ScreencastFrameAck(id))
	}(frame.SessionID)
}

func (s *Session) startScreencast() {
	s.mu.Lock()
	if s.screencast || s.closed || !s.started {
		s.mu.Unlock()
		return
	}
	s.screencast = true
	s.mu.Unlock()
	go func() {
		err := chromedp.Run(s.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			return page.StartScreencast().
				WithFormat(page.ScreencastFormatJpeg).
				WithQuality(60).
				WithMaxWidth(1280).
				WithMaxHeight(800).
				WithEveryNthFrame(1).
				Do(ctx)
		}))
		if err != nil {
			s.mu.Lock()
			s.startErr = err
			s.screencast = false
			s.mu.Unlock()
		}
	}()
}

func (s *Session) stopScreencast() {
	s.mu.Lock()
	if !s.screencast {
		s.mu.Unlock()
		return
	}
	s.screencast = false
	viewers := len(s.viewers)
	s.mu.Unlock()
	if viewers == 0 {
		_ = chromedp.Run(s.ctx, page.StopScreencast())
	}
}

func (s *Session) Subscribe() (<-chan Frame, func()) {
	ch := make(chan Frame, 2)
	s.mu.Lock()
	if s.closed {
		// A viewer that arrives after the browser closed gets an already
		// closed stream; the SSE handler exits instead of waiting forever.
		s.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	s.viewers[ch] = struct{}{}
	s.lastUsed = time.Now()
	s.mu.Unlock()
	s.startScreencast()
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			s.mu.Lock()
			// Session.close closes every viewer and clears the map. Only the
			// side that still finds the channel in the map may close it, so
			// close+unsub can never close the same channel twice.
			if _, ok := s.viewers[ch]; ok {
				delete(s.viewers, ch)
				close(ch)
			}
			s.mu.Unlock()
			s.stopScreencast()
		})
	}
	return ch, unsub
}

func (s *Session) Status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionStatus{
		ID:      s.id,
		Running: s.started && !s.closed,
		URL:     s.url,
		Title:   s.title,
		Control: s.control,
		Target:  string(s.targetID),
	}
}

func (s *Session) SetControl(control string) {
	s.mu.Lock()
	s.control = control
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// Subscribe opens a viewer for a session, starting its browser if needed.
func (m *Manager) Subscribe(sessionID string) (<-chan Frame, func(), error) {
	s, err := m.getOrCreate(sessionID)
	if err != nil {
		return nil, nil, err
	}
	ch, unsub := s.Subscribe()
	return ch, unsub, nil
}

func (m *Manager) SessionStatus(sessionID string) SessionStatus {
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return SessionStatus{ID: sessionID, Control: "agent"}
	}
	return s.Status()
}

func (m *Manager) SetControl(sessionID, control string) error {
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return errSessionStopped
	}
	if control != "agent" && control != "human" {
		return errors.New("control 只能是 agent 或 human")
	}
	s.SetControl(control)
	return nil
}

func (m *Manager) CloseSession(sessionID string) {
	m.mu.Lock()
	s := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	if s != nil && m.profileOwners[s.profileName] == sessionID {
		delete(m.profileOwners, s.profileName)
	}
	m.mu.Unlock()
	if s != nil {
		s.close()
		m.releaseRuntime(s.runtime)
	}
}

func (s *Session) close() {
	s.closeInternal(true)
}

// closeGone cleans up a session whose target was already destroyed by the
// user. It must not send Target.closeTarget again: the target is gone and that
// command can block while the browser is delivering the destroy event.
func (s *Session) closeGone() {
	s.closeInternal(false)
}

func (s *Session) closeInternal(closeTarget bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for ch := range s.viewers {
		close(ch)
	}
	s.viewers = map[chan Frame]struct{}{}
	s.mu.Unlock()
	s.finishStart(errSessionStopped)
	if s.runtime != nil {
		// Shared mode: close only this session's target. The Chrome process
		// and profile stay alive for the other sessions; Manager.releaseRuntime
		// closes Chrome when the last reference goes away.
		if closeTarget {
			_ = s.runtime.closeTarget(context.Background(), s.targetID)
		}
		if s.cancel != nil {
			s.cancel()
		}
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.allocCxl != nil {
		s.allocCxl()
	}
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		sessions = append(sessions, s)
		delete(m.sessions, id)
		if m.profileOwners[s.profileName] == id {
			delete(m.profileOwners, s.profileName)
		}
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
		m.releaseRuntime(s.runtime)
	}
}

// Shutdown closes every browser and refuses new sessions. A plain Disable uses
// CloseAll so the feature can be turned back on without restarting the server.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.CloseAll()
}

func (m *Manager) reap() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cfg := m.config()
		idle := time.Duration(cfg.IdleMinutes) * time.Minute
		now := time.Now()
		var close []*Session
		m.mu.Lock()
		for id, s := range m.sessions {
			s.mu.Lock()
			idleFor := now.Sub(s.lastUsed)
			human := s.control == "human"
			s.mu.Unlock()
			if !human && idleFor > idle {
				close = append(close, s)
				delete(m.sessions, id)
				if m.profileOwners[s.profileName] == id {
					delete(m.profileOwners, s.profileName)
				}
			}
		}
		m.mu.Unlock()
		for _, s := range close {
			s.close()
			m.releaseRuntime(s.runtime)
			log.Printf("browser: closed idle session %s", s.id)
		}
	}
}

func probeChromeContext(ctx context.Context, chromePath string) error {
	dir, err := os.MkdirTemp("", "cc-webui-browser-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(dir),
		chromedp.WindowSize(800, 600),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()
	tabCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	if err := chromedp.Run(tabCtx, chromedp.Navigate("about:blank")); err != nil {
		return err
	}
	return nil
}

func (m *Manager) policyConfig() policyConfig {
	return policyConfig{Browser: m.config(), SelfListen: m.selfListen()}
}

var _ = strings.TrimSpace
