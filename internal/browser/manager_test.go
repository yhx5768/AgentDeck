package browser

import "testing"

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
