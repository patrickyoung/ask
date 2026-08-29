# Security

## Reporting

Report a vulnerability privately through GitHub's
[security advisory form](https://github.com/patrickyoung/ask/security/advisories/new).
Please do not open a public issue for anything exploitable.

## What ask holds

`ask` is a filter, not a service. It does not listen for incoming connections,
run code on your behalf, or store OAuth credentials. Its automatic session
files live under `~/.ask`:

| path | mode | contents |
| --- | --- | --- |
| `~/.ask/sessions/*.jsonl` | 0600 | messages, answers, attachments, and request metadata |

`ASK_DIR` moves that default; an explicit `-f` session can live elsewhere.
Directories created by `ask` use mode 0700.

A session log holds media attachment bytes, inlined text files, and the user
message formed from argv and stdin. Surrounding whitespace on textual stdin
is removed when that message is formed. The resulting session is
self-contained, so a session file deserves the same care as its inputs.
Copying one copies everything it means.

API keys are read from the environment and are never written to disk. OAuth
login, refresh, rotation, resource binding, and storage belong to the separate
`oauth` filter. `oauth with` transfers one Authorization header to Ask on an
inherited descriptor.

## Deliberate properties

- **OAuth credentials enter only through a descriptor.** `-header-fd` accepts
  one bounded HTTP Authorization header. Ask does not accept it in argv, the
  environment, stdin, or ordinary output, and it never writes it to a session.
- **An inherited Authorization header is origin-bound.** The first provider
  request fixes its origin. A redirect to another origin is refused before
  the transport attaches the credential.
- **Attachments are typed by content, never by name.** A `.png` holding a
  shell script is a shell script. Filenames handed to the model are reduced
  to a base name with control characters stripped, so a file cannot name
  itself into the prompt.
- **Media is not decoded.** No image dimensions, no PDF structure, no audio
  duration — `ask` matches a signature and passes the bytes through. There
  are no media parsers, and so no media parser bugs.
- **JSON Schemas cannot fetch other documents.** Local fragment references
  work, but the schema compiler has no external URL loader. This prevents a
  schema from turning validation into a network or filesystem read.
- **One writer per session,** enforced by a lock, because two processes
  appending to one log would interleave two conversations and break every
  digest after the first.

## Not in scope

Anything a language model writes is untrusted input. `ask` puts it on
stdout, and what happens next is the shell's business. Piping model output
into `eval` or `sh` executes untrusted text. Prompt injection through attached
or piped content is inherent to the task: if you feed it a hostile document,
the answer is downstream of a hostile document.
