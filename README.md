# ask

Put a question through a language model. Get the answer on stdout.

```
$ ask what is the airspeed velocity of an unladen swallow
About 11 m/s for a European swallow in level cruising flight.

$ git diff | ask "write a commit message"
Fix off-by-one in the ring buffer wrap

$ ask "does this log show an OOM?" -q < kern.log && echo "yes it does"
```

It is a filter, and it behaves like one: the answer is stdout, progress is
stderr, and the exit status says what happened. It has no tools, no agent
loop, and nothing to configure. The model answers, and the answer is the
product.

What it does have is a log you can trust. Every run appends to an
append-only JSONL session file, and the conversation sent to the model is a
pure function of that file — provably, on demand:

```
$ ask replay -check
ok: 20260801-142233-a3f9c1e0.jsonl replays exactly (11 events)
```

## Install

```
go install github.com/patrickyoung/ask@latest
export ASK_MODEL=anthropic/claude-sonnet-5
export ANTHROPIC_API_KEY=...
```

Go 1.26 or newer, and a Unix: Linux, macOS, or BSD. `ask` opens attachments
non-blocking so a fifo is refused rather than waited on, and locks a session
with `flock(2)` so a crashed writer strands nothing — neither has a Windows
spelling, and pretending otherwise would be a worse answer than saying so.
WSL works.

Five providers, one flag: `anthropic`, `openai`, `openai-codex`, `gemini`,
`openrouter`. Models are `provider/model`, and OpenRouter models keep their
own slash (`openrouter/anthropic/claude-sonnet-4.5`).

On a ChatGPT subscription, import the Codex CLI's credentials instead of
setting a key — and use the model that account is actually entitled to, or
the API says so and nothing works:

```
ask login openai-codex -from-codex
export ASK_MODEL=openai-codex/$(awk -F'"' '/^model/{print $2}' ~/.codex/config.toml)
```

**[GUIDE.md](GUIDE.md)** is the field guide: what `ask` is good at, what it
is not, and the recipes — measured against a real Codex account, including
the three things that will bite you (no clock, no timeout, and `xargs -P`
silently scrambling parallel answers).

## Attachments

`-a` takes a file, and repeats:

```
$ ask -a chart.png "what is the trend?"
$ ask -a q3.pdf -a q4.pdf "what changed between these quarters?"
$ screencapture -x -t png - | ask "what is on my screen?"
```

What a file *is* comes from reading it, never from its name. Bytes with a
known signature are that type; anything else that is valid UTF-8 without
NULs is text; anything else is refused by name before it is sent. So a
`.png` holding a shell script is a shell script, and `-a main.go` is `cat
main.go` with a label — text inlines, so the log stays greppable.

| provider | carries |
| --- | --- |
| anthropic | images, PDF |
| openai, openai-codex | images, PDF |
| gemini | images, audio, video, PDF |
| openrouter | images, PDF, WAV, MP3 |

Anything a provider can't carry is refused *before* the session log is
touched, naming the provider and what it does take — a message that could
never be sent must not become an event that every later fold rebuilds.

The bytes live in the log. A photo makes a large record; in exchange the
session stays one self-contained file that still replays exactly, and
copying it copies everything it means. Limits are fixed at 16 attachments,
16 MB each, 32 MB per message. Piped input is bounded by the same 16 MB, and
crossing it is an error rather than a truncation — an answer about the first
part of a file, presented as an answer about the file, is not something the
next program in the pipe can detect. Session files are mode 0600.

Three things this makes possible, all run against PDFs with known contents:

```bash
# a picture of a page becomes JSON, and JSON decides. No OCR installed.
qlmanage -t -s 1400 -o . report.pdf
ask -a report.pdf.png 'Return ONLY minified JSON: {"runway_months":0}' > r.json
[ "$(jq -r .runway_months r.json)" -lt 12 ] && echo escalate

# two images, and what changed — point it at UI screenshots for a
# visual regression check
ask -a q1.png -a q3.png 'Same report, two quarters apart, in order.
  List ONLY the metrics that moved: metric: before -> after (direction).'

# documents accumulate across processes, like everything else
for q in q1 q2 q3; do ask -q -a $q.pdf "Next report."; done
ask -q 'Across all three, name the trend that most threatens this company.'
#> burn rose $410K -> $505K -> $640K while runway fell 19 -> 15 -> 11 months
```

That last one is the combination worth understanding: attachments ride the
same accumulating conversation everything else does, so three documents
that were never in context together can still be compared in one question.

The one you will actually use is the clipboard. Press ⌃⇧⌘4, drag a box
round an error dialog or a chart in someone's slide deck, then:

```bash
askclip() {
  local f=$(mktemp -t askclip).png
  osascript -e "set f to (open for access POSIX file \"$f\" with write permission)" \
            -e 'write (the clipboard as «class PNGf») to f' \
            -e 'close access f' 2>/dev/null \
    || { echo "no image on the clipboard" >&2; return 1; }
  ask -a "$f" "$@"; local r=$?; rm -f "$f"; return $r
}

askclip "what is this error and how do I fix it?"
```

[GUIDE.md](GUIDE.md) has all six worked through with their real output.

## The Unix contract

| stream | carries |
| --- | --- |
| stdout | the answer, and nothing else |
| stderr | streamed text, reasoning, retries, token counts |
| exit 0 | answered |
| exit 1 | error — bad usage, missing key, provider failure, no text |
| exit 2 | context window full (permanent: start a new conversation) |
| exit 130 | interrupted |

Reasoning never touches stdout. `2>/dev/null` leaves the answer alone.
`-q` silences stderr entirely. When stdout and stderr are the same
terminal, the answer is not printed twice.

Exit 2 is worth its own row. A full context window fails identically
forever, so a supervisor loop has to be able to tell it from a rate limit —
otherwise it retries a permanent error until someone notices the bill.

## Conversations

Each run continues the current conversation, so `ask` remembers:

```
$ ask "what is a monad"
$ ask "give me an example"        # continues
$ ask -n "unrelated question"     # starts fresh
```

Because that is the default, a pipeline inherits whatever was asked before
it. Every continuing run says so on stderr — which session, how many turns
— so it is never a silent surprise. Use `-n` when you want a clean slate,
or `-f` to keep a thread in a file of your own:

```
$ ask -f review.jsonl "review this diff" < patch
$ ask -f review.jsonl "now summarize the risks"
$ ask replay -check review.jsonl
```

Sessions live in `~/.ask/sessions/` (or `$ASK_DIR`), with a `current`
symlink naming the one a bare `ask` continues. One writer at a time: a
second is refused, because two processes appending to one log would
interleave two conversations and break every digest written afterwards. The
lock is an `flock(2)` on the session file, so a writer that dies releases it
on the way out — there is no lock file to find, and no session to unstick.

## The system prompt

The default is written for one situation: a model whose output is about to
be read by a program. No preamble, no sign-off, no markdown chrome, exact
shapes when a shape is asked for — and, the part that matters most, it
knows it cannot ask a clarifying question, because nothing is listening on
stdin. An ambiguous request gets the most useful reading plus a stated
assumption, never a question mark and an empty pipe.

It is a value, not a secret. `ask system` prints it, so extending it is
ordinary shell rather than a config format:

```
$ ask -S "$(ask system; cat house-style.md)" "draft the release note"
$ ask -S "" "raw model, no system prompt"
$ export ASK_SYSTEM="You are a terse SQL tutor."
```

Whatever wins is recorded verbatim in the session header and on every
request, so a log says what shaped it.

## The log

One JSONL file per conversation, one event per line, appended and never
rewritten:

```
session    the header: id, version, model, system prompt, SDK versions
user       what you asked
request    the exact normalized request — everything except the messages
assistant  the turn as it streamed, blocks and all
retry      a provider failure that was worth waiting out
done       how it ended: end, max_tokens, overflow, error
abort      interrupted
```

`user` and `assistant` events are the conversation; folding them produces
exactly what goes to the provider. The `request` event records everything
else about the call, and stands in for the messages with a SHA-256 digest
rather than a second copy of what the file already holds.

That is what makes the check possible. `ask replay -check` folds the events
before each request and compares the hash. If they ever disagree, the log
is not a record of what happened, and it says so:

```
$ ask replay -check
ask: replay divergence at request seq 4:
folded:  9c1d...
logged:  2ef8...
```

Three views over the same events, and no second logging pipeline anywhere:

```
$ ask replay              # re-render it for a human
$ ask replay -json        # raw JSONL for a program
$ ask replay -step 4      # the exact request, messages reconstituted
```

Reasoning state round-trips per provider — Anthropic thinking signatures,
OpenAI encrypted reasoning (`store: false`, always), Gemini thought
signatures, OpenRouter `reasoning_details` unmodified and in order — so a
continued conversation is genuinely the same conversation. Switching
providers mid-thread is allowed and degrades gracefully: each adapter
replays its own opaque state and ignores another's.

## Auth

Keys come from the environment: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
`GEMINI_API_KEY`, `OPENROUTER_API_KEY`.

Subscription logins are stored instead, in `~/.ask/auth.json` (mode 0600):

```
$ ask login openai-codex -from-codex     # import from the Codex CLI
$ ask auth
$ ask logout openai-codex
```

Corporate gateways are a transport property, not a provider feature: one
authenticating HTTP client shared by every adapter.

```
export ANTHROPIC_BASE_URL=https://gateway.corp/anthropic
export ASK_AUTH_URL=https://idp.corp/oauth/token
export ASK_AUTH_CLIENT_ID=... ASK_AUTH_CLIENT_SECRET=...
```

With `ASK_AUTH_URL` set, the vendor key becomes optional — the gateway owns
it. Tokens live in memory for the life of the process, are refreshed with
a margin, and are never written to disk. The transport never retries: `ask`
owns retries, and they have to be visible in the log.

Token endpoints must be `https`, except on loopback. They are bounded in
time, and they never follow a redirect. All three protect the same thing:
the form body carries a client secret or a refresh token — the long-lived
half — so http would put it on the wire in the clear, and Go would re-send
it to whatever host a 307 named. A gateway inside a corporate network is
still reached across a network.

```
ask: ASK_AUTH_URL: token endpoint http://idp.corp/oauth/token is http: a
client secret or refresh token would travel in the clear. Use https (http
is allowed only on loopback)
```

That is refused when the endpoint is configured, not when a token first
expires. The same rule covers `ask login -token-url` and a `token_url`
already sitting in the credential file. [SECURITY.md](SECURITY.md) has the
rest of what `ask` holds and where.

Anthropic on Google Vertex AI follows Claude Code's environment
conventions, so a machine configured for one works for the other:

```
export ANTHROPIC_VERTEX_PROJECT_ID=my-project CLOUD_ML_REGION=us-east5
```

The Vertex request reshaping happens below the logging layer, so request
events keep their normalized shape and the replay invariant is untouched.

## Think in shell

```bash
# a shape, not a chat
ask -S "$(ask system)" 'Return only JSON: {"sev":1-5,"why":"..."}' < alert.txt | jq .sev

# fan out over files, each its own conversation
ls *.go | xargs -P4 -I{} sh -c 'ask -n -q "review {}" < {} > {}.review'

# branch on the answer
if ask -q "is this a security fix? answer yes or no" < patch | grep -qi yes; then
  git tag security-$(date +%s)
fi

# a second opinion from another model, same question
ask -n -m openai/gpt-5 "$(ask replay -step 2 | jq -r '.messages[0].blocks[0].text')"

# prove the whole archive still replays
for f in ~/.ask/sessions/*.jsonl; do ask replay -check "$f" >/dev/null || echo "BAD $f"; done
```

## Commands

```
ask [flags] [message ...]       ask; piped stdin composes with the message
ask replay [flags] [session]    re-render a session (-check verifies replay)
ask system                      print the default system prompt
ask login openai-codex [flags]  store subscription auth (-from-codex)
ask logout <provider>           remove stored credentials
ask auth [list]                 list stored credential providers
ask version                     print the version
ask help                        print the summary
```

Anything that is not a command is a message. A word one edit from a
command is refused as a probable typo — otherwise `ask replya` is a paid
model call appended to your live conversation — and `--` forces one
through: `ask -- replay`.

Full reference in `ask.1`. Tests enforce that it stays true: every verb
appears in the man page synopsis, in `ask help`, and here; every flag,
environment variable and provider is documented; the man page carries the
same version the binary reports; the man page is lint-clean and pure ASCII;
help fits eighty columns; and requested help goes to stdout while misuse
goes to stderr.

## What it deliberately is not

`ask` is the conversational core of [mu](https://github.com/patrickyoung/mu),
a file-based agent, carved out as a filter. What was left behind was left
behind on purpose: no tools, no agent loop, no verifier, no workspace, no
skills, no config file, no REPL, no daemon, no MCP.

The pieces that came across are the ones that were hard to get right: the
provider adapters and their reasoning round-trips, the stream contract
every adapter is held to, gateway and subscription auth, and the replay
invariant. That is about 1,950 lines and most of the value. The CLI, the
log, the conversation loop and auth are the other 2,175 — 4,125 in total,
against mu's 9,500, and 3,900 lines of tests holding it there.

The turn loop itself is under sixty lines, and that is the point.

A REPL was the closest call, and the answer was no: the shell already is
one, and `ask` remembers between invocations.

## Contributing

Read [AGENTS.md](AGENTS.md) first — it is short, and it is the whole set of
rules a change is held to. The two that matter most: the Unix contract
(stdout is the answer alone, exit 2 means the context window and nothing
else) and the replay invariant (`event.Check` is never relaxed, only
migrated). New provider adapters earn their keep by passing `checkContract`
against a wire fixture.

```
go test ./...          # and -race when touching the log or a stream
```

`go test` also proves the documentation: every command appears in the man
page, in `ask help`, and here; every flag and environment variable is
documented; help fits eighty columns and goes to stdout while misuse goes to
stderr. A change that outruns its docs fails.

The list of things left out on purpose is at the end of AGENTS.md. It is a
feature, and adding one of them back is a conversation before it is a patch.

Security issues go through [SECURITY.md](SECURITY.md), not the issue
tracker.

## License

[MIT](LICENSE).
