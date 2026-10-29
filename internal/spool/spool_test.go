package spool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hive-RSI-Lab/hive-spool/internal/hive"
)

// fakeHive is the slice of the daemon's API the channel hook reaches: post in
// a chat, read the journal, report.
type fakeHive struct {
	srv      *httptest.Server
	mu       sync.Mutex
	events   []hive.Event
	posts    []hive.Message
	projects map[string]bool
	n        int
}

func newFakeHive(t *testing.T) *fakeHive {
	f := &fakeHive{projects: map[string]bool{"hive": true}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad token"})
			return
		}
		var b struct{ Chat, Thread, Text string }
		json.NewDecoder(r.Body).Decode(&b)
		if p, ok := strings.CutPrefix(b.Chat, "project:"); ok && !f.projects[p] {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		f.mu.Lock()
		f.n++
		m := hive.Message{ID: fmt.Sprintf("m%d", f.n), Chat: b.Chat, Thread: b.Thread, From: "spool", Name: "spool", Text: b.Text}
		if m.Thread == "" {
			m.Thread = m.ID
		}
		f.posts = append(f.posts, m)
		f.mu.Unlock()
		f.emit("chat.posted", m)
		json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		f.mu.Lock()
		out := []hive.Event{}
		for _, e := range f.events {
			if e.Seq > after && len(out) < 2000 {
				out = append(out, e)
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/module/report", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHive) emit(kind string, data any) {
	raw, _ := json.Marshal(data)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, hive.Event{Seq: int64(len(f.events) + 1), At: time.Now().UTC(), Kind: kind, Data: raw})
}

func (f *fakeHive) sent() []hive.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hive.Message(nil), f.posts...)
}

// reply is the lead (or anyone) posting in a chat thread.
func (f *fakeHive) reply(chat, thread, from, text string) {
	f.emit("chat.posted", map[string]string{"chat": chat, "thread": thread, "id": "r" + text, "from": from, "name": from, "text": text})
}

// task is a task.saved line as the work module journals it: the whole task.
func (f *fakeHive) task(id, chat, thread, stage string, more map[string]any) {
	t := map[string]any{"id": id, "project": "hive", "title": "task " + id, "kind": "work", "stage": stage, "chat": chat, "thread": thread, "issue": map[string]any{"number": 7, "url": "https://github.com/o/hive/issues/7"}, "pr": map[string]any{}}
	for k, v := range more {
		t[k] = v
	}
	f.emit("task.saved", t)
}

type spoolT struct {
	t    *testing.T
	hive *fakeHive
	home string
	cfg  Config
	door *Door
}

func newSpool(t *testing.T) *spoolT {
	dir := t.TempDir()
	s := &spoolT{t: t, hive: newFakeHive(t), home: filepath.Join(dir, "home"), cfg: Config{Inbox: filepath.Join(dir, "hive-shadow", "inbox"), Outbox: filepath.Join(dir, "hive-shadow", "outbox")}}
	os.MkdirAll(s.home, 0o700)
	// Something already in the journal before the module starts: never the spool's.
	s.hive.reply("lead", "old", "lead-1", "history")
	s.restart()
	return s
}

func (s *spoolT) restart() {
	s.door = New(s.cfg, &hive.Client{API: s.hive.srv.URL, Token: "tok"}, "spool", s.home)
	if err := s.door.Start(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

// write appends raw to the inbox as the relay does, newline and all.
func (s *spoolT) write(raw string) {
	s.t.Helper()
	f, err := os.OpenFile(s.cfg.Inbox, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		s.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(raw); err != nil {
		s.t.Fatal(err)
	}
}

// ask appends a top-level message (thread "") or a reply as one whole line.
func (s *spoolT) ask(ts, thread, text string) string {
	id := "C0AAAAAAAAA/" + ts
	var th any
	if thread != "" {
		th = thread
	}
	raw, _ := json.Marshal(map[string]any{"id": id, "channel": "C0AAAAAAAAA", "thread": th, "user": "U0BBBBBBBBB", "text": text, "posted": 1793300000.5, "heard": 1793300042.5})
	s.write(string(raw) + "\n")
	return id
}

func (s *spoolT) step() {
	s.t.Helper()
	if err := s.door.Step(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

// out reads the outbox's lines, each checked whole.
func (s *spoolT) out() []Out {
	s.t.Helper()
	raw, err := os.ReadFile(s.cfg.Outbox)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		s.t.Fatalf("outbox ends in a partial line: %q", raw)
	}
	var outs []Out
	for _, l := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if l == "" {
			continue
		}
		var o Out
		if err := json.Unmarshal([]byte(l), &o); err != nil {
			s.t.Fatalf("outbox line %q: %v", l, err)
		}
		outs = append(outs, o)
	}
	return outs
}

func (s *spoolT) req(id string) *Request {
	s.t.Helper()
	r := s.door.State().Requests[id]
	if r == nil {
		s.t.Fatalf("no request %s", id)
	}
	return r
}

// The two lines autobox gave as the relay's own (chat 261029-224842-e2d6677df4).
const (
	sampleTop   = `{"id": "C0AAAAAAAAA/1793300000.123456", "channel": "C0AAAAAAAAA", "thread": null, "user": "U0BBBBBBBBB", "text": "can you update the sizing sheet for the north site", "posted": 1793300000.123456, "heard": 1793300042.51}`
	sampleReply = `{"id": "C0AAAAAAAAA/1793300120.000200", "channel": "C0AAAAAAAAA", "thread": "1793300000.123456", "user": "U0CCCCCCCCC", "text": "<@U0BBBBBBBBB> use the October numbers", "posted": 1793300120.0002, "heard": 1793300162.9}`
)

func TestTopLevelStartsAThreadAndAReplyJoinsIt(t *testing.T) {
	s := newSpool(t)
	s.write(sampleTop + "\n" + sampleReply + "\n")
	s.step()
	posts := s.hive.sent()
	if len(posts) != 2 || posts[0].Chat != "lead" || posts[0].Thread != posts[0].ID {
		t.Fatalf("want a new thread in the lead's chat and the reply in it, got %+v", posts)
	}
	if !strings.Contains(posts[0].Text, "can you update the sizing sheet for the north site") || !strings.Contains(posts[0].Text, "C0AAAAAAAAA/1793300000.123456") || !strings.Contains(posts[0].Text, "U0BBBBBBBBB") {
		t.Errorf("post doesn't carry the request: %q", posts[0].Text)
	}
	if posts[1].Chat != "lead" || posts[1].Thread != posts[0].Thread || !strings.Contains(posts[1].Text, "<@U0BBBBBBBBB> use the October numbers") {
		t.Errorf("the reply belongs in its parent's thread: %+v", posts[1])
	}
	r := s.req("C0AAAAAAAAA/1793300000.123456")
	if r.Status != "open" || r.Chat != "lead" || r.Thread != posts[0].Thread || len(r.Lines) != 2 || len(r.Replies) != 0 {
		t.Errorf("request: %+v", r)
	}
	if len(s.out()) != 0 {
		t.Errorf("nothing to answer yet: %+v", s.out())
	}
	info, _ := os.Stat(s.cfg.Inbox)
	if st := s.door.State(); st.Inbox != info.Size() {
		t.Errorf("offset %d, inbox %d bytes", st.Inbox, info.Size())
	}

	// Steps after, a restart, and the same line sent again: still once each.
	s.step()
	s.restart()
	s.write(sampleTop + "\n")
	s.step()
	if n := len(s.hive.sent()); n != 2 {
		t.Fatalf("posted %d times, want twice", n)
	}
	if _, err := os.Stat(s.cfg.Inbox); err != nil {
		t.Fatal("the inbox is never moved:", err)
	}
}

func TestPartialLineWaits(t *testing.T) {
	s := newSpool(t)
	s.write(sampleTop[:40])
	s.step()
	if len(s.hive.sent()) != 0 || s.door.State().Inbox != 0 {
		t.Fatal("took half a line")
	}
	s.write(sampleTop[40:])
	s.restart()
	s.step()
	if len(s.hive.sent()) != 0 {
		t.Fatal("took a line with no newline yet")
	}
	s.write("\n" + sampleReply[:30])
	s.step()
	if p := s.hive.sent(); len(p) != 1 || !strings.Contains(p[0].Text, "north site") {
		t.Fatalf("posts: %+v", p)
	}
	s.write(sampleReply[30:] + "\n")
	s.step()
	if p := s.hive.sent(); len(p) != 2 || p[1].Thread != p[0].Thread {
		t.Fatalf("the reply, once whole, goes in its parent's thread: %+v", p)
	}
}

func TestReplyToAThreadNotSeenStartsItsOwn(t *testing.T) {
	s := newSpool(t)
	s.write(sampleReply + "\n")
	id2 := s.ask("1793300300.000300", "1793300000.123456", "and the south site")
	s.step()
	posts := s.hive.sent()
	if len(posts) != 2 || posts[1].Thread != posts[0].Thread {
		t.Fatalf("posts: %+v", posts)
	}
	r := s.req("C0AAAAAAAAA/1793300120.000200")
	if r.SlackThread != "1793300000.123456" || len(r.Lines) != 2 || r.Lines[1] != id2 {
		t.Fatalf("request: %+v", r)
	}
	s.hive.reply("lead", posts[0].Thread, "lead-1", "Updated.")
	s.step()
	if o := s.out(); len(o) != 1 || o[0].Request != "C0AAAAAAAAA/1793300120.000200" {
		t.Fatalf("an answer names an id the relay sent: %+v", o)
	}
}

func TestProjectLineGoesToItsLead(t *testing.T) {
	s := newSpool(t)
	s.write(`{"id": "C1/1.0", "channel": "C1", "thread": null, "user": "U1", "text": "Fix the flaky test.", "project": "hive"}` + "\n")
	s.write(`{"id": "C1/2.0", "channel": "C1", "thread": null, "user": "U1", "text": "Something.", "project": "nowhere"}` + "\n")
	s.step()
	posts := s.hive.sent()
	if len(posts) != 2 {
		t.Fatalf("posts: %+v", posts)
	}
	if posts[0].Chat != "project:hive" || s.req("C1/1.0").Chat != "project:hive" {
		t.Errorf("a line naming project hive: %+v", posts[0])
	}
	if posts[1].Chat != "lead" || !strings.Contains(posts[1].Text, "nowhere") || s.req("C1/2.0").Chat != "lead" {
		t.Errorf("an unknown project goes to the lead, saying so: %+v", posts[1])
	}
}

func TestAnswersAndDeliveredComeBackAndItIsDoneWhenTheTasksAre(t *testing.T) {
	s := newSpool(t)
	id := s.ask("1793300000.000001", "", "Make the score page load faster.")
	s.step()
	th := s.hive.sent()[0].Thread
	s.hive.reply("lead", th, "lead-1", "On it: filing two tasks.")
	s.hive.task("t1", "lead", th, "new", nil)
	s.hive.task("t2", "lead", th, "queued", nil)
	s.hive.task("t9", "lead", "other-thread", "new", nil)
	s.hive.reply("lead", "other-thread", "lead-1", "not this one")
	s.step()
	if o := s.out(); len(o) != 1 || o[0] != (Out{Kind: "answer", Request: id, Text: "On it: filing two tasks."}) {
		t.Fatalf("outbox after filing: %+v", o)
	}
	if r := s.req(id); r.Status != "open" || len(r.Tasks) != 2 || r.Tasks[1].Stage != "queued" {
		t.Fatalf("request after filing: %+v", r)
	}

	pr := map[string]any{"number": 42, "url": "https://github.com/o/hive/pull/42", "state": "MERGED", "head_sha": "aaaa1111"}
	s.hive.task("t1", "lead", th, "merged", map[string]any{"pr": pr, "sha": "aaaa1111", "merge_sha": "mmmm2222"})
	s.hive.reply("lead", th, "hive", "Merged: task t1 https://github.com/o/hive/pull/42")
	s.hive.task("t1", "lead", th, "done", map[string]any{"pr": pr, "sha": "aaaa1111", "merge_sha": "mmmm2222"})
	s.step()
	o := s.out()
	if len(o) != 2 || o[1].Kind != "delivered" || o[1].Request != id {
		t.Fatalf("one delivered line for the merge, none for the Hive's note: %+v", o)
	}
	for _, want := range []string{"https://github.com/o/hive/pull/42", "aaaa1111", "mmmm2222"} {
		if !strings.Contains(o[1].Text, want) {
			t.Errorf("delivered %q lacks %s", o[1].Text, want)
		}
	}
	if r := s.req(id); r.Status != "open" || len(r.Merged) != 1 {
		t.Fatalf("t2 is still queued, so not done: %+v", r)
	}

	s.hive.task("t2", "lead", th, "stopped", map[string]any{"reason": "not needed"})
	s.hive.reply("lead", th, "lead-1", "Done: the page loads in 200 ms now.")
	s.step()
	if r := s.req(id); r.Status != "done" || r.DoneBy != "tasks" || r.Done == nil {
		t.Fatalf("every task done or stopped: %+v", r)
	}
	if o := s.out(); len(o) != 3 || o[2].Text != "Done: the page loads in 200 ms now." {
		t.Fatalf("outbox: %+v", o)
	}

	// A task filed later opens it again.
	s.hive.task("t3", "lead", th, "new", nil)
	s.step()
	if r := s.req(id); r.Status != "open" || r.Done != nil {
		t.Fatalf("a new task should open it again: %+v", r)
	}
	if len(s.out()) != 3 {
		t.Fatal("a task's stage is no outbox line")
	}
}

func TestBlockedTaskKeepsItOpen(t *testing.T) {
	s := newSpool(t)
	id := s.ask("1.1", "", "Do a thing.")
	s.step()
	s.hive.task("t1", "lead", s.hive.sent()[0].Thread, "blocked", nil)
	s.step()
	if r := s.req(id); r.Status != "open" {
		t.Fatalf("blocked is neither done nor stopped: %+v", r)
	}
}

func TestLeadSaysItNeedsNothing(t *testing.T) {
	s := newSpool(t)
	id := s.ask("1.2", "", "What version is main on?")
	s.step()
	th := s.hive.sent()[0].Thread
	s.hive.reply("lead", th, "lead-1", "Main is on v0.1.150.")
	s.step()
	if r := s.req(id); r.Status != "open" {
		t.Fatalf("a reply alone isn't done: %+v", r)
	}
	s.hive.reply("lead", th, "lead-1", "It was tagged today.\nspool: done")
	s.hive.reply("lead", th, "lead-1", "spool: done")
	s.step()
	if r := s.req(id); r.Status != "done" || r.DoneBy != "lead" {
		t.Fatalf("the lead said spool: done: %+v", r)
	}
	o := s.out()
	if len(o) != 2 || o[1].Text != "It was tagged today." {
		t.Fatalf("spool: done is for the spool, not an answer: %+v", o)
	}
}

func TestJoinedAskCounts(t *testing.T) {
	s := newSpool(t)
	id := s.ask("1.3", "", "Speed up the build.")
	s.step()
	th := s.hive.sent()[0].Thread
	s.hive.task("big", "lead", "elsewhere", "working", map[string]any{"joins": []map[string]any{{"from": "lead-1", "chat": "lead", "thread": th}}})
	s.step()
	if r := s.req(id); len(r.Tasks) != 1 || r.Tasks[0].ID != "big" {
		t.Fatalf("a task the thread's ask joined is the thread's: %+v", r.Tasks)
	}
}

func TestEmptyAndBadLinesAreNotPosted(t *testing.T) {
	s := newSpool(t)
	s.write("not json\n\n{\"text\": \"no id\"}\n")
	id := s.ask("1.4", "", "  ")
	s.step()
	if len(s.hive.sent()) != 0 {
		t.Fatal("posted a line with no request")
	}
	if r := s.req(id); r.Status != "refused" || r.Error == "" {
		t.Fatalf("request: %+v", r)
	}
	s.ask("1.5", "", "A real one.")
	s.step()
	if len(s.hive.sent()) != 1 {
		t.Fatal("the lines after a bad one are still read")
	}
}

func TestHiveDownLeavesTheLineForLater(t *testing.T) {
	s := newSpool(t)
	s.door.Hive.Token = "wrong"
	id := s.ask("1.6", "", "Do it later.")
	if err := s.door.Step(context.Background()); err == nil {
		t.Fatal("want an error when the post fails")
	}
	if s.door.State().Inbox != 0 {
		t.Fatal("a line that wasn't posted must not be read past")
	}
	s.door.Hive.Token = "tok"
	s.step()
	if len(s.hive.sent()) != 1 || s.req(id).Status != "open" {
		t.Fatal("posted once the Hive answers")
	}
}

func TestInboxCutShortIsReadAgainWithoutRepeats(t *testing.T) {
	s := newSpool(t)
	s.ask("2.1", "", "first")
	s.ask("2.2", "", "second")
	s.step()
	os.Remove(s.cfg.Inbox)
	s.ask("2.2", "", "second")
	s.ask("2.3", "", "third")
	s.step()
	if p := s.hive.sent(); len(p) != 3 || !strings.Contains(p[2].Text, "third") {
		t.Fatalf("posts: %+v", p)
	}
}

func TestOutboxWriteCutShortIsFinishedNotRepeated(t *testing.T) {
	s := newSpool(t)
	s.write("") // the relay's file
	os.WriteFile(s.cfg.Outbox, []byte(`{"kind":"answer","request":"x","text":"before"}`+"\n"), 0o644)
	at := int64(len(`{"kind":"answer","request":"x","text":"before"}` + "\n"))
	// A crash mid-write: the mark saved, half the batch written.
	s.door.st.Out = []Out{{Kind: "answer", Request: "a", Text: "one"}, {Kind: "delivered", Request: "a", Text: "two"}}
	s.door.st.OutAt = &at
	f, _ := os.OpenFile(s.cfg.Outbox, os.O_WRONLY|os.O_APPEND, 0o644)
	f.WriteString(`{"kind":"answer","request":"a","text":"one"}` + "\n" + `{"kind":"deliv`)
	f.Close()
	s.step()
	o := s.out()
	if len(o) != 3 || o[1].Text != "one" || o[2].Text != "two" {
		t.Fatalf("outbox: %+v", o)
	}
	if st := s.door.State(); len(st.Out) != 0 || st.OutAt != nil {
		t.Fatalf("state: %+v", st)
	}
}

func TestParseLine(t *testing.T) {
	l, err := ParseLine([]byte(sampleReply))
	if err != nil || l.ID != "C0AAAAAAAAA/1793300120.000200" || l.Thread != "1793300000.123456" || l.User != "U0CCCCCCCCC" || l.Text != "<@U0BBBBBBBBB> use the October numbers" || l.slackKey() != "C0AAAAAAAAA/1793300000.123456" {
		t.Fatalf("reply: %+v %v", l, err)
	}
	if l, err = ParseLine([]byte(sampleTop)); err != nil || l.Thread != "" || l.slackKey() != l.ID {
		t.Fatalf("top: %+v %v", l, err)
	}
	for _, bad := range []string{"x", `{"text": "t"}`, `[1]`} {
		if _, err := ParseLine([]byte(bad)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(filepath.Join(dir, "spool.json"))
	if err != nil || !strings.HasSuffix(c.Inbox, "/hive-shadow/inbox") || !strings.HasSuffix(c.Outbox, "/hive-shadow/outbox") || !filepath.IsAbs(c.Inbox) {
		t.Fatalf("default: %+v %v", c, err)
	}
	os.WriteFile(filepath.Join(dir, "spool.json"), []byte(`{"inbox": "/x/in", "outbox": "/x/out"}`), 0o600)
	if c, err = LoadConfig(filepath.Join(dir, "spool.json")); err != nil || c.Inbox != "/x/in" || c.Outbox != "/x/out" {
		t.Fatalf("named: %+v %v", c, err)
	}
	os.WriteFile(filepath.Join(dir, "spool.json"), []byte(`{"inbox": "rel"}`), 0o600)
	if _, err = LoadConfig(filepath.Join(dir, "spool.json")); err == nil {
		t.Fatal("a relative path should be refused")
	}
}
