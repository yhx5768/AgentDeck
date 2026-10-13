package server

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"cc-webui/internal/store"
)

func statusEv(state string) *store.Event {
	return &store.Event{Kind: "status", Ts: time.Now(), Data: json.RawMessage(`{"state":"` + state + `"}`)}
}

func userEv() *store.Event {
	return &store.Event{Kind: "user", Ts: time.Now(), Data: json.RawMessage(`{"text":"hi"}`)}
}

// lastStates extracts the states of the status events a slice ends with
// (after any repair), for compact assertions.
func lastState(evs []*store.Event) string {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != "status" {
			continue
		}
		var d struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(evs[i].Data, &d)
		return d.State
	}
	return ""
}

func TestCloseStaleTurnStart(t *testing.T) {
	cases := []struct {
		name   string
		events []*store.Event
		// wantClosed: the dangling turn_start gets a closing interrupted event
		// appended; wantLast is the state of the final status event after repair.
		wantAppended bool
		wantLast     string
	}{
		{
			name:         "dangling turn_start at the end gets closed",
			events:       []*store.Event{userEv(), statusEv("turn_start")},
			wantAppended: true,
			wantLast:     "interrupted",
		},
		{
			name:         "dangling turn_start after older completed turns",
			events:       []*store.Event{userEv(), statusEv("turn_start"), statusEv("turn_end"), userEv(), statusEv("turn_start")},
			wantAppended: true,
			wantLast:     "interrupted",
		},
		{
			name:     "log closed by turn_end stays untouched",
			events:   []*store.Event{userEv(), statusEv("turn_start"), statusEv("turn_end")},
			wantLast: "turn_end",
		},
		{
			name:     "log closed by error stays untouched",
			events:   []*store.Event{userEv(), statusEv("turn_start"), statusEv("error")},
			wantLast: "error",
		},
		{
			name:     "log closed by interrupted stays untouched",
			events:   []*store.Event{userEv(), statusEv("turn_start"), statusEv("interrupted")},
			wantLast: "interrupted",
		},
		{
			name:     "user echo without any status stays untouched",
			events:   []*store.Event{userEv()},
			wantLast: "",
		},
		{
			name:   "empty log stays empty",
			events: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := closeStaleTurnStart(c.events)
			if c.events == nil && got == nil {
				return
			}
			if c.wantAppended && len(got) != len(c.events)+1 {
				t.Fatalf("len = %d, want %d (one interrupted appended)", len(got), len(c.events)+1)
			}
			if !c.wantAppended && len(got) != len(c.events) {
				t.Fatalf("len = %d, want %d (unchanged)", len(got), len(c.events))
			}
			if c.wantLast != "" && lastState(got) != c.wantLast {
				t.Fatalf("last status state = %q, want %q", lastState(got), c.wantLast)
			}
			if c.wantLast == "" && lastState(got) != "" {
				t.Fatalf("expected no status events, got trailing %q", lastState(got))
			}
		})
	}
}

// 同一批剪贴板图片在浏览器里全叫 image.png。覆盖写会让第二张顶掉第一张，
// 一轮里的 N 张图就变成「最后一张 × N」——这正是回归点。
func TestWriteUploadExclusiveNoClobber(t *testing.T) {
	dir := t.TempDir()
	want := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
	gotNames := make([]string, 0, len(want))
	paths := make([]string, 0, len(want))
	for i, data := range want {
		name, path, err := writeUploadExclusive(dir, "image.png", data)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		gotNames = append(gotNames, name)
		paths = append(paths, path)
	}
	if gotNames[0] != "image.png" || gotNames[1] != "image-2.png" || gotNames[2] != "image-3.png" {
		t.Fatalf("names = %v, want [image.png image-2.png image-3.png]", gotNames)
	}
	for i, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(b, want[i]) {
			t.Fatalf("%s content = %q, want %q (clobbered)", path, b, want[i])
		}
	}
}

func TestWriteUploadExclusiveNoExt(t *testing.T) {
	dir := t.TempDir()
	name1, _, err := writeUploadExclusive(dir, "README", []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	name2, _, err := writeUploadExclusive(dir, "README", []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	if name1 != "README" || name2 != "README-2" {
		t.Fatalf("names = %q/%q, want README/README-2", name1, name2)
	}
}

// O_EXCL 是原子的：同名并发上传不能都挑到同一个后缀，否则又变回互相覆盖。
func TestWriteUploadExclusiveConcurrent(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	names := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names[i], _, errs[i] = writeUploadExclusive(dir, "image.png", []byte{byte('a' + i)})
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("write %d: %v", i, errs[i])
		}
		if seen[names[i]] {
			t.Fatalf("duplicate stored name %q", names[i])
		}
		seen[names[i]] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("files on disk = %d, want %d", len(entries), n)
	}
}
