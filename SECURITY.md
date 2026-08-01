# Security

## Reporting

Report a vulnerability privately through GitHub's
[security advisory form](https://github.com/patrickyoung/ask/security/advisories/new).
Please do not open a public issue for anything exploitable. Expect an
acknowledgement within a few days.

## What ask holds

`ask` is a filter, not a service. It listens on nothing, opens no ports, and
runs no code on your behalf. What it does hold is credentials and
conversations, and both live in `~/.ask`:

| path | mode | contents |
| --- | --- | --- |
| `~/.ask/auth.json` | 0600 | OAuth access and refresh tokens (`ask login`) |
| `~/.ask/sessions/*.jsonl` | 0600 | every message, answer, and attachment, verbatim |

Both directories are 0700.

A session log holds the bytes of anything you attached and the whole text of
anything you piped in. That is what makes it replay exactly, and it means a
session file deserves the same care as the material that went into it.
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
- **A rotating refresh token is spent once.** Refreshes happen under a lock
  on the credential file, so a parallel fan-out cannot have eight processes
  each present the same token and the last one store a version the issuer
  has already retired — which would log you out of your own account.
- **Attachments are typed by content, never by name.** A `.png` holding a
  shell script is a shell script. Filenames handed to the model are reduced
  to a base name with control characters stripped, so a file cannot name
  itself into the prompt.
- **Nothing is decoded.** No image dimensions, no PDF structure, no audio
  duration — `ask` matches a signature and passes the bytes through. There
  are no media parsers, and so no media parser bugs.
- **One writer per session,** enforced by a lock, because two processes
  appending to one log would interleave two conversations and break every
  digest after the first.
- **A token passed in argv is visible in `ps`** and in shell history.
  `ask login` says so, accepts `-` to read from stdin, and prefers
  `-from-codex` over both.

## Not in scope

Anything a language model writes is untrusted input. `ask` puts it on
stdout, and what happens next is the shell's business — piping model output
into `eval`, `sh`, or `jq -e` is your decision, not ask's. Prompt injection
through attached or piped content is inherent to the task: if you feed it a
hostile document, the answer is downstream of a hostile document.
