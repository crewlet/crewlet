# ADR-0153: A repository's bytes are served inert on the dashboard's origin

- **Status:** accepted
- **Authority:** `internal/api/pagepolicy`
- **Enforced-by:**
  - `internal/gitlet/host.TestADownloadIsServedInert`
  - `internal/api/pagepolicy.TestTheInertPolicyRunsNothing`
  - `internal/api/auth.TestADownloadIsNeverChallenged`
- **Tag-status:** unreleased

## The decision

**A file's bytes and a commit's ZIP are served on the dashboard's own origin,
inert.** Gitlet answers two downloads beside git's smart HTTP,
`GET /git/{repo}/-/raw` and `GET /git/{repo}/-/archive.zip`, to the person or
token asking, with the session cookie, a bearer or a Basic token. Their bytes
are somebody's content on the origin that holds every person's session, so
every answer is written through `pagepolicy.Download`:

- **Two types for a file, never one read from its name.** A text file (no NUL
  in its first 8 000 bytes, git's own rule, `codeindex.Binary`) not asked to
  download is `text/plain; charset=utf-8`, inline; every other file is
  `application/octet-stream` as an attachment, and a ZIP is
  `application/zip` as one. `mime.TypeByExtension` is never asked.
- **`X-Content-Type-Options: nosniff`**, so no browser makes a document,
  script, style or worker of a type it was not given.
- **`pagepolicy.Inert`**: `default-src 'none'; base-uri 'none'; form-action
  'none'; frame-ancestors 'none'; sandbox`, with no `allow-` token, so a
  document made of the bytes anyway runs nothing, loads nothing and is framed
  nowhere; `X-Frame-Options: DENY` beside it.
- **`Cross-Origin-Resource-Policy: same-origin`**, so no other site loads the
  bytes as a subresource, and **`Cache-Control: private, no-store`**, so no
  shared cache keeps a read that names no reader.
- **The filename through `mime.FormatMediaType`**, the one header carrying a
  name somebody chose: quotes escaped, every byte outside printable ASCII
  encoded, so no line break reaches a header.

**A download is never challenged for Basic.** A request presenting no
credential, or a refused one, is `401` in one line with no `WWW-Authenticate`
(`auth.DownloadUnauthorized`): a browser answers a Basic challenge with its own
password prompt and keeps what is typed into it. Basic still works sent with
the request, as `curl -u :$TOKEN` sends it.

## Why the obvious alternatives are wrong

**Serving a file with its extension's type.** Any writer of a repository, a
coding box among them, can push an `.html` or `.svg` that would then run as
the reader, on the origin that holds their session, and a `.js` that loads as a
same-origin script, which the dashboard's `script-src 'self'` admits.

**A separate origin for repository bytes.** A listener, a hostname and a
certificate every deployment must route and maintain, and a ticket per view to
carry the reader across, for bytes that never need to run.

**A ticketed URL.** A credential in a URL is written into every proxy's and
browser's history, which is why the guard reads none there
(`auth.Guard.Credential`).

## What this does not decide

How the dashboard shows a file or offers a download (decided where the
dashboard's code views are built), and the artifact origin's policies, which
keep their own rules; `pagepolicy.ArtifactInert` is the same value as `Inert`,
spelled once.
