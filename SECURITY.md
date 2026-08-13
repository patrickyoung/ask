# Security

## Reporting

Report a vulnerability privately through GitHub's
[security advisory form](https://github.com/patrickyoung/ask/security/advisories/new).
Please do not open a public issue for anything exploitable.

## What ask holds

`ask` is a filter, not a service. It does not listen for incoming connections
or run code on your behalf. By default, its stored credentials and automatic
session files live under `~/.ask`:

| path | mode | contents |
| --- | --- | --- |
| `~/.ask/auth.json` | 0600 | OAuth tokens and refresh metadata (`ask login`) |
| `~/.ask/sessions/*.jsonl` | 0600 | messages, answers, attachments, and request metadata |

`ASK_AUTH_FILE` and `ASK_DIR` move those defaults; an explicit `-f` session
can live elsewhere. Directories created by `ask` use mode 0700.

A session log holds media attachment bytes, inlined text files, and the user
message formed from argv and stdin. Surrounding whitespace on textual stdin
is removed when that message is formed. The resulting session is
self-contained, so a session file deserves the same care as its inputs.
Copying one copies everything it means.

API keys are read from the environment and are never written to disk.
Gateway tokens (`ASK_AUTH_URL`) live in memory for the life of the process.

## Deliberate properties

- **Token endpoints must be https, except on loopback.** The request body
  carries a client secret or a refresh token — the long-lived half of the
  credential — and http would put it on the wire in the clear. A gateway
  inside a corporate network is still reached across a network. Loopback is
  the only exception, because those bytes never reach a wire; a hostname
  that merely *resolves* to 127.0.0.1 does not qualify. This is enforced on
  `ASK_AUTH_URL`, on `ask login -token-url`, and on a `token_url` read back
  from the credential file, and it is checked when the endpoint is
  configured rather than when a token expires.
- **Token endpoints never follow redirects.** Go re-sends the body on a 307
  or 308, so a redirect would hand the secret to another host. Stripping the
  `Authorization` header, which the standard library does, is no help when
  the secret is a form field. `ask` returns the redirect as an error instead.
- **Token requests are bounded in time.** Model streams are not; bound those
  yourself (see [GUIDE.md](GUIDE.md)).
- **A stored rotating refresh token is spent once.** Refreshes of credentials
  in the auth file happen under its lock, so parallel processes cannot spend
  the same stored token and race to save different replacements.
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
- **A token passed in argv is visible in `ps`** and in shell history.
  `ask login` says so, accepts `-` to read from stdin, and prefers
  `-from-codex` over both.

## Not in scope

Anything a language model writes is untrusted input. `ask` puts it on
stdout, and what happens next is the shell's business. Piping model output
into `eval` or `sh` executes untrusted text. Prompt injection through attached
or piped content is inherent to the task: if you feed it a hostile document,
the answer is downstream of a hostile document.
