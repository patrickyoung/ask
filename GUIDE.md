# The ask field guide

Everything here was run on macOS 26.5.2 against a real ChatGPT/Codex subscription
(`openai-codex/gpt-5.6-sol`) while writing this guide. Numbers are measured,
not estimated. Where something failed, it says so.

```
ask login openai-codex -from-codex
export ASK_MODEL=openai-codex/gpt-5.6-sol
```

> The model name matters. `gpt-5.1-codex` returns *"not supported when using
> Codex with a ChatGPT account"*. Use whatever your `~/.codex/config.toml`
> says. `awk -F'"' '/^model/{print $2}' ~/.codex/config.toml`

---

## The one idea

`ask` has no tools. It cannot open a file, run a command, browse the web, or
see an image. It reads stdin and writes stdout. That sounds like a
limitation, and it is — but it is also the whole design:

**The shell is its hands. The log is its memory.**

Every pattern in this guide is the same three beats:

```
gather evidence (shell)  →  reason over it (ask)  →  act on the result (shell)
```

The tool that gathers is `ps`, `git`, `log show`, `pbpaste`, `sqlite3`,
`osascript` — things your Mac already has and that already do their jobs
perfectly. `ask` is the missing verb between them: *understand this*.

---

## What it is good at

### 1. Swallowing something enormous in one gulp

The entire `mu` Go codebase — 517 KB, 46 files — piped into a single
invocation:

```bash
find ~/projects/micro-agent -name '*.go' -exec cat {} + |
  ask "Name the single most subtle invariant this codebase protects,
       and the file that enforces it. Two lines max."
```

```
Every request's logged conversation digest must exactly match folding all
preceding context-bearing events.
internal/event/fold.go (event.Check)
```

**149,300 input tokens, 8.5 seconds, correct.** That is not summarisation —
it found the one architectural invariant that makes the project work, and
named the function that enforces it.

Measured ceiling: **~1.2 MB of text accepted** in one call. At 6 MB the
request never completed. There is no chunking inside `ask`; see the rolling
digest below for corpora bigger than the window.

### 2. Producing exact shapes, reliably enough to drive a shell

Five independent runs of the same request:

```bash
ask -n -q 'Return ONLY minified JSON, no fence: {"n":<the integer 7>,"w":"<the word seven>"}'
```

```
{"n":7,"w":"seven"}    ← 5/5 identical
```

One-word contracts held 3/3. This is what makes `ask` safe in a pipe: you
can `| jq` the output without defensive parsing. The default system prompt
does the heavy lifting here — it tells the model its output is data.

### 3. Remembering across separate processes

This is the capability no stateless LLM CLI has, and it is the reason `ask`
keeps a log. Six separate invocations, six separate processes, **one
accumulating conversation**:

```bash
ask -n -q "I'll show you one Go package per message. Reply READY."
for pkg in event provider tool agent workspace; do
  cat ~/projects/micro-agent/internal/$pkg/*.go |
    ask -q "Package $pkg. Reply with only its name and a 6-word purpose."
done

ask -q 'Across ALL five packages you have seen, name the one cross-package
        coupling that would break the replay invariant if someone changed
        it carelessly. Name the two packages and the mechanism.'
```

```
event ↔ provider
provider.Request.Logged stores Digest(Messages); event.Check recomputes
provider.Digest(event.Fold(priorEvents)).
Changing Fold semantics or Message/Block JSON serialization carelessly
makes existing logs diverge.
```

**7 turns, 330,899 accumulated input tokens, across 6 processes.** No single
file contains that answer. The shell loop did the map; the conversation did
the reduce.

### 4. Generating its own falsifiable checks

Ask it for findings *plus a command that would prove each one*, then run
them. This is a verification loop built out of nothing but the filter
contract:

```bash
ask -n -q "You are auditing a macOS machine. I'll send several system views."
ps aux                       | ask -q "System view: running processes."
netstat -an | grep -i listen | ask -q "System view: listening sockets."
launchctl list               | ask -q "System view: launchd jobs."
brew outdated --verbose      | ask -q "System view: outdated packages."

ask -q 'Correlate everything. Output ONLY a JSON array, highest risk first:
[{"finding":"","evidence":"which two views","severity":"high|medium|low",
  "check":"a shell command I can run to verify"}]' > audit.json

jq -r '.[] | "\(.severity)\t\(.finding)\t\(.check)"' audit.json |
while IFS=$'\t' read -r sev finding check; do
  echo "[$sev] $finding"; eval "$check" 2>&1 | head -3 | sed 's/^/  | /'
done
```

Real output from this machine — **3 of 4 findings confirmed** by executing
the model's own checks:

```
[medium] Privileged third-party PACE license daemon persists with launch agent
  | root  333  /Library/PrivilegedHelperTools/licenseDaemon.app/...licenseDaemon
  | gui/502/com.paceap.eden.licensed.agent = { active count = 0
[low] Outdated Ollama and llama.cpp services are actively running locally
  | llama.cpp
  | ollama
  | 46930
```

The fourth needed `sudo` and stayed unverified — which is the point. You get
claims you can check, not claims you must trust.

### 5. Saying "I don't know"

Asked to triage a log file that turned out to be empty, it produced:

```
| No log data provided | 0 | No unified-log entries were available. | Unknown |
```

It did not invent a table. In a pipeline that runs unattended, a model that
fabricates when the input is empty is worse than no model.

### 6. Leaving evidence

That audit is still on disk and still provable:

```bash
$ ask replay -check 20260801-172845-ffa4884688e883d7.jsonl
ok: ...jsonl replays exactly (25 events)
```

25 events, 6 turns, 269,888 input tokens, 190 KB — including all **137,567
characters of `ps aux`** exactly as they were sent. `ask replay -step N`
reconstitutes the precise request from the fold. Six months from now you can
prove what the machine looked like when the finding was made.

### Seeing

`-a` attaches a file; binary on stdin is an attachment too. Measured live
against Codex:

```bash
$ screencapture -x -t png /tmp/shot.png       # 858 KB, 2560x1440
$ ask -a /tmp/shot.png "In one sentence: what application is in the
    foreground and what is it showing?"

Terminal is in the foreground, showing a Claude Code session editing and
testing Go attachment-handling code.
```

It read my actual screen, correctly, in 4.2 seconds. The no-flag form works
too, because binary stdin is sniffed:

```bash
screencapture -x -t png - | ask "what is on my screen?"
```

PDFs work the same way, and this is where it earns its keep — a document
you would otherwise have to open and read:

```bash
$ cupsfilter q.txt > q.pdf
$ ask -a q.pdf "What is the single biggest risk named in this document?"

The biggest risk is customer concentration, with one customer accounting
for 38% of revenue.
```

A file's type comes from its bytes, never its name, so `-a main.go` inlines
as text (and the log stays greppable) while `-a shot.png` attaches. What a
provider cannot carry is refused *before* anything is logged:

```
$ ask -a clip.wav "transcribe"
ask: /tmp/clip.wav: openai-codex does not accept audio/wav
     (it takes application/pdf, image/*)
```

For audio and video, point `-m` at Gemini, which carries both.

---

## What it is not good at

Measured, not theorised. Each of these was tested.

| Limit | What actually happens | What to do |
| --- | --- | --- |
| **No filesystem** | *"I can't access /tmp/secret.txt or read files from your filesystem."* | `cat file \| ask ...` |
| **No commands** | *"I can't run commands or access this machine."* | `uname -a \| ask ...` |
| ~~No vision~~ | Fixed: `-a photo.png`, or pipe binary in | see Attachments |
| **No clock** | Asked the date, answered **"March 26, 2026"** — confidently wrong, on 1 August | `date \| ask ...` |
| **No timeout** | 6 retries with escalating backoff; a flaky connection blocked for **minutes** | see below |
| **Context ceiling** | ~1.2 MB fine; 6 MB never returned | rolling digest, below |
| **No cost on Codex** | subscription auth reports no price | use OpenRouter if you need dollars |

### The clock one will bite you

```bash
$ ask -n -q "What is today's date? If you cannot know, say so."
Today is March 26, 2026.          # ← it was 1 August 2026
```

It refuses file access honestly. It does **not** refuse the date. Any prompt
whose answer depends on *now* must be handed the time:

```bash
$ date | ask -n -q "Today's date is in stdin. What is it, and what day?"
August 1, 2026 — Saturday
```

### There is no timeout, and macOS has no `timeout`

Neither `timeout` nor `gtimeout` exists on a stock Mac. When the connection
to the provider went flaky, `ask` retried six times with escalating backoff
and blocked for over two minutes. Bound it yourself:

```bash
# a portable hard bound, no coreutils required
ask_t() { ( "$@" & p=$!; ( sleep "${ASK_TIMEOUT:-60}"; kill -9 $p 2>/dev/null ) & k=$!
            wait $p; r=$?; kill $k 2>/dev/null; return $r ) }

ask_t ask -q "..." < big.txt || echo "gave up"
```

Or `brew install coreutils` and use `gtimeout 60 ask ...`.

When it *does* stall, the log tells you why — that is what the log is for:

```bash
$ jq -c 'select(.type=="retry").data' ~/.ask/sessions/current
{"attempt":1,"status":0,"wait_ms":1425,"error":"..."}
{"attempt":2,"status":0,"wait_ms":4035,"error":"..."}
```

`status: 0` means the request never reached an HTTP response at all.

---

## The three gotchas that cost me the most time

### 1. Parallel fan-out silently scrambles your answers

`xargs -P` returns results in **completion** order, not input order. This
looks like it works and is wrong:

```bash
cat questions.txt | xargs -P8 -I{} sh -c 'ask -n -q "{}"'
```

```
capital of Peru | 1970                      ← wrong
author of the C programming language | Homebrew   ← wrong
```

Carry the key through, or key the output by index:

```bash
mkdir -p out; i=0
while IFS= read -r q; do
  i=$((i+1))
  ( printf '%s\t%s\n' "$q" "$(ask -n -q "Answer briefly: $q")" \
      > "out/$(printf '%03d' $i)" ) &
done < questions.txt
wait; cat out/*
```

```
capital of Peru                     Lima
author of the C programming language Dennis Ritchie
what signal is 9                    SIGKILL
```

**8 questions in 5 seconds**, correctly attached.

### 2. Parallel means `-n` (or `-f`), always

One writer per session, enforced by a lock. Concurrent `ask` calls without
`-n` will refuse each other by name — correctly, because interleaving two
conversations into one log would break every digest after the first. Use
`-n` for independent questions, `-f thread.jsonl` for a named thread.

### 3. `ask` continues by default

`ask "..."` joins the current conversation. That is the feature, but in a
script it means run #2 inherits run #1's context. It always says so on
stderr:

```
ask: 20260801-172603-6c97... · 1 turns so far (-n starts fresh)
```

In any pipeline that should be stateless, pass `-n`.

---

## Recipes worth stealing

Everything below was run.

### Beat the context ceiling: the rolling digest

For a corpus larger than any window, carry a fixed-size state forward. This
digested **1.4 MB of `git log -p`** through a bounded context:

```bash
git log -p --no-color > history.txt        # 1.7 MB
split -b 350000 history.txt chunk.
: > state.md
for c in chunk.*; do
  { echo "=== RUNNING NOTES ==="; cat state.md
    echo "=== NEW CHUNK ==="; cat "$c"; } |
  ask -n -q 'Update the running notes with anything new in this chunk.
             Output ONLY the updated notes: at most 10 bullet lines, each a
             distinct theme. Merge related items; do not append blindly.' \
    > state.next && mv state.next state.md
done
cat state.md
```

A real line out of the result, which no single chunk contained:

> Removed `mu serve` and its embedded web surface; append-only JSONL logs,
> `mu replay`, sidecar notes and machine-readable event streams remain the
> external viewing and steering interface.

`-n` on each chunk is deliberate: state lives in `state.md`, not in the
conversation, so memory is bounded no matter how long the corpus is.

### The clipboard as universal I/O

The single highest-value macOS one-liner. Works with any app on the system.

```bash
pbpaste | ask -n -q "Fix grammar. Output only the corrected sentence." | pbcopy
```

```
in:  we was gonna go to the store but it was closed so we went home
out: We were going to go to the store, but it was closed, so we went home.
```

Bind it to a hotkey with Automator → Quick Action → Run Shell Script, and
you have system-wide LLM text transformation in every text field on the Mac.

### Commit messages from the diff

```bash
git diff --staged | ask -n -q "Write a git commit message: a 50-char
  subject, blank line, then why (not what). No fences." | git commit -F -
```

### Speak the answer, and notify when a long job lands

```bash
ask -n -q "Summarise in one sentence" < report.txt | tee /tmp/a.txt
say -v Samantha "$(cat /tmp/a.txt)"
osascript -e 'display notification "ask: done" with title "asklab"'
```

Both verified working.

### Branch a script on a judgement

The exit code is the point. `ask` answers, the shell decides:

```bash
if git diff --staged | ask -q "Does this diff touch authentication or
     crypto? Answer yes or no." | grep -qi '^yes'; then
  echo "security-sensitive — requesting review" >&2
fi
```

### Exit 2 means stop, not retry

The one exit code worth branching on explicitly. A full context window fails
identically forever, so a supervisor must not retry it:

```bash
ask -q "..." < huge.log
case $? in
  0) : ;;
  2) echo "context full — starting fresh"; ask -n -q "..." < huge.log ;;
  *) echo "transient — retrying"; sleep 5; ask -q "..." < huge.log ;;
esac
```

### Extend the system prompt instead of replacing it

The default is a value, so composing is ordinary shell:

```bash
ask -S "$(ask system; cat ~/.ask/house-style.md)" "draft the release note"
```

`ask system` prints the built-in prompt — 231 words written for a model
whose output is about to be read by a program. Its most important rule is
one most chat prompts get wrong: *you cannot ask a clarifying question,
because nothing is listening on stdin.* Ambiguity gets the most useful
reading plus a stated assumption, never a question mark and an empty pipe.

### Audit your own usage

```bash
jq -s 'map(select(.type=="assistant").data.usage.in) | add' ~/.ask/sessions/*.jsonl
for f in ~/.ask/sessions/*.jsonl; do ask replay -check "$f" >/dev/null || echo "BAD $f"; done
```

---

## The demonstration that convinced me

I piped `ask`'s own complete source — 107 KB — into `ask`, and asked for the
single most likely latent bug:

```bash
{ for f in $(git ls-files '*.go' | grep -v _test); do
    echo "===== $f ====="; cat $f; done; } |
  ask -n -q -effort high 'Find the single most likely LATENT BUG — wrong
    now, will bite a real user. Output only: FILE / CLAIM / WHY / REPRO.'
```

```
FILE: main.go
CLAIM: The -q flag silently discards a successful answer whenever stdout
       and stderr refer to the same terminal, file, or pipe.
WHY:   With -q, cmdAsk installs no renderer, so the answer is never
       streamed to stderr. finish still suppresses its stdout write when
       sameOut returns true, incorrectly assuming the renderer already
       emitted the answer.
REPRO: tmp=$(mktemp); ask -q -n 'Print exactly: visible' >"$tmp" 2>&1; ...
```

It was right. `ask -q "question"` typed in a terminal — where both streams
are the tty — printed **nothing at all** and exited 0. The existing test
missed it because it used two separate temp files, which is the one case
that cannot reproduce it.

The bug is fixed (`cb45b9f`) and the regression test now covers both cases.
A tool that finds a severe bug in its own source, names the mechanism, and
hands you a working repro is doing real work.

Two other bugs were found the same way while writing this guide: the OpenAI
SDK swallowing error bodies it does not recognise — which also broke
context-overflow detection, and so the exit code — and retry events that
recorded `status: 0` without saying why.

---

## Reference card

```
ask "question"              continue the current conversation
ask -n "question"           start a fresh one          ← use in scripts
ask -f t.jsonl "question"   a named thread of your own
cmd | ask "instruction"     stdin is evidence, argv is the instruction
cmd | ask                   stdin alone is the whole question
ask -a f.png "question"     attach a file; repeat -a for more
cmd | ask "question"        binary on stdin attaches itself

-q          answer only, no progress on stderr
-json       raw event stream on stdout instead of the answer
-S text     replace the system prompt   (ask system prints the default)
-effort     off | low | medium | high
-m spec     provider/model               ($ASK_MODEL)

ask replay            re-render the current session
ask replay -check     prove it replays exactly
ask replay -step N    the exact request sent at seq N

exit 0 answered · 1 error · 2 context window full · 130 interrupted
stdout = answer · stderr = progress · ~/.ask/sessions/ = the record
```

**Rules of thumb**

- Anything time-dependent: pipe `date` in.
- Anything in a script: `-n`.
- Anything parallel: `-n`, and key the output by index.
- Anything unattended: bound it with a timeout and branch on exit 2.
- Anything you might have to defend later: it is already in the log.
- Anything visual: `-a` it, or pipe it — but check the provider carries it.
