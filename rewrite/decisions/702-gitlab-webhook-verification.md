# d-702 — GitLab webhooks: accept the token scheme GitLab actually sends

Status: **decided, implemented — and it reverses a documented decision, so
it needs ratification.** Phase: 7 ·
Implementation: `internal/api/webhooks/verify.go`,
`internal/api/webhooks/routes.go` · Docs: `docs/integrations/gitlab.md`

## The measurement

Pointed a real GitLab at a running engine and created an issue. Every
delivery was refused. The engine logged one `webhook_signature_invalid` per
delivery; GitLab's own settings page showed a healthy, executable hook.

Captured a real `Issue Hook` delivery at a bare HTTP sink to see what GitLab
sends. On **gitlab-ee 19.3.0**, through a **project** hook, for a **real
event** (not the `/test` endpoint), with the hook's `token` set:

```
x-gitlab-event:        Issue Hook
x-gitlab-event-uuid:   243c86f4-…
x-gitlab-webhook-uuid: 85661e6d-…
x-gitlab-instance:     http://gitlab.local:8929
webhook-id:            c32719e0-…
webhook-timestamp:     1787574263
x-gitlab-token:        whsec_probe
```

**There is no `webhook-signature` header.** GitLab sends the
Standard-Webhooks *envelope* — `webhook-id` and `webhook-timestamp` — and
does not sign it.

The `webhook_standard_signature` feature flag is **off by default** on that
version (`Feature.get(:webhook_standard_signature).state → off`). Enabling it
and re-testing changed nothing: still no signature header, on a group hook
test delivery and on a real project-hook event.

## What the engine believed

`docs/integrations/gitlab.md` said, and the verifier enforced:

> inbound webhooks are verified by the GitLab 19.1+ Standard-Webhooks HMAC
> signature (`webhook-signature` header) and nothing else. The weaker plain
> `X-Gitlab-Token` scheme is intentionally unsupported; gitlab.com always
> runs ≥ 19.1 and the docker-compose test instance runs `gitlab-ee:latest`,
> so the signing token is always available.

The premise is false. "19.1+" is not the gate; emitting the signature at all
is, and a current GitLab does not.

The consequence is total: **the GitLab integration could not receive a single
webhook.** Not degraded — zero. And it fails in the shape that takes longest
to diagnose, because both ends report health: the hook is executable, the
engine is up, and the only evidence is a warning line per delivery.

## The decision

**Prefer the signature; accept the token when GitLab sends no signature.**

1. `webhook-signature` present → verify it exactly as before. Unchanged, and
   still the only scheme accepted when it is there: a delivery that carries a
   signature must have a *valid* one, so an attacker cannot strip it to reach
   the weaker path.
2. No `webhook-signature` → compare `X-Gitlab-Token` against the configured
   secret in constant time.
3. Neither, or no configured secret → refuse, as now.

## Why, and what it costs

The plain token is weaker in one specific way: it is a bearer value with no
timestamp, so a captured delivery can be replayed. It is not weaker in the
way "unsigned" suggests — the value never appears in a URL or a log, and the
comparison is constant-time.

Two things bound the replay:

- **The delivery ledger.** Every inbound delivery is deduped on its id, and
  GitLab sends `X-Gitlab-Event-UUID` on every request. A replayed delivery is
  dropped as a duplicate before it wakes anything.
- **Transport.** The token is only observable to something that can already
  read the request body, which is the payload itself.

Against that: the alternative is an integration that receives nothing. A
security property that is only available by not shipping the feature is not a
security property.

The engine says which scheme verified a delivery, once per epoch rather than
per request, so an operator on the weaker path knows it — and knows the
remedy is a GitLab that signs, not a config change here.

## What needs ratification

This reverses a decision that was written down and argued. The reasons to
look at it again:

- The original judgement is defensible and this is a real downgrade for
  anyone whose GitLab *does* sign — though for them nothing changes, because
  a present signature is still required to be valid.
- A stricter option exists: a config field that refuses the token scheme
  outright, for a deployment that has verified its instance signs. That is
  one field and one branch; it is not built, because nothing yet knows of a
  GitLab that signs, and a knob whose safe setting nobody can use is a knob
  that only ever gets set wrong.

If the answer is "keep requiring the signature", then the GitLab integration
is not in v1 and its doc needs the same status banner Slack and Confluence
carry — which is a bigger change than this one.
