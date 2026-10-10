package runner

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cc-webui/internal/store"
)

// Group chat orchestrator (parallel semantics).
//
// A group owns one shadow member session per participant (store.Session with
// GroupID set, hidden from the sidebar). A group turn fans the user's message
// out to all targeted members CONCURRENTLY: every member gets the same history
// snapshot composed into its prompt (own prior replies framed as "yours",
// peers' as "[来自 X 的回复]"), so same-turn outputs are invisible to each
// other — like humans typing at once. Turns are sequential ACROSS rounds: a
// new message while a turn runs gets 409 turn_busy.
//
// Everything engine-specific is reused untouched: dispatch goes through
// Enqueue (resume context, permission hook, SSE all behave exactly like a
// single chat), and member sessions stay individually replayable. The group
// itself keeps an independent transcript (data/groups/<gid>/transcript.jsonl)
// of clean user/member texts; live member text blocks are forwarded into the
// group channel as they complete. Tool plumbing stays in the member view —
// the group timeline is text only.

// GroupMember is one participant's shadow session.
type GroupMember struct {
	ID       string `json:"id"`   // shadow session id
	Name     string `json:"name"` // unique within the group (@name)
	CLI      string `json:"cli"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// Cursor is the group-log length this member has consumed: the next
	// dispatch injects entries[Cursor:] (minus its own, which live in its
	// resume context already).
	Cursor int `json:"cursor"`
}

// GroupMeta is data/groups/<gid>/meta.json.
type GroupMeta struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Cwd       string        `json:"cwd"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Members   []GroupMember `json:"members"`
}

// GroupEntry is one line of data/groups/<gid>/transcript.jsonl.
type GroupEntry struct {
	ID         string   `json:"id"`
	Ts         int64    `json:"ts"` // unix millis
	TurnID     string   `json:"turn_id"`
	Kind       string   `json:"kind"` // "user" | "member"
	MemberID   string   `json:"member_id,omitempty"`
	MemberName string   `json:"member_name,omitempty"`
	Recipients []string `json:"recipients,omitempty"` // member ids (user kind)
	Text       string   `json:"text"`
}

// GroupEvent is the live payload fanned out on the group channel
// ("group:"+gid runtime subs, Kind="group").
type GroupEvent struct {
	Type       string   `json:"type"` // user|member_begin|member_text|member_end|turn_end
	TurnID     string   `json:"turn_id,omitempty"`
	MemberID   string   `json:"member_id,omitempty"`
	MemberName string   `json:"member_name,omitempty"`
	Recipients []string `json:"recipients,omitempty"`
	Text       string   `json:"text,omitempty"`
	OK         bool     `json:"ok,omitempty"`
	Error      string   `json:"error,omitempty"`
	Ts         int64    `json:"ts"`
}

func groupRuntimeKey(gid string) string { return "group:" + gid }

func newGroupID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type groupTurnState struct {
	turnID string
	abort  chan struct{}
	done   chan struct{}
}

type groupState struct {
	mu     sync.Mutex
	active map[string]*groupTurnState // gid -> in-flight turn
}

func newGroupState() *groupState { return &groupState{active: map[string]*groupTurnState{}} }

// GroupsDir returns data/groups (absolute; DataDir already is).
func (m *Manager) GroupsDir() string { return filepath.Join(m.cfg().DataDir, "groups") }

func (m *Manager) groupDir(gid string) string { return filepath.Join(m.GroupsDir(), gid) }

func (m *Manager) groupMetaPath(gid string) string {
	return filepath.Join(m.groupDir(gid), "meta.json")
}

func (m *Manager) groupLogPath(gid string) string {
	return filepath.Join(m.groupDir(gid), "transcript.jsonl")
}

// ReadGroupMeta loads a group's meta.json.
func (m *Manager) ReadGroupMeta(gid string) (*GroupMeta, error) {
	raw, err := os.ReadFile(m.groupMetaPath(gid))
	if err != nil {
		return nil, err
	}
	var meta GroupMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// WriteGroupMeta persists meta.json atomically (0600).
func (m *Manager) WriteGroupMeta(meta *GroupMeta) error {
	if err := os.MkdirAll(m.groupDir(meta.ID), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.groupMetaPath(meta.ID) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.groupMetaPath(meta.ID))
}

// ReadGroupLog returns the group's transcript entries in order. Missing file
// (fresh group) is empty, not an error.
func (m *Manager) ReadGroupLog(gid string) ([]GroupEntry, error) {
	raw, err := os.ReadFile(m.groupLogPath(gid))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []GroupEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e GroupEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate one corrupt line, keep the rest
		}
		out = append(out, e)
	}
	return out, nil
}

// AppendGroupLog appends one entry (0600 file, 0700 dir).
func (m *Manager) AppendGroupLog(gid string, e GroupEntry) error {
	if err := os.MkdirAll(m.groupDir(gid), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(m.groupLogPath(gid), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

// BroadcastGroup fans one live event to group subscribers.
func (m *Manager) BroadcastGroup(gid string, ev GroupEvent) {
	ev.Ts = time.Now().UnixMilli()
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	m.broadcastGroupRaw(gid, "group", raw)
}

func (m *Manager) broadcastGroupRaw(gid, kind string, raw []byte) {
	r := m.rt(groupRuntimeKey(gid))
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.nextSyntheticSeq(m.st, groupRuntimeKey(gid))
	sev := &store.Event{Seq: seq, Ts: time.Now(), Kind: kind, Data: raw}
	for ch := range r.subs {
		select {
		case ch <- sev:
		default:
		}
	}
}

// forwardGroupPermission relays a member's approval prompt into the group
// channel so the group view can answer it: same kind/data, plus the member
// session id the resolve POST needs.
func (m *Manager) forwardGroupPermission(gid string, mb *GroupMember, ev *store.Event) {
	var payload map[string]any
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		return
	}
	payload["member_id"] = mb.ID
	payload["member_name"] = mb.Name
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	m.broadcastGroupRaw(gid, ev.Kind, raw)
}

// SubscribeGroup registers a listener on the group channel.
func (m *Manager) SubscribeGroup(gid string) (<-chan *store.Event, func()) {
	return m.Subscribe(groupRuntimeKey(gid))
}

// GroupBusy reports whether the group has a turn in flight.
func (m *Manager) GroupBusy(gid string) bool {
	m.grp.mu.Lock()
	defer m.grp.mu.Unlock()
	_, ok := m.grp.active[gid]
	return ok
}

// GroupStop aborts the in-flight turn (members are Stopped individually;
// the wait loop exits as each goes idle).
func (m *Manager) GroupStop(gid string) bool {
	m.grp.mu.Lock()
	st, ok := m.grp.active[gid]
	m.grp.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-st.done:
	default:
		close(st.abort)
	}
	for _, mid := range m.groupMemberIDs(gid) {
		m.Stop(mid)
	}
	return true
}

func (m *Manager) groupMemberIDs(gid string) []string {
	meta, err := m.ReadGroupMeta(gid)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(meta.Members))
	for _, mb := range meta.Members {
		ids = append(ids, mb.ID)
	}
	return ids
}

var (
	errGroupBusy    = fmt.Errorf("turn_busy")
	errGroupNoRCT   = fmt.Errorf("no recipients")
	errGroupUnknown = fmt.Errorf("unknown member")
)

// GroupSend fans text out to recipients (empty = all members) and returns the
// turn id. The turn runs detached: member_begin/text/end + turn_end events
// arrive on the group channel.
func (m *Manager) GroupSend(gid, text string, recipients []string) (string, error) {
	meta, err := m.ReadGroupMeta(gid)
	if err != nil {
		return "", err
	}
	m.grp.mu.Lock()
	if _, ok := m.grp.active[gid]; ok {
		m.grp.mu.Unlock()
		return "", errGroupBusy
	}
	st := &groupTurnState{turnID: newGroupID(), abort: make(chan struct{}), done: make(chan struct{})}
	m.grp.active[gid] = st
	m.grp.mu.Unlock()
	finish := func() {
		m.grp.mu.Lock()
		if cur, ok := m.grp.active[gid]; ok && cur == st {
			delete(m.grp.active, gid)
		}
		m.grp.mu.Unlock()
		close(st.done)
	}

	byID := map[string]*GroupMember{}
	byName := map[string]*GroupMember{}
	for i := range meta.Members {
		mb := &meta.Members[i]
		byID[mb.ID] = mb
		byName[strings.ToLower(mb.Name)] = mb
	}
	var targets []*GroupMember
	if len(recipients) == 0 {
		for i := range meta.Members {
			targets = append(targets, &meta.Members[i])
		}
	} else {
		for _, r := range recipients {
			mb, ok := byID[r]
			if !ok {
				mb, ok = byName[strings.ToLower(r)]
			}
			if !ok {
				finish()
				return "", fmt.Errorf("%w: %s", errGroupUnknown, r)
			}
			targets = append(targets, mb)
		}
	}
	if len(targets) == 0 {
		finish()
		return "", errGroupNoRCT
	}
	for _, t := range targets {
		if m.Busy(t.ID) {
			finish()
			return "", errGroupBusy
		}
	}

	history, err := m.ReadGroupLog(gid)
	if err != nil {
		finish()
		return "", err
	}
	targetIDs := make([]string, 0, len(targets))
	for _, t := range targets {
		targetIDs = append(targetIDs, t.ID)
	}
	now := time.Now().UnixMilli()
	if err := m.AppendGroupLog(gid, GroupEntry{
		ID: newGroupID(), Ts: now, TurnID: st.turnID,
		Kind: "user", Recipients: targetIDs, Text: text,
	}); err != nil {
		log.Printf("group %s: append user entry: %v", gid, err)
	}
	m.BroadcastGroup(gid, GroupEvent{Type: "user", TurnID: st.turnID, Recipients: targetIDs, Text: text})

	// Compose each member's prompt from its unconsumed delta only: the
	// member's resume context already carries everything it ran, so only
	// user turns and PEERS' replies since its last dispatch are injected
	// (full history on first run). Own entries are skipped at compose time.
	type job struct {
		mb     *GroupMember
		prompt string
	}
	jobs := make([]job, 0, len(targets))
	for _, t := range targets {
		from := t.Cursor
		if from < 0 || from > len(history) {
			from = 0
		}
		jobs = append(jobs, job{mb: t, prompt: composeGroupPrompt(history[from:], t, text)})
		t.Cursor = len(history)
	}
	meta.UpdatedAt = time.Now()
	if err := m.WriteGroupMeta(meta); err != nil {
		log.Printf("group %s: write meta: %v", gid, err)
	}

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			m.runGroupMember(gid, st, j.mb, j.prompt)
		}(j)
	}
	go func() {
		wg.Wait()
		meta.UpdatedAt = time.Now()
		if err := m.WriteGroupMeta(meta); err != nil {
			log.Printf("group %s: write meta at turn end: %v", gid, err)
		}
		m.BroadcastGroup(gid, GroupEvent{Type: "turn_end", TurnID: st.turnID, OK: true})
		finish()
	}()
	return st.turnID, nil
}

// runGroupMember runs one member's leg: subscribe, enqueue, forward completed
// text blocks, wait for idle (or abort), then persist the member entry.
func (m *Manager) runGroupMember(gid string, st *groupTurnState, mb *GroupMember, prompt string) {
	m.BroadcastGroup(gid, GroupEvent{Type: "member_begin", TurnID: st.turnID, MemberID: mb.ID, MemberName: mb.Name})
	ch, unsub := m.Subscribe(mb.ID)
	defer unsub()
	if err := m.Enqueue(mb.ID, Message{Text: prompt}); err != nil {
		m.BroadcastGroup(gid, GroupEvent{Type: "member_end", TurnID: st.turnID, MemberID: mb.ID, MemberName: mb.Name, Error: err.Error()})
		return
	}
	var texts []string
	idle := false
	for !idle {
		select {
		case <-st.abort:
			// Stop() was already fanned out by GroupStop; just wait out idle.
		case <-time.After(300 * time.Millisecond):
		}
		// Drain everything available without blocking: text blocks stream in
		// as the model finishes them; idle is observed via Busy. Approval
		// prompts are relayed to the group channel so the group view (not
		// just the member view) can answer them.
	drain:
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					break drain
				}
				if ev.Kind == "permission_request" || ev.Kind == "permission_resolved" {
					m.forwardGroupPermission(gid, mb, ev)
					continue
				}
				if t := memberTextOf(ev); t != "" {
					texts = append(texts, t)
					m.BroadcastGroup(gid, GroupEvent{Type: "member_text", TurnID: st.turnID, MemberID: mb.ID, MemberName: mb.Name, Text: t})
				}
			default:
				break drain
			}
		}
		if !m.Busy(mb.ID) {
			// One last drain: the final blocks race turn_end.
			for {
				select {
				case ev, ok := <-ch:
					if !ok {
						break
					}
					if ev.Kind == "permission_request" || ev.Kind == "permission_resolved" {
						m.forwardGroupPermission(gid, mb, ev)
						continue
					}
					if t := memberTextOf(ev); t != "" {
						texts = append(texts, t)
						m.BroadcastGroup(gid, GroupEvent{Type: "member_text", TurnID: st.turnID, MemberID: mb.ID, MemberName: mb.Name, Text: t})
					}
				default:
					goto observed
				}
			}
		observed:
			idle = true
		}
	}
	text := strings.Join(texts, "\n\n")
	end := GroupEvent{Type: "member_end", TurnID: st.turnID, MemberID: mb.ID, MemberName: mb.Name, OK: true}
	if text == "" {
		end.OK = false
		end.Error = "no text output"
	} else if err := m.AppendGroupLog(gid, GroupEntry{
		ID: newGroupID(), Ts: time.Now().UnixMilli(), TurnID: st.turnID,
		Kind: "member", MemberID: mb.ID, MemberName: mb.Name, Text: text,
	}); err != nil {
		log.Printf("group %s: append member entry: %v", gid, err)
	} else {
		end.Text = text
	}
	m.BroadcastGroup(gid, end)
}

// memberTextOf extracts one assistant text block from a member stream event.
// Deltas (stream_event) are skipped: the completed block arrives separately,
// which is also what the member timeline renders.
func memberTextOf(ev *store.Event) string {
	if ev == nil || ev.Kind != "stream" {
		return ""
	}
	var d struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return ""
	}
	if d.Type != "assistant" {
		return ""
	}
	var parts []string
	for _, c := range d.Message.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// composeGroupPrompt renders the snapshot for one member: full user turns,
// peers' replies under [来自 X 的回复], own replies as "yours" (kept for
// framing even though resume context carries them — cheap and unambiguous).
func composeGroupPrompt(history []GroupEntry, target *GroupMember, current string) string {
	var b strings.Builder
	hasHistory := false
	for _, e := range history {
		switch e.Kind {
		case "user":
			if strings.TrimSpace(e.Text) == "" {
				continue
			}
			b.WriteString("USER: " + e.Text + "\n\n")
			hasHistory = true
		case "member":
			if strings.TrimSpace(e.Text) == "" {
				continue
			}
			if e.MemberID == target.ID {
				b.WriteString("你的上一条回复: " + e.Text + "\n\n")
			} else {
				name := e.MemberName
				if name == "" {
					name = "另一位成员"
				}
				b.WriteString("[来自 " + name + " 的回复]\n" + e.Text + "\n\n")
			}
			hasHistory = true
		}
	}
	if !hasHistory {
		return current
	}
	return "[群聊历史]\n" + b.String() + "[当前用户消息]\n" + current
}
