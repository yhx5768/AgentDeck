package browser

import (
	"testing"

	"cc-webui/internal/config"
)

func TestViewerCloseAndUnsubscribeDoNotDoubleClose(t *testing.T) {
	s := &Session{viewers: map[chan Frame]struct{}{}, ready: make(chan struct{})}
	ch, unsub := s.Subscribe()
	s.close()
	unsub()
	if _, ok := <-ch; ok {
		t.Fatal("viewer channel should be closed by Session.close")
	}
}

func TestUnsubscribeThenCloseDoesNotPanic(t *testing.T) {
	s := &Session{viewers: map[chan Frame]struct{}{}, ready: make(chan struct{})}
	ch, unsub := s.Subscribe()
	unsub()
	s.close()
	if _, ok := <-ch; ok {
		t.Fatal("viewer channel should be closed by unsubscribe")
	}
}

func TestSubscribeAfterCloseIsAlreadyClosed(t *testing.T) {
	s := &Session{viewers: map[chan Frame]struct{}{}, ready: make(chan struct{})}
	s.close()
	ch, unsub := s.Subscribe()
	unsub()
	if _, ok := <-ch; ok {
		t.Fatal("late viewer should get a closed channel")
	}
}

func TestProfileNameModes(t *testing.T) {
	cm := config.NewManager("", &config.Config{DataDir: t.TempDir(), Browser: config.BrowserConfig{}})
	m := NewManager(cm, t.TempDir())
	if got := m.profileName("s1"); got != "session:s1" {
		t.Fatalf("session profile = %q", got)
	}
	if got := profileDirName("session:s1"); got != "s1" {
		t.Fatalf("session dir = %q", got)
	}
	cm2 := config.NewManager("", &config.Config{DataDir: t.TempDir(), Browser: config.BrowserConfig{ProfileKey: "me"}})
	m2 := NewManager(cm2, t.TempDir())
	m2.sharedProfile = true
	if got := m2.profileName("s1"); got != "shared:me" {
		t.Fatalf("shared profile = %q", got)
	}
	if got := profileDirName("shared:me"); got != "shared-me" {
		t.Fatalf("shared dir = %q", got)
	}
}

func TestSetSharedProfilePersists(t *testing.T) {
	dir := t.TempDir()
	cm := config.NewManager("", &config.Config{DataDir: dir, Browser: config.BrowserConfig{ProfileKey: "me"}})
	m := NewManager(cm, dir)
	if got := m.Status().ProfileMode; got != "session" {
		t.Fatalf("default profile mode = %q", got)
	}
	st, err := m.SetSharedProfile(true)
	if err != nil {
		t.Fatalf("enable shared: %v", err)
	}
	if st.ProfileMode != "shared" || st.ProfileKey != "me" {
		t.Fatalf("shared status = %#v", st)
	}
	m2 := NewManager(cm, dir)
	if got := m2.Status().ProfileMode; got != "shared" {
		t.Fatalf("persisted profile mode = %q", got)
	}
	if _, err := m2.SetSharedProfile(false); err != nil {
		t.Fatalf("disable shared: %v", err)
	}
	if got := m2.Status().ProfileMode; got != "session" {
		t.Fatalf("disabled profile mode = %q", got)
	}
}
