# Development guidance

Rob Pike is the bar. `ask` is smaller than the program it was carved from,
and staying smaller is a feature, not an accident.

The whole program is four steps: fold the log into a conversation, log the
request, stream the turn, log it. If a change makes that sentence longer,
it needs a very good reason.

When changing `ask`:

- preserve the Unix contract: stdout is the answer alone, stderr is
  progress, the exit code is the outcome. Exit 2 means the context window
  is full and nothing else — it exists so a retry loop can stop;
- keep conversation state explicit. A plain `ask` starts fresh; `-c`
  continues exactly `current` and fails if that pointer is unavailable;
  `-f` continues or creates only the named file and never moves `current`;
- never weaken the replay invariant. `event.Check` has two branches and
  both are the same assertion; a request that records neither messages nor
  a digest is a hard error, so a lost field cannot make the check
  vacuously pass. Migrate formats, do not relax the check;
- normalized Context JSONL on stdin remains message data, but its user event
  carries a compact manifest over the exact in-message bytes. Never duplicate
  the snapshot, refetch it during replay, or accept a manifest that cannot be
  reconstructed from the message;
- a new provider adapter earns its keep by passing `checkContract` over a
  wire fixture. Streamed text must equal the logged text blocks, and
  streamed reasoning must survive as something replayable to that same
  provider;
- keep the log append-only and single-writer. The lock is not politeness:
  two writers interleave two conversations and break every digest written
  afterwards. It is an `flock` on the session file, taken before the file
  is read — a fold over a prefix that was stale when it was read writes a
  digest the file will not reproduce, which is the same divergence from the
  other side. Do not reintroduce a pid file: judging one stale is three
  steps that are not one operation, and pids are recycled;
- fail before creating a session file. A bad invocation must leave no
  litter;
- keep structured output at the boundary. A schema is raw request metadata,
  never prompt text and never part of `Fold`; each adapter maps it to the
  provider's native field. Validate the completed document only after the
  four logged steps and before admitting it to normal stdout. A rejected
  document remains an honest assistant turn in the append-only log; do not
  repair it with another model call or hide it from replay;

- **a note is a record, not a message.** `ask note` puts text or typed JSON in a session
  without a model call, and it is not folded — so the conversation a
  provider sees is exactly what it was without it, and every digest ever
  written still matches. `-s` is required and there is no way around it,
  which is what makes the verb an instance of the rule below rather than an
  exception to it. Do not fold notes, and do not add a way to write an
  unsigned one: the moment either happens, a session stops being able to say
  who put a sentence in it. Structured records are atomic with a prefix seal:
  replay must reject an unsealed record, a sequence gap, or a changed prefix.
  `replay -check -json` emits the same in-memory event snapshot it verified;
  do not split that producer contract back into a check followed by a reread.
  A failing verifier result is both a record and a `user` message because the
  model has to act on it; a passing one is only a record, because the run is
  over and it is addressed to a later reader;
- **model-written text in a conversation is stamped, or it does not go in.**
  `ask compact` is the one thing that puts words in a conversation that
  nobody said, and it is admissible only because every part of it is
  attributable: the note is a `user` event with `source: "summary"`, its
  header names the `parent` it came from and the `summary` session the note
  was written in, and that session replays like any other. Compaction is
  never automatic, never silent, and never edits the source — three files,
  and all three prove. Anything that would put unattributed text into a fold
  is the thing this rule exists to refuse;
- never truncate input silently. Too much on stdin, too large an
  attachment, too many of them: each is an error naming the limit. An
  answer about the first part of a file, presented as an answer about the
  file, is not something the next program in the pipe can detect;
- credentials do not travel. A token endpoint is https except on loopback
  (`auth.CheckTokenURL`, checked where it is configured *and* where it is
  used), and it is fetched with `tokenClient` — bounded in time, and no
  redirects, because the form body carries a secret that Go would re-send
  on a 307. A rotating refresh token is spent under the credential file's
  lock, once;
- run `go test ./...` — and `go test -race ./...` when touching the log or
  a stream — before reporting success;
- keep the docs true: a new flag belongs in `ask help` and `ask.1`; a new
  command belongs in both plus `README.md`; a new environment variable
  belongs in `ask.1` and `ask help`. Tests enforce each of those, plus a
  lint-clean pure-ASCII man page, help inside eighty columns, and help on
  stdout with usage errors on stderr. Guard wholes as well as parts: a
  flag-level check stays green while an entire verb goes undocumented.

Things left out on purpose. Do not add them back without asking: tools, an
agent loop, a model verifier loop, a workspace, skills, prompt templates, a
config file, a REPL, a daemon, and MCP. Local JSON Schema validation is an
output check, not a second model turn.

Text enters a conversation from argv and stdin. `ask compact` is the single
exception, added deliberately because exit 2 was otherwise a dead end, and
it is bounded by the rule above: the note is stamped, its provenance is in
the header, and the call that wrote it is a session you can read. A second
exception needs the same three properties or it is not one.

Branching a session verbatim is `cp`, and that is why there is no `fork`
verb: a session is a self-contained file and `-f` names one. A verb that
copies a file would be a verb that copies a file.
