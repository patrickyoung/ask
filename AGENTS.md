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
- never weaken the replay invariant. `event.Check` has two branches and
  both are the same assertion; a request that records neither messages nor
  a digest is a hard error, so a lost field cannot make the check
  vacuously pass. Migrate formats, do not relax the check;
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
agent loop, a verifier, a workspace, skills, prompt templates, a config
file, a REPL, a daemon, MCP, and any second mechanism for getting text into
a conversation besides argv and stdin.
