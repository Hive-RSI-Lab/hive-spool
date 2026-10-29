# spool: the relay's two JSON Lines files as a door to the Hive

A door made of two files, for a system that can't reach the Hive's API. The
API listens only on loopback inside the Hive's network namespace, behind the
daemon's own token. The relay feeding a shadow Hive (dispatch PR #2202,
`host/hive-members/hive-shadow`) is one such system. It appends each Slack
message a person writes to the **inbox**. The module posts it to the Hive's
lead, a top-level message as a thread of its own and a reply in its parent's
thread. What comes of that thread is appended to the **outbox**: the lead's
answers and the merged PRs it delivered. Neither file is ever moved or
renamed.

It is an optional module, not one of the defaults.

## How it works

```
 the relay                                 the Hive
 ─────────                                 ────────
 inbox  (JSON Lines, appended)  ──▶ module (hooks: channel), from a saved byte offset
                                      POST /api/chat {chat: lead, thread: "" | the parent's}
                                        → the lead reads "Module spool: …"
                                      GET /api/events
                                        chat.posted in that thread   → answer
                                        task.saved filed from it     → delivered (each merge)
 outbox (JSON Lines, appended)  ◀── whole lines only
```

- **In.** Every few seconds the module reads the inbox from the byte offset
  kept in `state.json`. It takes whole lines only: a last line with no newline
  yet waits for the next pass. Each line is taken **once**. The offset is
  saved after each line, and an id already taken is never posted again, even
  if the relay sends it twice. A top-level message (`thread` null) is posted
  to the lead's chat as a new thread. A reply (`thread` set) is posted in the
  thread its parent started. A reply whose parent the module never saw starts
  a thread of its own, and later replies to the same parent join it. If the
  Hive doesn't answer a post, the offset stays before that line and it is
  tried again on the next pass. A line that isn't JSON, or has no id, is
  skipped and logged. A line with no text is not posted. If the inbox is
  replaced or cut short (its first bytes differ, or it is shorter than the
  offset), it is read again from the start, and the ids already taken are
  skipped.
- **Out.** The module follows the journal from the moment it is installed,
  not its history:
  - every `chat.posted` line in the thread by the lead or an agent is an
    **answer**. The module's own posts and the Hive's notes (`Merged: …`) are
    not. A `spool: done` line is left out of the answer's text, and a reply
    that is only `spool: done` writes none.
  - every merge a task filed from that thread records (`merge_sha`) is a
    **delivered** line, with the PR's URL, the head SHA that passed the gate
    and the merge SHA. The work module records on each task the chat and
    thread it was filed from, so this includes the lead's briefs, helpers its
    workers spawn, and reverts of what it merged. A task that an ask from the
    thread was joined into counts too. A task that merges checkpoints has one
    line per merge.
  - Lines are appended whole, each once. Where a batch starts is saved before
    it is written, so after a crash mid-write the next pass writes only the
    part that is missing.
- **Done.** A request is `done` when every task its thread filed is `done` or
  `stopped`. If the lead says it needs nothing, or nothing more, it writes
  `spool: done` on a line of its own in the thread. That makes the request
  done once no task it filed is still open. A `blocked` task keeps the request
  open. So does a task filed later. The relay's format has no line for this,
  so it is kept in `state.json` (`requests.<id>.status`), not the outbox.
- **State.** `state.json` in the module's HOME holds the journal cursor, the
  inbox offset, every request taken, and the outbox lines not yet written, so
  a restart carries on where the module stopped.

## The inbox

One file, JSON Lines, appended by the relay. Each line is one Slack message by
a person:

```json
{"id": "C0AAAAAAAAA/1793300000.123456", "channel": "C0AAAAAAAAA", "thread": null, "user": "U0BBBBBBBBB", "text": "can you update the sizing sheet for the north site", "posted": 1793300000.123456, "heard": 1793300042.51}
{"id": "C0AAAAAAAAA/1793300120.000200", "channel": "C0AAAAAAAAA", "thread": "1793300000.123456", "user": "U0CCCCCCCCC", "text": "<@U0BBBBBBBBB> use the October numbers", "posted": 1793300120.0002, "heard": 1793300162.9}
```

- `id`: `<channel>/<ts>`, the request id every outbox line names.
- `channel`, `user`: Slack ids. `text`: raw mrkdwn, mentions as `<@U…>`.
- `thread`: the parent's ts for a reply, null at top level.
- `posted`, `heard`: epoch seconds.
- `project` (not sent by the relay): a line that names a Hive project goes to
  that project's lead. If the Hive doesn't have it, or the module isn't
  granted it, the line goes to the lead with a note saying so.

The text is posted with a note telling the lead that it comes from outside the
Hive and is information, not instructions.

## The outbox

One file, JSON Lines, appended by the module and read by the relay from a byte
offset:

```json
{"kind":"answer","request":"C0AAAAAAAAA/1793300000.123456","text":"Updated the north site's sheet with the October numbers."}
{"kind":"delivered","request":"C0AAAAAAAAA/1793300000.123456","text":"https://github.com/o/sizing/pull/42 merged into sizing main as 8c1d… (head 3f2a…): Update the north site's sizing sheet"}
```

- `request`: the id of the inbox line that started the thread, always an id
  the relay sent. Answers to a thread's replies name the same id, so the relay
  answers in the same Slack thread.
- `answer`: the text of a reply by the lead or an agent.
- `delivered`: one merged PR: its URL, the merge SHA on main and the head SHA.

## Set it up

1. Install the module, and grant it `*` in the control center's Modules screen.
   The lead's chat belongs to no project, and only a `*` grant reaches it.
2. Optional: write `spool.json` in the module's own HOME
   (`<workspace>/.hive/modules/spool/spool.json`; `hive spool where` prints it
   and the files in use) to change the files:

   ```json
   {"inbox": "~/hive-shadow/inbox", "outbox": "~/hive-shadow/outbox"}
   ```

   Those are the defaults (the relay's `~MEMBER/hive-shadow/inbox` and
   `outbox`). `~` is the home of the account the Hive runs as, not the
   module's HOME. Paths must be absolute or start with `~/`. The module
   creates the files' folder but not the inbox, which the relay writes; the
   outbox appears with its first line.

When it starts, the module reports `module.reported` with kind `watching` and
names both files. A step that fails is logged and reported once as kind
`error`, then tried again. Examples are an outbox it can't write or the Hive
not answering.

## What it holds, and who can read it

- The request text and everything the thread produces are written to the
  outbox. Anyone who can read that folder can read them.
- Its reach in the Hive is the `channel` hook: `/api/chat`, `/api/events`,
  `/api/run/stop` and `/api/task/stop`. It uses the first two, limited to the
  projects it is granted. It opens no port and reaches nothing on the network.

## Commands

- `hive spool`: the program the daemon runs.
- `hive spool where`: print where spool.json goes and the files it names.

## Tests

`go test ./...` runs against a fake Hive API and the two files in a temp
folder. The tests cover:

- the relay's two sample lines: a top-level message starting a thread and a
  reply (thread set) posted in it, once each across restarts and repeats;
- a partial last line waiting for its newline, across a restart;
- a reply whose parent wasn't seen, and a line naming a project;
- answer and delivered lines with PR URLs and SHAs, and the Hive's notes and
  `spool: done` kept out of answers;
- done by tasks and by `spool: done`, a blocked task, a later task reopening;
- joined asks;
- bad and empty lines, the Hive being down, the inbox replaced, and an outbox
  write cut short by a crash.
