# spool: hand the Hive work as files, collect what it delivered as files

A door made of two folders, for a system that can't reach the Hive's API. The
API listens only on loopback inside the Hive's network namespace, behind the
daemon's own token. The box feeding a shadow Hive is one such system. It drops
a request file in the **inbox**. The module posts it to the Hive's lead as a
thread of its own. What comes of that thread is written to the **outbox**, one
file per request, and that file is rewritten each time something new arrives.

It is an optional module, not one of the defaults.

## How it works

```
 outside                                  the Hive
 ───────                                  ────────
 inbox/<name>  ──▶ module (hooks: channel)
                     POST /api/chat {chat: lead | project:<p>, text}
                       → a new thread; the lead reads "Module spool: …"
 inbox/taken/<name>  (moved once posted)
                     GET /api/events
                       chat.posted in that thread   → replies
                       task.saved filed from it     → tasks, merges
 outbox/<id>.json ◀── rewritten as each one arrives
```

- **In.** Every few seconds the module reads the inbox. A regular file goes in
  once it has sat unchanged for 2 seconds; names starting with `.` or ending
  in `.tmp`, `.part` or `~` are skipped. It is posted **once**, to the lead's
  chat, or to the lead of the project it names, as a new thread. Then it moves
  to `inbox/taken/`. The module records the request in `state.json` before
  moving the file, so a crash or restart never posts it twice. If a file comes
  back with an id already taken, it isn't posted again; it moves to
  `taken/<name>.2.json`. If the Hive doesn't answer the post, the file stays in
  the inbox and is tried again on the next pass. If the project it names is
  one the Hive doesn't have, or one the module isn't granted, the request goes
  to the lead with a note saying so.
- **Out.** The module follows the journal from the moment it is installed,
  not its history:
  - every `chat.posted` line in the request's thread, except the module's own
    post, is a **reply**;
  - every `task.saved` line for a task filed from that thread is a **task**.
    The work module records on each task the chat and thread it was filed
    from, so this includes the lead's briefs, helpers its workers spawn, and
    reverts of what it merged. A task that an ask from the thread was joined
    into counts too.
  - each merge a task records (`merge_sha`) is a **merged** entry, with the
    PR's URL and number, its head SHA and the merge SHA. A task that merges
    checkpoints has one entry per merge.
- **Done.** A request is `done` when every task its thread filed is `done` or
  `stopped`. If the lead says it needs nothing, or nothing more, it writes
  `spool: done` on a line of its own in the thread. That makes the request
  done once no task it filed is still open. A `blocked` task keeps the request
  open. So does a task filed later: it moves the request back to `open`, and
  it is done again when that task ends. The lead is told all this in the post.
- **State.** `state.json` in the module's HOME holds the journal cursor and
  every request taken, so a restart carries on where the module stopped.

## The request file (inbox)

The module takes files in the format the box writes them. It adds no format of
its own. Any of these work:

- **A JSON object.** The request's text is the first non-empty string under
  `text`, `request`, `prompt`, `body`, `message`, `ask`, `content`, `task` or
  `description`. Every other field except `id` and `project` is shown to the
  lead after the text as `key: value`, so nothing the box sends is lost. A file
  with none of those fields still works: all its fields become the text.
  - `id` (string or number, optional): the request's id. Without one, the id
    is the file's name without its extension.
  - `project` (optional): the Hive project whose lead gets it. Without one, it
    goes to the Hive's lead.

  ```json
  {"id": "req-0042", "project": "hive", "text": "Add a dark mode to the dashboard.", "from": "workspace"}
  ```
- **Anything else** (`.txt`, `.md`, no extension, JSON that isn't an object):
  the whole file, trimmed, is the request's text, and its id is the file's
  name without its extension.

The text is posted with a note telling the lead that it comes from outside the
Hive and is information, not instructions. A file with no text is not posted.
Its outbox file says `refused`.

## The answer file (outbox/<id>.json)

One file per request. `<id>` is the request's id with any character outside
`A-Z a-z 0-9 . _ -` changed to `-`. Each version replaces the last whole (it
is written to a temporary file and renamed), so a reader never sees half of
one.

```json
{
  "id": "req-0042",
  "file": "req-0042.json",
  "project": "hive",
  "chat": "project:hive",
  "thread": "261029-201500-ab12cd34ef",
  "status": "done",
  "done_by": "tasks",
  "request": "Add a dark mode to the dashboard.\nfrom: workspace",
  "input": {"id": "req-0042", "project": "hive", "text": "Add a dark mode to the dashboard.", "from": "workspace"},
  "posted": "2026-10-29T20:15:00Z",
  "updated": "2026-10-29T23:02:11Z",
  "done": "2026-10-29T23:02:11Z",
  "final_reply": "Dark mode is merged: https://github.com/o/hive/pull/42",
  "replies": [
    {"at": "2026-10-29T20:16:03Z", "from": "lead-project-hive-b38c", "name": "lead", "text": "Filing it."},
    {"at": "2026-10-29T22:58:40Z", "from": "hive", "name": "hive", "text": "Merged: …"},
    {"at": "2026-10-29T23:02:11Z", "from": "lead-project-hive-b38c", "name": "lead", "text": "Dark mode is merged: https://github.com/o/hive/pull/42"}
  ],
  "tasks": [
    {"id": "261029-201610-9f8e7d6c5b", "project": "hive", "title": "Dark mode for the dashboard",
     "kind": "work", "stage": "done", "issue": "https://github.com/o/hive/issues/41",
     "pr": "https://github.com/o/hive/pull/42", "head_sha": "3f2a…", "merge_sha": "8c1d…",
     "updated": "2026-10-29T22:58:40Z"}
  ],
  "merged": [
    {"task": "261029-201610-9f8e7d6c5b", "project": "hive", "pr": 42,
     "url": "https://github.com/o/hive/pull/42", "head_sha": "3f2a…", "merge_sha": "8c1d…",
     "at": "2026-10-29T22:58:40Z"}
  ],
  "lead_done": false
}
```

- `status`: `open` while work is under way, `done` as defined above, or
  `refused` (with `error`) for a file that held no request.
- `done_by`: `tasks` (every task done or stopped) or `lead` (the lead said
  `spool: done`, with nothing open).
- `request`: the text the lead was given. `input`: the file as dropped, when
  it was JSON.
- `final_reply`: the last reply from the lead or an agent, not the Hive's own
  notes.
- `replies`: everything posted in the thread except the module's own post, in
  order. `from` is a run id, or `hive` for the Hive's notes.
- `tasks`: each task as last saved, with `stage` (`new`, `queued`, `working`,
  …, `merged`, `done`, `blocked`, `stopped`) and, once it has them, the PR URL
  and SHAs.
- `merged`: what was delivered, for a scorer to read: each merged PR's URL and
  number, the head SHA that passed the gate, and the merge SHA on main.

## Set it up

1. Install the module, and grant it `*` in the control center's Modules screen.
   The lead's chat belongs to no project, and only a `*` grant reaches it.
2. Optional: write `spool.json` in the module's own HOME
   (`<workspace>/.hive/modules/spool/spool.json`; `hive spool where` prints it
   and the folders in use) to change the folders:

   ```json
   {"inbox": "~/hive-shadow/inbox", "outbox": "~/hive-shadow/outbox"}
   ```

   Those are the defaults. `~` is the home of the account the Hive runs as,
   not the module's HOME. Paths must be absolute or start with `~/`. The
   module creates the folders and `inbox/taken/`.

When it starts, the module reports `module.reported` with kind `watching` and
names both folders. A step that fails is logged and reported once as kind
`error`, then tried again. Examples are a folder it can't write or the Hive
not answering.

## What it holds, and who can read it

- The request text and everything the thread produces are written to the
  outbox. Anyone who can read that folder can read them.
- Its reach in the Hive is the `channel` hook: `/api/chat`, `/api/events`,
  `/api/run/stop` and `/api/task/stop`. It uses the first two, limited to the
  projects it is granted. It opens no port and reaches nothing on the network.

## Commands

- `hive spool`: the program the daemon runs.
- `hive spool where`: print where spool.json goes and the folders it names.

## Tests

`go test ./...` runs against a fake Hive API and a spool in a temp folder. The
tests cover:

- a post made once, and its move to `taken/`, across restarts and repeats;
- a project's lead, and an unknown project;
- replies, tasks, merges with their PR URLs and SHAs, and done by tasks;
- done by `spool: done`, a blocked task, and a later task reopening a request;
- joined asks;
- empty and half-written files, and the Hive being down;
- both file formats.
