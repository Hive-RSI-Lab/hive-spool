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
	s := &spoolT{t: t, hive: newFakeHive(t), home: filepath.Join(dir, "home"), cfg: Config{Inbox: filepath.Join(dir, "inbox"), Outbox: filepath.Join(dir, "outbox")}}
	os.MkdirAll(s.home, 0o700)
	// Something already in the journal before the module starts: never the spool's.
	s.hive.reply("lead", "old", "lead-1", "history")
	s.restart()
	return s
}

func (s *spoolT) restart() {
	s.door = New(s.cfg, &hive.Client{API: s.hive.srv.URL, Token: "tok"}, "spool", s.home)
	s.door.Settle = 0
	if err := s.door.Start(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

func (s *spoolT) drop(name, body string) {
	if err := os.WriteFile(filepath.Join(s.cfg.Inbox, name), []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *spoolT) step() {
	s.t.Helper()
	if err := s.door.Step(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

func (s *spoolT) out(id string) Request {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.cfg.Outbox, id+".json"))
	if err != nil {
		s.t.Fatal(err)
	}
	var r Request
	if err := json.Unmarshal(raw, &r); err != nil {
		s.t.Fatalf("outbox %s: %v\n%s", id, err, raw)
	}
	return r
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestRequestPostsOnceToTheLeadAndMovesToTaken(t *testing.T) {
	s := newSpool(t)
	s.drop("r-001.json", `{"id": "req-1", "text": "Add a dark mode to the dashboard.", "from": "workspace"}`)
	s.step()
	posts := s.hive.sent()
	if len(posts) != 1 || posts[0].Chat != "lead" || posts[0].Thread != posts[0].ID {
		t.Fatalf("want one post starting its own thread in the lead's chat, got %+v", posts)
	}
	if !strings.Contains(posts[0].Text, "Add a dark mode to the dashboard.") || !strings.Contains(posts[0].Text, "from: workspace") || !strings.Contains(posts[0].Text, "req-1") {
		t.Errorf("post doesn't carry the request: %q", posts[0].Text)
	}
	if exists(filepath.Join(s.cfg.Inbox, "r-001.json")) || !exists(filepath.Join(s.cfg.Inbox, "taken", "r-001.json")) {
		t.Error("the file should be moved to inbox/taken/")
	}
	r := s.out("req-1")
	if r.Status != "open" || r.Chat != "lead" || r.Thread != posts[0].Thread || r.File != "r-001.json" || string(r.Input) == "" {
		t.Errorf("outbox: %+v", r)
	}
	if len(r.Replies) != 0 {
		t.Errorf("the spool's own post or the history is not a reply: %+v", r.Replies)
	}

	// Steps after, a restart, and the same file dropped again: still one post.
	s.step()
	s.restart()
	s.drop("r-001.json", `{"id": "req-1", "text": "Add a dark mode to the dashboard."}`)
	s.step()
	if n := len(s.hive.sent()); n != 1 {
		t.Fatalf("posted %d times, want once", n)
	}
	if !exists(filepath.Join(s.cfg.Inbox, "taken", "r-001.2.json")) {
		t.Error("the repeat should be moved beside the first")
	}
}

func TestProjectRequestGoesToItsLead(t *testing.T) {
	s := newSpool(t)
	s.drop("a.json", `{"project": "hive", "prompt": "Fix the flaky test."}`)
	s.drop("b.json", `{"project": "nowhere", "prompt": "Something."}`)
	s.step()
	posts := s.hive.sent()
	if len(posts) != 2 {
		t.Fatalf("posts: %+v", posts)
	}
	if posts[0].Chat != "project:hive" || s.out("a").Chat != "project:hive" || s.out("a").Project != "hive" {
		t.Errorf("a named project hive: %+v", posts[0])
	}
	if posts[1].Chat != "lead" || !strings.Contains(posts[1].Text, "nowhere") || s.out("b").Chat != "lead" {
		t.Errorf("an unknown project goes to the lead, saying so: %+v", posts[1])
	}
}

func TestRepliesTasksAndMergesComeBackAndItIsDoneWhenTheTasksAre(t *testing.T) {
	s := newSpool(t)
	s.drop("ask.txt", "Make the score page load faster.\n")
	s.step()
	th := s.hive.sent()[0].Thread
	s.hive.reply("lead", th, "lead-1", "On it: filing two tasks.")
	s.hive.task("t1", "lead", th, "new", nil)
	s.hive.task("t2", "lead", th, "queued", nil)
	s.hive.task("t9", "lead", "other-thread", "new", nil)
	s.hive.reply("lead", "other-thread", "lead-1", "not this one")
	s.step()
	r := s.out("ask")
	if r.Status != "open" || len(r.Tasks) != 2 || r.Tasks[0].ID != "t1" || r.Tasks[1].Stage != "queued" || r.Final != "On it: filing two tasks." || len(r.Replies) != 1 {
		t.Fatalf("outbox after filing: %+v", r)
	}

	pr := map[string]any{"number": 42, "url": "https://github.com/o/hive/pull/42", "state": "MERGED", "head_sha": "aaaa1111"}
	s.hive.task("t1", "lead", th, "merged", map[string]any{"pr": pr, "sha": "aaaa1111", "merge_sha": "mmmm2222"})
	s.hive.reply("lead", th, "hive", "Merged: task t1 https://github.com/o/hive/pull/42")
	s.hive.task("t1", "lead", th, "done", map[string]any{"pr": pr, "sha": "aaaa1111", "merge_sha": "mmmm2222"})
	s.step()
	r = s.out("ask")
	if r.Status != "open" {
		t.Fatalf("t2 is still queued, so not done: %+v", r)
	}
	if len(r.Merged) != 1 || r.Merged[0].URL != "https://github.com/o/hive/pull/42" || r.Merged[0].HeadSHA != "aaaa1111" || r.Merged[0].MergeSHA != "mmmm2222" || r.Merged[0].PR != 42 {
		t.Fatalf("merged: %+v", r.Merged)
	}
	if r.Final != "On it: filing two tasks." || len(r.Replies) != 2 {
		t.Errorf("the Hive's note is a reply but not the lead's final word: %+v", r)
	}

	s.hive.task("t2", "lead", th, "stopped", map[string]any{"reason": "not needed"})
	s.hive.reply("lead", th, "lead-1", "Done: the page loads in 200 ms now.")
	s.step()
	r = s.out("ask")
	if r.Status != "done" || r.DoneBy != "tasks" || r.Done == nil || r.Final != "Done: the page loads in 200 ms now." || r.Tasks[1].Reason != "not needed" {
		t.Fatalf("every task done or stopped: %+v", r)
	}

	// A task filed later opens it again.
	s.hive.task("t3", "lead", th, "new", nil)
	s.step()
	if r = s.out("ask"); r.Status != "open" || r.Done != nil {
		t.Fatalf("a new task should open it again: %+v", r)
	}
}

func TestBlockedTaskKeepsItOpen(t *testing.T) {
	s := newSpool(t)
	s.drop("x.txt", "Do a thing.")
	s.step()
	th := s.hive.sent()[0].Thread
	s.hive.task("t1", "lead", th, "blocked", nil)
	s.step()
	if r := s.out("x"); r.Status != "open" {
		t.Fatalf("blocked is neither done nor stopped: %+v", r)
	}
}

func TestLeadSaysItNeedsNothing(t *testing.T) {
	s := newSpool(t)
	s.drop("q.md", "What version is main on?")
	s.step()
	th := s.hive.sent()[0].Thread
	s.hive.reply("lead", th, "lead-1", "Main is on v0.1.150.")
	s.step()
	if r := s.out("q"); r.Status != "open" {
		t.Fatalf("a reply alone isn't done: %+v", r)
	}
	s.hive.reply("lead", th, "lead-1", "Main is on v0.1.150.\nspool: done")
	s.step()
	r := s.out("q")
	if r.Status != "done" || r.DoneBy != "lead" || len(r.Tasks) != 0 || !strings.HasPrefix(r.Final, "Main is on") {
		t.Fatalf("the lead said spool: done: %+v", r)
	}
}

func TestJoinedAskCounts(t *testing.T) {
	s := newSpool(t)
	s.drop("j.txt", "Speed up the build.")
	s.step()
	th := s.hive.sent()[0].Thread
	s.hive.task("big", "lead", "elsewhere", "working", map[string]any{"joins": []map[string]any{{"from": "lead-1", "chat": "lead", "thread": th}}})
	s.step()
	if r := s.out("j"); len(r.Tasks) != 1 || r.Tasks[0].ID != "big" {
		t.Fatalf("a task the thread's ask joined is the thread's: %+v", r.Tasks)
	}
}

func TestEmptyFileIsRefusedAndNotPosted(t *testing.T) {
	s := newSpool(t)
	s.drop("empty.json", `{"id": "e1", "text": ""}`)
	s.step()
	if len(s.hive.sent()) != 0 {
		t.Fatal("posted an empty request")
	}
	if r := s.out("e1"); r.Status != "refused" || r.Error == "" {
		t.Fatalf("outbox: %+v", r)
	}
}

func TestUnsettledAndHiddenFilesWait(t *testing.T) {
	s := newSpool(t)
	s.door.Settle = time.Hour
	s.drop("new.txt", "Still being written")
	s.drop(".hidden", "x")
	s.drop("part.tmp", "x")
	s.step()
	if len(s.hive.sent()) != 0 || !exists(filepath.Join(s.cfg.Inbox, "new.txt")) {
		t.Fatal("took a file before it settled")
	}
	s.door.Settle = 0
	s.step()
	if len(s.hive.sent()) != 1 || !exists(filepath.Join(s.cfg.Inbox, ".hidden")) || !exists(filepath.Join(s.cfg.Inbox, "part.tmp")) {
		t.Fatalf("posts: %+v", s.hive.sent())
	}
}

func TestHiveDownLeavesTheFileForLater(t *testing.T) {
	s := newSpool(t)
	s.door.Hive.Token = "wrong"
	s.drop("later.txt", "Do it later.")
	if err := s.door.Step(context.Background()); err == nil {
		t.Fatal("want an error when the post fails")
	}
	if !exists(filepath.Join(s.cfg.Inbox, "later.txt")) {
		t.Fatal("a file that wasn't posted must stay in the inbox")
	}
	s.door.Hive.Token = "tok"
	s.step()
	if len(s.hive.sent()) != 1 || s.out("later").Status != "open" {
		t.Fatal("posted once the Hive answers")
	}
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		name, raw, id, project, text string
	}{
		{"a.txt", "  hello\n", "a", "", "hello"},
		{"b.json", `{"id": 17, "request": "do x"}`, "17", "", "do x"},
		{"c.json", `{"project": "hive", "ask": "do y", "priority": 2, "tags": ["a"]}`, "c", "hive", "do y\npriority: 2\ntags: [\"a\"]"},
		{"d.json", `{"title": "only a title"}`, "d", "", "title: only a title"},
		{"e.json", `not json {`, "e", "", "not json {"},
		{"noext", `x`, "noext", "", "x"},
	} {
		q := Parse(c.name, []byte(c.raw))
		if q.ID != c.id || q.Project != c.project || q.Text != c.text {
			t.Errorf("%s: got %+v", c.name, q)
		}
	}
	if FileID("a/../b c") != "a-..-b-c" || FileID("..") != "request" {
		t.Errorf("FileID: %q %q", FileID("a/../b c"), FileID(".."))
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
		t.Fatal("a relative folder should be refused")
	}
}
