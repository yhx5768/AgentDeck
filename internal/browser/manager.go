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

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"cc-webui/internal/config"
)

var (
	ErrDisabled       = errors.New("浏览器功能未开启")
	ErrChromeMissing  = errors.New("未找到本地 Chrome/Chromium")
	ErrCapacity       = errors.New("浏览器并发已达上限")
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
	Enabled bool `json:"enabled"`
}

// Manager owns the PoC's one-browser-per-session runtime. It is deliberately
// small: Chrome is local, each session gets an isolated user-data-dir, and
// every action is serialized through Session.mu.
type Manager struct {
	cm      *config.Manager
	dataDir string

	mu       sync.Mutex
	sessions map[string]*Session
	enabled  bool
	chrome   ChromeInfo

	settingsPath string
	lastErr      string
	closed       bool
}

type Session struct {
	id       string
	profile  string
	ctx      context.Context
	cancel   context.CancelFunc
	allocCxl context.CancelFunc

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

type Status struct {
	Enabled      bool         `json:"enabled"`
	Available    bool         `json:"available"`
	ChromePath   string       `json:"chrome_path,omitempty"`
	ChromeSource string       `json:"chrome_source,omitempty"`
	Running      int          `json:"running"`
	MaxBrowsers  int          `json:"max_browsers"`
	IdleMinutes  int          `json:"idle_minutes"`
	Reason       string       `json:"reason,omitempty"`
	Install      *InstallPlan `json:"install,omitempty"`
}

type SessionStatus struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	Control string `json:"control"`
}

func NewManager(cm *config.Manager, dataDir string) *Manager {
	m := &Manager{
		cm:           cm,
		dataDir:      dataDir,
		sessions:     map[string]*Session{},
		settingsPath: filepath.Join(dataDir, "browser", "settings.json"),
	}
	if cfg := cm.Get(); cfg != nil {
		m.enabled = cfg.BrowserOn()
	}
	if raw, err := os.ReadFile(m.settingsPath); err == nil {
		var s settings
		if json.Unmarshal(raw, &s) == nil {
			m.enabled = s.Enabled
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
		return config.BrowserConfig{MaxBrowsers: 2, IdleMinutes: 10, FPS: 8, Quality: 60}
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
	raw, _ := json.Marshal(settings{Enabled: m.enabled})
	tmp := m.settingsPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.settingsPath)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	enabled := m.enabled
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
		IdleMinutes:  cfg.IdleMinutes,
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
	if len(m.sessions) >= cfg.MaxBrowsers {
		m.mu.Unlock()
		return nil, ErrCapacity
	}
	chrome := DetectChrome(cfg.ChromePath)
	if chrome.Path == "" {
		m.mu.Unlock()
		return nil, ErrChromeMissing
	}
	profile := filepath.Join(m.dataDir, "browser", "profiles", profileKey(sessionID))
	if err := os.MkdirAll(profile, 0o700); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	s := &Session{
		id:       sessionID,
		profile:  profile,
		lastUsed: time.Now(),
		control:  "agent",
		viewers:  map[chan Frame]struct{}{},
		ready:    make(chan struct{}),
	}
	m.sessions[sessionID] = s
	m.mu.Unlock()

	// Start outside the manager lock: a slow first Chrome must not block
	// status calls or other sessions. Concurrent callers wait on s.ready.
	err := s.start(chrome.Path, cfg)
	s.finishStart(err)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.lastErr = err.Error()
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Lock()
	m.chrome = chrome
	m.mu.Unlock()
	return s, nil
}

func (s *Session) finishStart(err error) {
	s.readyOnce.Do(func() {
		s.mu.Lock()
		s.startErr = err
		s.mu.Unlock()
		close(s.ready)
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
	s.viewers[ch] = struct{}{}
	s.lastUsed = time.Now()
	s.mu.Unlock()
	s.startScreencast()
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.viewers, ch)
			close(ch)
			s.mu.Unlock()
			s.stopScreencast()
		})
	}
	return ch, unsub
}

func (s *Session) Status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionStatus{ID: s.id, Running: s.started && !s.closed, URL: s.url, Title: s.title, Control: s.control}
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
	m.mu.Unlock()
	if s != nil {
		s.close()
	}
}

func (s *Session) close() {
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
	started := s.started
	s.mu.Unlock()
	s.finishStart(errSessionStopped)
	if s.cancel != nil {
		s.cancel()
	}
	if s.allocCxl != nil {
		s.allocCxl()
	}
	_ = started
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		sessions = append(sessions, s)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
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
			}
		}
		m.mu.Unlock()
		for _, s := range close {
			s.close()
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
