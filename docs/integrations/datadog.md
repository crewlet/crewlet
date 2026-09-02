# Datadog

Datadog reaches Crewlet through its **Webhooks integration**, which posts a monitor's payload to a URL you configure. A firing monitor becomes an inbound event on the same path as everything else, so a seat can be woken by an alert exactly as it is by a comment on a merge request.

## Configuration

```yaml
integrations:
  datadog:
    enabled: true
    webhook_token: "${DATADOG_WEBHOOK_TOKEN}"
```

- **`webhook_token` is required** when enabled. It is compared against the `X-Crewlet-Token` header on every delivery, and a route with nothing to check against answers **503** rather than accepting one.

## Verification is weaker here, and that is the provider's ceiling

Every other inbound route verifies an HMAC over the request body. Datadog cannot do that: its webhook attaches custom headers, but only with **fixed values**, so there is nothing varying with the payload to sign.

The strongest check available is therefore a constant-time comparison of a shared token. The difference is real and worth stating rather than glossing:

- a replayed delivery is indistinguishable from a fresh one
- anyone holding the token can forge an alert

Treat `webhook_token` as a signing key — it is doing that job with none of the guarantees. Rotate it the same way, and keep it a `${VAR}` rather than a literal.

## Setting it up in Datadog

1. Open **Integrations → Webhooks** and add a webhook.
2. Set the URL to `https://<your-engine>/webhooks/datadog`.
3. Under **Headers**, add `X-Crewlet-Token` with your token's value.
4. Reference it from a monitor's notification message with `@webhook-<name>`.

Datadog posts an empty body unless the webhook defines a payload template, so give it one that carries at least `id`, `title`, `alert_transition` and `priority`.

## Deduplication

Datadog stamps each notification with an `id` that is stable across its own retries, and the route claims on it, so a retried alert is answered as a duplicate rather than waking a seat twice.

A payload carrying no `id` is processed **without** a claim. There is nothing stable to key on, and delivering a firing monitor twice is better than dropping it.
