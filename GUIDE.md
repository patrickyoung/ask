# The ask field guide

`ask` does one job: it turns a message into an answer.

The shell supplies the message. `ask` supplies the judgment. The shell uses
the answer.

```text
collect data  ->  ask  ->  inspect or act
```

This separation explains the interface. `ask` has no filesystem tools and
runs no commands. If the model needs a fact, put that fact in the message.

## Choose the conversation first

A plain `ask` starts a new conversation. Continuing is explicit.

```sh
ask 'Explain this interface.'       # new conversation
ask -c 'Now give one example.'      # continue current
ask 'Review this file.' < x.go      # another new conversation
ask -f audit.jsonl 'Begin audit.'   # named conversation
```

Use these rules:

- Interactive follow-up: use `-c`.
- Independent script: plain `ask` is already fresh.
- Work with a stable name: use `-f file.jsonl`.
- Parallel work: plain invocations are independent; use distinct `-f` paths
  for named threads.

One session has one writer. A second writer is refused because interleaved
turns would no longer describe one conversation.

## Form the message

Arguments are the instruction. Text on stdin is the evidence.

```sh
git diff | ask 'Write a commit message.'
go test ./... 2>&1 | ask 'Explain the first useful failure.'
ask 'Explain compare-and-swap in two sentences.'
```

If both are present, `ask` sends the instruction followed by stdin inside
`<stdin>` delimiters. If only stdin is present, stdin is the whole message.
Surrounding whitespace on textual stdin is removed.

This is also how to provide facts the model cannot know:

```sh
date -u | ask 'State this time in ISO 8601.'
uname -a | ask 'Explain this platform string.'
```

Do not ask the model to open `/tmp/x`, inspect the current repository, or run
`date`. It cannot. Let the shell do those operations and pass their results.

## Request a machine-readable answer

Use `-schema` when another program will consume JSON.

```sh
cat > finding.schema.json <<'JSON'
{
  "type": "object",
  "properties": {
    "found": {"type": "boolean"},
    "reason": {"type": "string"}
  },
  "required": ["found", "reason"],
  "additionalProperties": false
}
JSON

ask -q -schema finding.schema.json \
  'Does this log show an out-of-memory failure?' < kernel.log |
  jq -e '.found'
```

The provider receives its native structured-output parameter. After the turn,
`ask` parses the text and validates the value against the same schema before
exit 0.

A refusal, invalid JSON, schema mismatch, or incomplete provider stop exits 1
without emitting a normal answer. The completed provider turn remains in the
log. `-json` is different by request: it emits the raw events even when the
structured answer is rejected.

The schema is limited to 1 MB and must be one JSON object. Local fragment
references work. External references do not. With `-schema -`, stdin contains
the schema, so put the question in argv.

## Attach files

Use `-a` for a regular file:

```sh
ask -a chart.png 'State the trend.'
ask -a old.pdf -a new.pdf 'List the changed claims.'
```

The order of `-a` flags is preserved. Type is determined from content:

- Recognized media becomes an attachment.
- Valid UTF-8 without NUL bytes becomes labelled text.
- Other bytes are refused.

Binary stdin is treated as one attachment:

```sh
screencapture -x -t png - | ask 'Describe this image.'
```

Provider families differ:

| provider | media |
| --- | --- |
| Anthropic | images, PDF |
| OpenAI and OpenAI Codex | images, PDF |
| Gemini | images, audio, video, PDF |
| OpenRouter | images, PDF, WAV, MP3 |

The check is per provider, not per model. A model can support less.

Files are read whole. `-a` accepts at most 16 files, 16 MB each and 32 MB in
total. Stdin has its own 16 MB limit. Too much input is an error, never a
partial message.

## Let the shell make the decision

Exit 0 means the model answered. It does not mean the answer was “yes.” Test
the answer itself.

For prose:

```sh
if git diff --staged |
   ask -q 'Does this change authentication? Answer yes or no.' |
   grep -Eiq '^yes$'; then
  echo 'security review required'
fi
```

For automation, structured JSON is safer:

```sh
if ask -q -schema finding.schema.json \
     'Does this change authentication?' < patch.json |
   jq -e '.found' >/dev/null; then
  echo 'security review required'
fi
```

Model output is untrusted input. Do not pipe it into `sh` or `eval` unless
you have chosen to execute untrusted text.

## Run independent work in parallel

Plain calls create independent sessions, so they can run in parallel. Output
still needs a key because processes finish out of order.

```sh
mkdir -p out
i=0
for file in *.go; do
  i=$((i + 1))
  key=$(printf '%03d' "$i")
  (ask -q "Review $file in five lines." < "$file" > "out/$key") &
done
wait
cat out/*
```

Each plain call gets its own session. Numbered files restore input order.
Do not run parallel `-c` calls: only one writer may hold a session.
After parallel plain calls, which one is `current` is unspecified; name any
thread whose identity matters with `-f`.

## Handle limits and retries

`ask` retries retryable provider failures up to six attempts. Retry waits and
causes are recorded in the session. Model streams have no overall deadline;
use the operating system's `timeout` command, or `gtimeout` from GNU
coreutils on macOS, when a caller needs one.

Exit 2 has one meaning: a model context window is full. For an ask turn, do
not retry the same session. Either start over:

```sh
ask 'Restate the task with only the needed input.'
```

or compact the session:

```sh
new=$(ask compact)
ask -f "$new" 'Continue with the next case.'
```

Compaction is explicit because a model decides what the handoff retains. The
source is unchanged. The summarizer call has its own session, and the new
session names both its parent and summarizer.

## Inspect the record

Use replay for four different questions:

```sh
ask replay                 # What happened?
ask replay -json           # What events are stored?
ask replay -check          # Does every request match its preceding fold?
ask replay -step 4         # What normalized request was made at seq 4?
```

`-check` is stronger than “the JSON parses.” For every request event, it
folds all preceding user and complete assistant events and compares that
conversation with the recorded digest.

Use `note` for an attributed fact that belongs in the record but should not
become model context:

```sh
go test ./... >/tmp/test.out 2>&1
status=$?
ask note -s test "exit $status"
```

A note has no model call and is not folded. `-s` is required so the record
says who wrote it.

## Control the system prompt

Print the built-in value with `ask system`:

```sh
ask -S "$(ask system; cat project-style.md)" 'Draft the release note.'
ask -S '' 'Answer with no system prompt.'
```

Precedence is simple:

1. Explicit `-S`, including `-S ''`.
2. `$ASK_SYSTEM`.
3. The built-in prompt.

The choice is made again on every invocation. A one-time `-S` does not become
the default for later turns; export `ASK_SYSTEM` or pass `-S` again when it
must remain in force. The session header records the creation prompt, and each
request records the prompt used for that call.

## Keep sessions usable

Sessions contain the resulting user messages and the bytes of every media
attachment. Treat them like their inputs. Useful maintenance commands are:

```sh
for file in ~/.ask/sessions/*.jsonl; do
  ask replay -check "$file" >/dev/null || echo "bad: $file"
done

cp ~/.ask/sessions/SESSION.jsonl experiment.jsonl
ask -f experiment.jsonl 'Try the other assumption.'
```

Copying is the exact branch operation because a session is self-contained.

## Reference card

```text
ask 'question'                  start a new conversation
ask -c 'question'               continue the current conversation
ask -f thread.jsonl 'question'  use a named conversation
command | ask 'instruction'     combine evidence and instruction
ask -a file 'instruction'       attach a file; repeat -a as needed
ask -schema schema.json 'task'  require validated JSON

ask replay                     render the current session
ask replay -check              verify request digests
ask replay -step N             reconstruct one normalized request
ask compact                    continue from a model-written handoff
ask note -s source 'text'      append a record, not a message

-q          suppress progress on stderr; errors still print
-json       emit raw events instead of the normal answer
-effort     off, low, medium, high, or xhigh; mapping varies by provider
-max-tokens maximum output tokens; default 16384; refused by openai-codex
-m          provider/model

exit 0  success
exit 1  error
exit 2  context window full
exit 130 interrupted
```
