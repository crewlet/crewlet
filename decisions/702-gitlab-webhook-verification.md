# d-702 — GitLab webhooks: the signature is mandatory

Status: **decided and implemented.** Supersedes an earlier version of this
document that decided the opposite on a false premise; the reversal is
recorded below rather than deleted, because the way it went wrong is the
useful part.
Implementation: `internal/gitlab/admin.go`, `internal/api/webhooks/verify.go`,
`internal/api/webhooks/routes.go` · Docs: `docs/integrations/gitlab.md`

## The decision

A GitLab delivery is authenticated by an HMAC signature over its body, and
by nothing else. A delivery without a valid `webhook-signature` is refused
with a 401. There is no fallback.

Hooks are provisioned with GitLab's **`signing_token`** attribute — a
`whsec_<standard-base64>` value over a 32-byte key, which is the only shape
the API accepts — never with `token`.

## What went wrong, and why it looked like a vendor limitation

The engine minted a correct signing key and registered it in the wrong
field. `hookBody` sent it as `token`.

GitLab takes two different secrets on a hook, and they are not two variants
of one idea:

| Attribute | What GitLab does with it |
|---|---|
| `signing_token` | Signs every delivery. Sends `webhook-id`, `webhook-timestamp`, `webhook-signature`. Never returned by the API; `signing_token_present` reports only whether one is set. |
| `token` | Echoes it back verbatim in the `X-Gitlab-Token` header. GitLab's own documentation calls this "not recommended" and "weaker". |

So the instance did exactly as it was asked: it did not sign, and it echoed
a 32-byte HMAC key back in cleartext on every delivery. The engine, which
verifies signatures, refused everything.

The capture that was taken at the time is the proof, and it was read
backwards. From a real `Issue Hook` on gitlab-ee 19.3.0:

```
webhook-id:            c32719e0-…
webhook-timestamp:     1787574263
x-gitlab-token:        whsec_probe
```

`webhook-id` and `webhook-timestamp` are sent on **every** delivery; only
`webhook-signature` is conditional on a signing token existing. And there,
in the header dump, is the signing key itself in plaintext — the single
clearest possible sign that it had been installed as a bearer token. It was
read as "GitLab sends the envelope but does not sign", and a fallback to the
plaintext token was added to make the integration work.

## Why the fallback had to go, beyond being unnecessary

It was reachable by **omitting a header**. The gate chose its scheme on
whether `webhook-signature` was present, so anyone who could reach the
endpoint could select the weaker check by simply not sending one. The
signature path was guarded against a bad signature falling through, which is
the attack that was anticipated; the one that mattered needed no signature
at all.

What it then verified was a bearer string that says nothing about the
payload it arrived with. Combined with the provisioning bug, that string was
also being broadcast in cleartext to the endpoint on every delivery.

## What the engine implements

Per <https://docs.gitlab.com/user/project/integrations/webhooks/>:

- The signed message is `{webhook-id}.{webhook-timestamp}.{body}`, over the
  **raw** received bytes.
- The key is the `whsec_` prefix stripped and the remainder **standard**
  base64-decoded. Not URL-safe, not raw/unpadded — the wrong alphabet
  usually still decodes to *something*, which is a mismatch with no message.
- `webhook-signature` may carry several space-separated `v1,<base64>`
  entries; GitLab sends one today and documents that this may change. Any
  entry matching is a match, which is how a key rotates without an outage.
- Comparison is constant-time.
- `webhook-timestamp` is checked against a tolerance window in **both**
  directions. A future stamp is as suspect as an old one: it is what a
  replay looks like against a node whose clock ran slow. The Standard
  Webhooks envelope exists partly for this, and a scheme that ignored the
  timestamp would be signature-checking a replayable message.

## What this costs

A GitLab older than **19.1** cannot sign: `signing_token` arrived in 19.0
behind a feature flag and went generally available in 19.1. Such an instance
cannot deliver to this engine at all, and that is the intended reading of
"mandatory" rather than an oversight. The alternative is accepting a
plaintext bearer token on a public endpoint, which is what this document
used to decide.

`crewlet gitlab provision` reports what it set, and the reconcile reads
`signing_token_present` back, so an operator finds out at provisioning time
rather than from silence.
