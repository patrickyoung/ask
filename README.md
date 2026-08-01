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
go install ./...          # or: go build -o ask . && mv ask ~/bin
export ASK_MODEL=anthropic/claude-sonnet-5
export ANTHROPIC_API_KEY=...
```

Five providers, one flag: `anthropic`, `openai`, `openai-codex`, `gemini`,
`openrouter`. Models are `provider/model`, and OpenRouter models keep their
own slash (`openrouter/anthropic/claude-sonnet-4.5`).

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
second is refused by name, because two processes appending to one log would
interleave two conversations and break every digest written afterwards.

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
appears in the man page synopsis, in `ask help`, and here; every flag and
environment variable is documented; the man page is lint-clean and pure
ASCII; help fits eighty columns; and requested help goes to stdout while
misuse goes to stderr.

## What it deliberately is not

`ask` is the conversational core of [mu](https://github.com/patrickyoung/mu),
a file-based agent, carved out as a filter. What was left behind was left
behind on purpose: no tools, no agent loop, no verifier, no workspace, no
skills, no config file, no REPL, no daemon, no MCP.

The pieces that came across are the ones that were hard to get right: the
provider adapters and their reasoning round-trips, the stream contract
every adapter is held to, gateway and subscription auth, and the replay
invariant. That is about 1,900 lines and most of the value. The CLI, the
log and the conversation loop are the other 1,600 — 3,500 in total, against
mu's 9,500.

The turn loop itself is under sixty lines, and that is the point.

A REPL was the closest call, and the answer was no: the shell already is
one, and `ask` remembers between invocations.
