// Command shared-profile-poc proves the core of the shared-profile multi-window
// design: one headless Chrome process, several targets/windows, one user-data
// dir, so cookies and localStorage are shared while each target keeps its own
// page. It is a standalone PoC; the AgentDeck manager is not wired to it yet.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><h1>shared profile targets</h1></body></html>`)
	}))
	defer srv.Close()

	profile, err := os.MkdirTemp("", "ccwebui-shared-profile-poc-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(profile)

	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ExecPath(chromePath()),
		chromedp.UserDataDir(profile),
		chromedp.WindowSize(1000, 700),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()
	rootCtx, rootCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(func(string, ...interface{}) {}), chromedp.WithErrorf(func(string, ...interface{}) {}))
	defer rootCancel()

	startCtx, startCancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer startCancel()
	if err := chromedp.Run(startCtx); err != nil {
		log.Fatalf("start Chrome: %v", err)
	}
	root := chromedp.FromContext(rootCtx)
	create := func(newWindow bool) target.ID {
		var id target.ID
		err := chromedp.Run(rootCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			id, err = target.CreateTarget("about:blank").WithNewWindow(newWindow).Do(cdp.WithExecutor(ctx, root.Browser))
			return err
		}))
		if err != nil {
			log.Fatalf("create target newWindow=%v: %v", newWindow, err)
		}
		return id
	}
	attach := func(id target.ID) (context.Context, context.CancelFunc) {
		ctx, cancel := chromedp.NewContext(rootCtx, chromedp.WithTargetID(id))
		return ctx, cancel
	}
	run := func(ctx context.Context, actions ...chromedp.Action) {
		if err := chromedp.Run(ctx, actions...); err != nil {
			log.Fatalf("target action: %v", err)
		}
	}

	window1 := create(true)
	window2 := create(true)
	tab := create(false)
	ctx1, cancel1 := attach(window1)
	defer cancel1()
	ctx2, cancel2 := attach(window2)
	defer cancel2()
	ctxTab, cancelTab := attach(tab)
	defer cancelTab()

	run(ctx1, chromedp.Navigate(srv.URL))
	run(ctx1, chromedp.Evaluate(`document.cookie = "shared_cookie=yes; path=/"; localStorage.setItem("shared_ls", "yes"); "set"`, nil))
	run(ctx2, chromedp.Navigate(srv.URL))
	var cookie, storage string
	run(ctx2, chromedp.Evaluate(`document.cookie`, &cookie), chromedp.Evaluate(`localStorage.getItem("shared_ls")`, &storage))
	run(ctxTab, chromedp.Navigate(srv.URL))
	var tabCookie, tabStorage string
	run(ctxTab, chromedp.Evaluate(`document.cookie`, &tabCookie), chromedp.Evaluate(`localStorage.getItem("shared_ls")`, &tabStorage))

	var targets []*target.Info
	run(rootCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		targets, err = target.GetTargets().Do(cdp.WithExecutor(ctx, root.Browser))
		return err
	}))
	pages := 0
	for _, info := range targets {
		if info.Type == "page" {
			pages++
		}
	}
	fmt.Printf("window1=%s window2=%s tab=%s\n", window1, window2, tab)
	fmt.Printf("window2 sees cookie=%q localStorage=%q\n", cookie, storage)
	fmt.Printf("tab sees cookie=%q localStorage=%q\n", tabCookie, tabStorage)
	fmt.Printf("page targets=%d (two windows + one tab + initial blank expected)\n", pages)
	if cookie != "shared_cookie=yes" || storage != "yes" || tabCookie != "shared_cookie=yes" || tabStorage != "yes" {
		log.Fatal("shared profile did not share cookie/localStorage across targets")
	}
	// Close the per-session targets, not the shared browser; the browser is
	// closed by the context cancellation at process exit.
	_ = chromedp.Run(rootCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, id := range []target.ID{window1, window2, tab} {
			if err := target.CloseTarget(id).Do(cdp.WithExecutor(ctx, root.Browser)); err != nil {
				return err
			}
		}
		return nil
	}))
	fmt.Println("PASS: one Chrome profile, independent targets, shared cookie/localStorage")
}

func chromePath() string {
	for _, p := range []string{"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "google-chrome"
}
