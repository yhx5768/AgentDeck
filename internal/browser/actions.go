package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func textResult(s string) Result {
	if len(s) > 20000 {
		s = s[:20000] + "\n…(truncated)"
	}
	return Result{Content: []Content{{Type: "text", Text: s}}}
}

func errorResult(s string) Result {
	r := textResult(s)
	r.IsError = true
	return r
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

var Tools = []Tool{
	{Name: "browser_status", Description: "Browser state: URL, title, control and whether it is running.", InputSchema: obj(map[string]any{})},
	{Name: "browser_open", Description: "Open an http(s) URL in the browser and return a page snapshot with element refs.", InputSchema: obj(map[string]any{"url": str("http(s) URL")}, "url")},
	{Name: "browser_navigate", Description: "Navigate the current tab: a URL, back, forward or reload.", InputSchema: obj(map[string]any{"url": str("http(s) URL"), "action": map[string]any{"type": "string", "enum": []string{"back", "forward", "reload"}}})},
	{Name: "browser_snapshot", Description: "Return the page's visible interactive elements with refs such as e12. Refs expire after navigation or reload.", InputSchema: obj(map[string]any{})},
	{Name: "browser_read", Description: "Read visible text from the page or one element.", InputSchema: obj(map[string]any{"ref": str("ref from snapshot"), "selector": str("CSS selector"), "offset": map[string]any{"type": "integer"}, "max_chars": map[string]any{"type": "integer"}})},
	{Name: "browser_screenshot", Description: "Take a PNG screenshot of the viewport, or the full page when full_page is true.", InputSchema: obj(map[string]any{"full_page": boolean("capture the whole page")})},
	{Name: "browser_click", Description: "Click an element by ref or CSS selector.", InputSchema: obj(map[string]any{"ref": str("ref from snapshot"), "selector": str("CSS selector")})},
	{Name: "browser_fill", Description: "Clear a field and fill it; optional submit presses Enter afterwards.", InputSchema: obj(map[string]any{"ref": str("ref from snapshot"), "selector": str("CSS selector"), "text": str("text to fill"), "submit": boolean("submit the form")}, "text")},
	{Name: "browser_type", Description: "Type text key by key into an element or the focused element.", InputSchema: obj(map[string]any{"ref": str("ref from snapshot"), "selector": str("CSS selector"), "text": str("text to type"), "clear": boolean("clear the field first")}, "text")},
	{Name: "browser_press", Description: "Press a key or chord, such as Enter, Escape, Control+a.", InputSchema: obj(map[string]any{"key": str("key or chord")}, "key")},
	{Name: "browser_wait", Description: "Wait for text, an element or a URL substring.", InputSchema: obj(map[string]any{"text": str("text to wait for"), "selector": str("CSS selector"), "url_includes": str("URL substring"), "timeout_ms": map[string]any{"type": "integer"}})},
	{Name: "browser_close", Description: "Close this session's browser. Saved logins are kept in its profile.", InputSchema: obj(map[string]any{})},
	{Name: "browser_restart", Description: "Restart this session's browser. Open pages are lost; logins are kept.", InputSchema: obj(map[string]any{})},
}

var refRE = regexp.MustCompile(`^e[0-9]{1,6}$`)

func selectorFor(args map[string]any) (string, error) {
	if ref, ok := args["ref"].(string); ok && ref != "" {
		if !refRE.MatchString(ref) {
			return "", fmt.Errorf("invalid ref %q", ref)
		}
		return fmt.Sprintf(`[data-ccwebui-ref=%q]`, ref), nil
	}
	if sel, ok := args["selector"].(string); ok && strings.TrimSpace(sel) != "" {
		if len(sel) > 2048 || strings.ContainsAny(sel, "\x00\r\n") {
			return "", fmt.Errorf("invalid selector")
		}
		return sel, nil
	}
	return "", fmt.Errorf("provide ref or selector")
}

func strArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func intArg(args map[string]any, key string, fallback int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return fallback
}

func boolArg(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}

// runAction bounds a CDP action so a stalled page cannot keep the HTTP
// handler (and the browser panel request) waiting forever. Cancelling the
// child context aborts the action, not the browser session.
func (s *Session) runAction(timeout time.Duration, actions ...chromedp.Action) error {
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	return chromedp.Run(ctx, actions...)
}

// Execute runs one MCP browser tool call. The caller already checked that the
// session exists and the browser switch is on.
func (m *Manager) Execute(ctx context.Context, sessionID, name string, args map[string]any) Result {
	if args == nil {
		args = map[string]any{}
	}
	switch name {
	case "browser_close":
		m.CloseSession(sessionID)
		return textResult("Browser closed. Saved logins are kept in this session's profile.")
	case "browser_restart":
		m.CloseSession(sessionID)
		if _, err := m.getOrCreate(sessionID); err != nil {
			return errorResult("Browser restart failed: " + err.Error())
		}
		return textResult("Browser restarted on one blank tab. Refs from before are gone; take a snapshot before acting.")
	case "browser_status":
		// Status is a read: it must not start a browser just to report that
		// none is running.
		st := m.SessionStatus(sessionID)
		return textResult(fmt.Sprintf("running=%v\nurl=%s\ntitle=%s\ncontrol=%s", st.Running, st.URL, st.Title, st.Control))
	}

	s, err := m.getOrCreate(sessionID)
	if err != nil {
		return errorResult(err.Error())
	}
	s.actionMu.Lock()
	defer s.actionMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	human := s.control == "human"
	if !closed && !human {
		s.lastUsed = time.Now()
	}
	s.mu.Unlock()
	if closed {
		return errorResult(errSessionStopped.Error())
	}
	if human {
		return errorResult(ErrHumanControl.Error() + "；等待用户交还浏览器后再继续。")
	}

	switch name {
	case "browser_open":
		url, err := ValidateURL(ctx, strArg(args, "url"), m.config(), m.selfListen())
		if err != nil {
			return errorResult(err.Error())
		}
		if err := s.runAction(30*time.Second, chromedp.Navigate(url)); err != nil {
			return errorResult("open failed: " + err.Error())
		}
		s.waitReady()
		s.updatePage()
		return textResult(s.snapshotText())
	case "browser_navigate":
		if action := strArg(args, "action"); action != "" {
			var js string
			switch action {
			case "back":
				js = "history.back()"
			case "forward":
				js = "history.forward()"
			case "reload":
				js = "location.reload()"
			default:
				return errorResult("action must be back, forward or reload")
			}
			if err := s.runAction(10*time.Second, chromedp.Evaluate(js, nil)); err != nil {
				return errorResult("navigate failed: " + err.Error())
			}
			time.Sleep(300 * time.Millisecond)
			s.updatePage()
			return textResult("Now at " + s.url + "\nTitle: " + s.title)
		}
		url, err := ValidateURL(ctx, strArg(args, "url"), m.config(), m.selfListen())
		if err != nil {
			return errorResult(err.Error())
		}
		if err := s.runAction(30*time.Second, chromedp.Navigate(url)); err != nil {
			return errorResult("navigate failed: " + err.Error())
		}
		s.waitReady()
		s.updatePage()
		return textResult(s.snapshotText())
	case "browser_snapshot":
		return textResult(s.snapshotText())
	case "browser_read":
		sel := ""
		if s, err := selectorFor(args); err == nil {
			sel = s
		}
		offset := intArg(args, "offset", 0)
		maxChars := intArg(args, "max_chars", 20000)
		if maxChars <= 0 || maxChars > 40000 {
			maxChars = 20000
		}
		js := fmt.Sprintf(`(() => { const el = %s; if (!el) return "__not_found__"; const t = (el.innerText || el.textContent || "").replace(/ /g, " "); return t.slice(%d, %d); })()`,
			jsElement(sel), offset, offset+maxChars)
		var out string
		if err := s.runAction(10*time.Second, chromedp.Evaluate(js, &out)); err != nil {
			return errorResult("read failed: " + err.Error())
		}
		if out == "__not_found__" {
			return errorResult("element not found")
		}
		return textResult(out)
	case "browser_screenshot":
		var buf []byte
		var err error
		if boolArg(args, "full_page") {
			err = s.runAction(20*time.Second, chromedp.FullScreenshot(&buf, 80))
		} else {
			err = s.runAction(20*time.Second, chromedp.CaptureScreenshot(&buf))
		}
		if err != nil {
			return errorResult("screenshot failed: " + err.Error())
		}
		s.updatePage()
		return Result{Content: []Content{
			{Type: "text", Text: "Screenshot of " + s.url + " (" + s.title + ")"},
			{Type: "image", Data: base64.StdEncoding.EncodeToString(buf), MimeType: "image/png"},
		}}
	case "browser_click":
		sel, err := selectorFor(args)
		if err != nil {
			return errorResult(err.Error())
		}
		if err := s.runAction(15*time.Second, chromedp.Click(sel, chromedp.ByQuery)); err != nil {
			return errorResult("click failed: " + err.Error())
		}
		time.Sleep(200 * time.Millisecond)
		s.updatePage()
		return textResult("Clicked. Now at " + s.url)
	case "browser_fill":
		sel, err := selectorFor(args)
		if err != nil {
			return errorResult(err.Error())
		}
		text := strArg(args, "text")
		js := fmt.Sprintf(`(() => {
			const el = %s; if (!el) return "__not_found__";
			el.focus();
			const proto = Object.getPrototypeOf(el);
			const setter = Object.getOwnPropertyDescriptor(proto, "value")?.set;
			if (setter) setter.call(el, %s); else el.value = %s;
			el.dispatchEvent(new Event("input", {bubbles:true}));
			el.dispatchEvent(new Event("change", {bubbles:true}));
			if (%s && el.form) el.form.requestSubmit();
			return "ok";
		})()`, jsElement(sel), jsonString(text), jsonString(text), fmt.Sprint(boolArg(args, "submit")))
		var out string
		if err := s.runAction(10*time.Second, chromedp.Evaluate(js, &out)); err != nil {
			return errorResult("fill failed: " + err.Error())
		}
		if out != "ok" {
			return errorResult("element not found")
		}
		s.updatePage()
		return textResult("Filled " + sel + ". Now at " + s.url)
	case "browser_type":
		sel, err := selectorFor(args)
		if err != nil {
			return errorResult(err.Error())
		}
		text := strArg(args, "text")
		if boolArg(args, "clear") {
			if err := s.runAction(10*time.Second, chromedp.Clear(sel, chromedp.ByQuery)); err != nil {
				return errorResult("clear failed: " + err.Error())
			}
		}
		if err := s.runAction(20*time.Second, chromedp.SendKeys(sel, text, chromedp.ByQuery)); err != nil {
			return errorResult("type failed: " + err.Error())
		}
		s.updatePage()
		return textResult("Typed into " + sel + ". Now at " + s.url)
	case "browser_press":
		key := strArg(args, "key")
		if key == "" {
			return errorResult("key is required")
		}
		// chromedp.KeyEvent handles chords such as Control+a.
		if err := s.runAction(10*time.Second, chromedp.KeyEvent(key)); err != nil {
			return errorResult("press failed: " + err.Error())
		}
		time.Sleep(100 * time.Millisecond)
		s.updatePage()
		return textResult("Pressed " + key + ". Now at " + s.url)
	case "browser_wait":
		timeout := time.Duration(intArg(args, "timeout_ms", 15000)) * time.Millisecond
		if timeout <= 0 || timeout > 60*time.Second {
			timeout = 15 * time.Second
		}
		if err := s.wait(ctx, args, timeout); err != nil {
			return errorResult(err.Error())
		}
		return textResult("Condition met. Now at " + s.url)
	default:
		return errorResult("unknown browser tool: " + name)
	}
}

func (s *Session) waitReady() {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	_ = chromedp.Run(ctx, chromedp.WaitReady("body", chromedp.ByQuery))
}

func (s *Session) updatePage() {
	var url, title string
	_ = s.runAction(5*time.Second, chromedp.Location(&url), chromedp.Title(&title))
	s.mu.Lock()
	s.url = url
	s.title = title
	s.mu.Unlock()
}

func (s *Session) snapshotText() string {
	var out string
	if err := s.runAction(10*time.Second, chromedp.Evaluate(snapshotJS, &out)); err != nil {
		return "snapshot failed: " + err.Error()
	}
	if len(out) > 20000 {
		out = out[:20000] + "\n…(truncated)"
	}
	return out
}

func (s *Session) wait(ctx context.Context, args map[string]any, timeout time.Duration) error {
	text := strArg(args, "text")
	sel := strArg(args, "selector")
	urlIncludes := strArg(args, "url_includes")
	deadline := time.Now().Add(timeout)
	for {
		js := "(() => {"
		conds := []string{}
		if text != "" {
			conds = append(conds, fmt.Sprintf(`(document.body && document.body.innerText.includes(%s))`, jsonString(text)))
		}
		if sel != "" {
			conds = append(conds, fmt.Sprintf(`!!document.querySelector(%s)`, jsonString(sel)))
		}
		if urlIncludes != "" {
			conds = append(conds, fmt.Sprintf(`location.href.includes(%s)`, jsonString(urlIncludes)))
		}
		if len(conds) == 0 {
			return fmt.Errorf("provide text, selector or url_includes")
		}
		js += "return " + strings.Join(conds, " && ") + "; })()"
		var ok bool
		if err := s.runAction(10*time.Second, chromedp.Evaluate(js, &ok)); err != nil {
			return fmt.Errorf("wait failed: %w", err)
		}
		if ok {
			s.updatePage()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func jsElement(sel string) string {
	if sel == "" {
		return "document.body"
	}
	return fmt.Sprintf("document.querySelector(%s)", jsonString(sel))
}

const snapshotJS = `(() => {
  const out = [];
  const seen = new Set();
  const els = document.querySelectorAll('a,button,input,textarea,select,[role="button"],[role="link"],[role="textbox"],[contenteditable="true"]');
  let i = 0;
  for (const el of els) {
    if (i >= 120) break;
    const r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) continue;
    const style = getComputedStyle(el);
    if (style.visibility === 'hidden' || style.display === 'none') continue;
    i++;
    const ref = 'e' + i;
    el.setAttribute('data-ccwebui-ref', ref);
    const tag = el.tagName.toLowerCase();
    let role = (el.getAttribute('role') || tag).toLowerCase();
    const password = el instanceof HTMLInputElement && el.type === 'password';
    let name = el.getAttribute('aria-label') || el.getAttribute('title')
      || (password ? '' : (el.innerText || el.value))
      || el.getAttribute('placeholder') || '';
    name = String(name).replace(/\s+/g, ' ').trim().slice(0, 160);
    let line = '[' + ref + '] ' + role;
    if (name) line += ' ' + JSON.stringify(name);
    if (el instanceof HTMLInputElement) {
      if (el.type === 'password') line += ' [value masked]';
      else if (el.value) line += ' [value=' + JSON.stringify(String(el.value).slice(0, 80)) + ']';
      line += ' [type=' + el.type + ']';
    }
    if (el.disabled) line += ' [disabled]';
    out.push(line);
  }
  return 'URL: ' + location.href + '\nTITLE: ' + document.title + '\n' + out.join('\n');
})()`

// InputEvent is the JSON body the browser panel posts. Coordinates are CSS
// pixels in the page viewport.
type InputEvent struct {
	Type       string  `json:"type"`
	Action     string  `json:"action"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     string  `json:"button"`
	Buttons    int64   `json:"buttons"`
	ClickCount int64   `json:"click_count"`
	Modifiers  int64   `json:"modifiers"`
	DeltaX     float64 `json:"delta_x"`
	DeltaY     float64 `json:"delta_y"`
	Key        string  `json:"key"`
	Code       string  `json:"code"`
	KeyCode    int64   `json:"key_code"`
	Text       string  `json:"text"`
	URL        string  `json:"url"`
	Delta      int     `json:"delta"`
}

func (m *Manager) Input(ctx context.Context, sessionID string, ev InputEvent) error {
	s, err := m.getOrCreate(sessionID)
	if err != nil {
		return err
	}
	s.actionMu.Lock()
	defer s.actionMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errSessionStopped
	}
	if ev.Type == "take" {
		s.control = "human"
		s.lastUsed = time.Now()
		s.mu.Unlock()
		return nil
	}
	if ev.Type == "release" {
		s.control = "agent"
		s.lastUsed = time.Now()
		s.mu.Unlock()
		return nil
	}
	human := s.control == "human"
	if human {
		s.lastUsed = time.Now()
	}
	s.mu.Unlock()
	if !human {
		return ErrHumanControl
	}
	switch ev.Type {
	case "navigate":
		url, err := ValidateURL(ctx, ev.URL, m.config(), m.selfListen())
		if err != nil {
			return err
		}
		if err := s.runAction(30*time.Second, chromedp.Navigate(url)); err != nil {
			return err
		}
		s.updatePage()
		return nil
	case "history":
		js := "history.back()"
		if ev.Delta > 0 {
			js = "history.forward()"
		}
		if err := s.runAction(10*time.Second, chromedp.Evaluate(js, nil)); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
		s.updatePage()
		return nil
	case "reload":
		if err := s.runAction(10*time.Second, chromedp.Evaluate("location.reload()", nil)); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
		s.updatePage()
		return nil
	case "text":
		return s.runAction(10*time.Second, input.InsertText(ev.Text))
	case "mouse":
		typ := input.MouseMoved
		switch ev.Action {
		case "down":
			typ = input.MousePressed
		case "up":
			typ = input.MouseReleased
		}
		button := input.None
		switch ev.Button {
		case "left":
			button = input.Left
		case "right":
			button = input.Right
		case "middle":
			button = input.Middle
		}
		return chromedp.Run(s.ctx, input.DispatchMouseEvent(typ, ev.X, ev.Y).
			WithButton(button).WithButtons(ev.Buttons).WithClickCount(ev.ClickCount).WithModifiers(input.Modifier(ev.Modifiers)))
	case "wheel":
		return chromedp.Run(s.ctx, input.DispatchMouseEvent(input.MouseWheel, ev.X, ev.Y).
			WithDeltaX(ev.DeltaX).WithDeltaY(ev.DeltaY).WithModifiers(input.Modifier(ev.Modifiers)))
	case "key":
		typ := input.KeyDown
		if ev.Action == "up" {
			typ = input.KeyUp
		}
		p := input.DispatchKeyEvent(typ).WithKey(ev.Key).WithCode(ev.Code).
			WithWindowsVirtualKeyCode(ev.KeyCode).WithModifiers(input.Modifier(ev.Modifiers))
		if typ == input.KeyDown && ev.Text != "" {
			p = p.WithText(ev.Text).WithUnmodifiedText(ev.Text)
		}
		return s.runAction(10*time.Second, p)
	default:
		return fmt.Errorf("unknown input type %q", ev.Type)
	}
}
