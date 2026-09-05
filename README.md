# ask

`ask` puts one question through a language model and writes the answer to
standard output.

```sh
ask 'What does this error mean?' < build.log
git diff | ask 'Write a commit message.'
ask -a chart.png 'What changed?'
```

It is a Unix filter:

| result | meaning |
| --- | --- |
| stdout | the answer only |
| stderr | progress and errors: streamed output, reasoning, retries, and usage |
| exit 0 | the command completed successfully |
| exit 1 | an error occurred |
| exit 2 | a model context window is full |
| exit 130 | the run was interrupted |

`-q` suppresses progress, not errors. `-json` replaces the normal answer with
the raw events written by that invocation.

An answer cut off at the output limit, an unfinished stream, or another
non-successful provider stop exits 1 with no normal answer on stdout.
Progress may already have appeared on stderr. `-json` still emits the recorded
events. Reported stdout write errors also exit 1; a closed downstream pipe
retains Unix SIGPIPE behavior. Output may already be partially written, so
consumers must check the exit status.

Each plain `ask` starts a fresh conversation. Use `-c` only when the new
question should include the current conversation.

## Install

`ask` requires Go 1.26 or newer and a Unix system: Linux, macOS, BSD, or WSL.

```sh
go install github.com/patrickyoung/ask@latest
export ASK_MODEL=anthropic/your-model
export ANTHROPIC_API_KEY=...
```

The model syntax is `provider/model`. The supported providers are:

- `anthropic`
- `openai`
- `openai-codex`
- `gemini`
- `openrouter`

OpenRouter model names retain their slash, for example
`openrouter/anthropic/your-model`.

OAuth credentials come from the separate `oauth` filter. Ask receives one
Authorization header on an inherited descriptor and never logs in, stores a
token, or refreshes one:

```sh
oauth with llm -- ask -header-fd 3 -m openai/your-model 'Hello.'
```

The same boundary serves `openai-codex`. Set `OPENAI_CODEX_ACCOUNT_ID` when
that account requires its non-secret routing id.

## Input

Arguments form the instruction. Standard input supplies the data:

```sh
ask 'Explain mutexes in one paragraph.'
go test ./... 2>&1 | ask 'Find the first useful failure.'
```

With both, `ask` sends the argument text followed by stdin inside `<stdin>`
delimiters. With stdin alone, stdin is the whole message. Surrounding
whitespace on textual stdin is removed.

Normalized `context/v1` JSONL remains ordinary message input, but Ask labels it
in the `user` event with a compact evidence manifest. The exact content remains
inline only once; the manifest records its byte location and SHA-256 digest and
indexes ordered refs, sources, retrieval times, citations, queries, and
connector fingerprints. Claimed Context input that is damaged is refused
before a session is created. Ask also recognizes Context JSONL inside the
standard `<stdin>` envelope when a coordinator such as Ply has already composed
the instruction and evidence into one turn.

Anything that is not a command is treated as a message. A word one edit away
from a command is refused as a likely typo. `--` forces it to be a message:

```sh
ask -- replay
```

`ask` cannot read a path named in a prompt or run a command. Use the shell to
produce the input, or attach the file.

## Conversations

A plain invocation starts a new conversation:

```sh
ask 'What is a monad?'
ask -c 'Show one Go example.'    # continues current
ask 'Start an unrelated task.'   # starts fresh
```

Use `-c` to continue the current session. It is an error when no current
session exists. Use `-f` for an explicitly named session, including one
outside the normal conversation directory:

```sh
ask -f review.jsonl 'Review this patch.' < patch.diff
ask -f review.jsonl 'List the remaining risks.'
```

Sessions normally live in `~/.ask/sessions` or `$ASK_DIR`. Each plain `ask`
becomes `current`, and `ask -c` continues exactly that symlink. A missing or
dangling `current` is an error; `ask` does not guess from the newest file.
A session named with `-f` never changes `current`.

Only one process may write a session at a time. The lock is an `flock(2)` on
the session file and is released by the kernel when the process exits.

If a conversation fills the context window, `ask` exits 2. Repeating the same
request cannot fix that conversation. Start fresh with a plain `ask`, or
compact it:

```sh
ask compact
```

Compaction creates two new files. One records the summarizer call. The other
starts a new conversation from the resulting handoff note. The source session
is not changed. The new message is stamped `source: "summary"`, and its header
names the original session and the summarizer session.

The source snapshot must pass the same verification as `replay -check` before
the summarizer is called. A failed or incomplete summary leaves its own session
for inspection, creates no handoff session, and does not move `current`.

To branch without summarizing, copy the session file and use `-f`:

```sh
cp ~/.ask/sessions/SESSION.jsonl what-if.jsonl
ask -f what-if.jsonl 'Assume the other design was chosen.'
```

## Structured JSON

`-schema` makes the output shape part of the provider request:

```sh
cat > result.schema.json <<'JSON'
{
  "type": "object",
  "properties": {"severity": {"enum": ["low", "medium", "high"]}},
  "required": ["severity"],
  "additionalProperties": false
}
JSON

ask -schema result.schema.json 'Classify this alert.' < alert.txt |
  jq -r .severity
```

The schema is sent through each provider's native structured-output field.
It is not inserted into the system prompt or user message. `ask` then parses
the completed document and validates it locally.

In normal answer mode, invalid JSON, a schema mismatch, a refusal, or an
incomplete provider stop exits 1 and emits no answer on stdout. The provider
turn remains in the append-only log. With `-json`, stdout still contains the
raw events requested by the caller.

Schemas must be JSON objects no larger than 1 MB. Fragment references within
the document work. External references are refused. `-schema -` reads the
schema from stdin, so the question must then come from the arguments.

Native structured output is model-dependent. An unsupported model fails at
the provider; `ask` does not fall back to prompt instructions. OpenRouter is
required to choose an endpoint that accepts the structured-output parameters.

## Attachments

`-a` attaches a regular file and may be repeated:

```sh
ask -a chart.png 'Describe the trend.'
ask -a q3.pdf -a q4.pdf 'What changed?'
screencapture -x -t png - | ask 'What is on this screen?'
```

The type comes from the bytes, not the filename. Recognized media is attached.
Valid UTF-8 without NUL bytes is inlined as labelled text. Other data is
refused before a session file is created. A directory, device, socket, or FIFO
is also refused; attachment files are opened non-blocking.
Empty files are refused. `-a -` is not supported; pipe stdin instead.

| provider | accepted attachment families |
| --- | --- |
| `anthropic` | images, PDF |
| `openai` | images, PDF |
| `openai-codex` | images, PDF |
| `gemini` | images, audio, video, PDF |
| `openrouter` | images, PDF, WAV, MP3 |

The table is per provider. A particular model may support less and can still
reject the request.

Limits are fixed:

- 16 files supplied with `-a`
- 16 MB per `-a` file
- 32 MB total across `-a` files
- 16 MB on stdin

Crossing a limit is an error; input is never silently truncated. Attachment
bytes are stored in the session, so the log remains self-contained. Session
files are created with mode 0600.

## The log

A session is append-only JSONL. Each line is one event:

| event | contents |
| --- | --- |
| `session` | session id, version, model, system prompt, SDK versions |
| `user` | the message, attachments, and optional Context evidence manifest |
| `request` | normalized request fields and a digest of the folded messages |
| `assistant` | streamed blocks, usage, model, and provider stop reason |
| `retry` | a retryable provider failure and wait |
| `done` | successful end, ordinary error, or context overflow |
| `abort` | interruption |
| `note` | attributed text that is recorded but not folded into conversation |

`user` and complete `assistant` events form the conversation. Notes, retries,
and other records do not.

Before each provider call, `ask` hashes the folded conversation and stores the
digest on the request event. Replay can check every request against the events
that precede it:

```sh
ask replay                    # render the current session
ask replay -json              # print its raw JSONL
ask replay -check             # verify request folds, sequence, and record seals
ask replay -check -json       # print that same verified event snapshot
ask replay -step 4            # reconstruct the request at sequence 4
```

Reasoning state is retained in the provider's own replayable form. When a
session switches providers, text survives; opaque reasoning from the other
provider is ignored.

`ask note` appends an attributed record without calling a model:

```sh
ask note -s deploy -f run.jsonl 'released as v1.4.2'
```

The session file must already exist. `-s` is required. Notes are not folded
into the conversation.

Programs can instead append typed JSON evidence and require a durable prefix
seal. JSON on stdin keeps evidence out of argv:

```sh
printf '%s' '{"status":"accepted","exit_code":0}' |
  ask note -s ply -f run.jsonl -k ply.verifier/v1 -json - -seal
ask replay -check run.jsonl
```

`-k`, `-json`, and `-seal` are atomic: all three are required. Replay checks
that each structured note is immediately sealed and that its exact event
prefix, event numbering, and every request fold still match. A seal detects
later edits; it attributes the record to the named local program, not to a
remote identity or cryptographic signing key. Because the digest is stored in
the same append-only file, it does not prove that the file was not truncated
back to an earlier valid sealed prefix.

Replay also reconstructs every Context evidence manifest from the exact bytes
inside its user message. Changing the snapshot, digest, ordered refs, citation
locations, query, or connector fingerprint makes the check fail. Evidence is
inline, so replay never refreshes a source or accepts a missing external blob.
Ask is the only session writer. Cite remains a filter with no log access; when
Ply runs `cite evidence.jsonl` as a verifier, Ply asks Ask to append the later
verdict as a sealed `ply.verifier/v1` note in this same history.

## System prompt

`ask system` prints the built-in prompt. `-S` replaces it for one invocation,
and `$ASK_SYSTEM` sets the default override:

```sh
ask -S "$(ask system; cat house-style.md)" 'Draft the release note.'
ask -S '' 'Use no system prompt.'
```

The choice is recomputed on every invocation; a `-S` value does not persist
to later turns. The session header records its creation prompt, while each
request event records the prompt used for that call.

## Authentication and gateways

API-key providers read:

- `ANTHROPIC_API_KEY`
- `OPENAI_API_KEY`
- `GEMINI_API_KEY`
- `OPENROUTER_API_KEY`

Each provider endpoint can be replaced with a corresponding base URL:

- `ANTHROPIC_BASE_URL`
- `OPENAI_BASE_URL`
- `OPENAI_CODEX_BASE_URL`
- `GEMINI_BASE_URL`
- `OPENROUTER_BASE_URL`

For OAuth, log in once with the independent
[`oauth`](https://github.com/patrickyoung/oauth) filter, then compose it with
Ask. The access token travels only through descriptor 3:

```sh
oauth with llm -- ask -header-fd 3 -m anthropic/your-model 'Hello.'
```

`-header-fd` also works with `ask compact`. An inherited authorization header
makes the corresponding vendor API key optional. Ask binds the header to the
first provider origin and refuses to send it across a redirect. Token login,
refresh, rotation, resource binding, and durable credential storage remain
OAuth's work.

Anthropic models can use the Vertex wire format:

```sh
export ANTHROPIC_VERTEX_PROJECT_ID=my-project
export CLOUD_ML_REGION=us-east5
```

`ANTHROPIC_VERTEX_BASE_URL` overrides the derived Vertex endpoint. `ask` does
not obtain Google credentials; use `oauth with ... -- ask -header-fd 3 ...` or
a proxy that adds them.

## Commands

```text
ask [flags] [message ...]       ask; stdin composes with the message
ask replay [flags] [session]    render, inspect, or check a session
ask compact [flags] [session]   continue a full session from a handoff
ask note -s src [flags] [text]  append text or sealed structured JSON
ask system                      print the built-in system prompt
ask version                     print the version
ask help                        print the built-in summary
```

Run `ask help` for the short reference. [ask.1](ask.1) is the complete manual,
and [GUIDE.md](GUIDE.md) explains reliable shell patterns.

## Scope

`ask` has no tools, agent loop, model-verifier loop, workspace, skills,
configuration file, REPL, daemon, or MCP client. The shell supplies data and
decides what to do with the answer. Local JSON Schema validation only decides
whether structured bytes may reach stdout; it does not add another model turn.

## Compatibility of failure handling

The session format, request digests, and folding of existing logs are unchanged;
no migration is needed. Successful calls retain their output and exit status.
The following corrections can affect scripts that relied on earlier behavior:

- Plain answers stopped at the output limit previously exited 0; they now
  exit 1 and withhold normal stdout, as structured answers already did.
  Other non-successful stops also exit 1. Terminal provider turns remain in
  the conversation. Inspect them with `ask replay` or request raw events with
  `-json` when partial output is useful.
- Streams missing completion previously could appear successful. Newly
  recorded failed streams are partial assistant turns and are not folded.
  Existing logged turns are not reclassified, preserving their request digests.
  Missing-completion responses are not automatically retried; transport and
  retryable HTTP failures retain their existing retry policy.
- Failed stdout writes now exit 1 instead of silently reporting success.
  The provider turn remains recorded; an output failure does not undo it.
- `compact` now refuses a source that fails replay verification before making
  a model call or creating files. There is no bypass for unsealed records.
- Generic size and output-token errors now exit 1, reserving exit 2 for
  identified context overflow. Ambiguous error messages also remain exit 1.
- Continuing a multi-block assistant turn through OpenRouter now sends every
  text block in order, separated by blank lines. Earlier versions sent only
  the last block. The stored conversation and its digest do not change.

## Contributing

Read [AGENTS.md](AGENTS.md) before changing the program. At minimum, run:

```sh
go test ./...
```

Run `go test -race ./...` after changing the log or a provider stream. Tests
also check command, flag, environment, provider, help, and man-page coverage.

Report security problems through [SECURITY.md](SECURITY.md), not a public
issue.

## License

[MIT](LICENSE).
