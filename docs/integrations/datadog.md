# Datadog

Datadog reaches Crewlet through its **Webhooks integration**, which posts a monitor's payload to a URL you configure. Alerts become inbound events on the same path every other integration uses, so an agent can be woken by a monitor transitioning to alert the same way it is woken by a comment on a merge request.

## Configuration

```yaml
integrations:
  datadog:
    enabled: true
    webhook_secret: "${DATADOG_WEBHOOK_SECRET}"
```

`webhook_secret` is **required** when Datadog is enabled. With nothing to check against, the route would accept anything, so an unset secret makes `POST /webhooks/datadog` answer 503 rather than serving.

## Verification is weaker here, and that is the provider's ceiling

Every other inbound route in Crewlet verifies an HMAC over the request body. Datadog cannot do that: its webhook can attach custom headers, but only with **fixed values**, so there is nothing that varies with the body to sign.

The strongest check available is therefore a constant-time comparison of a shared token, sent as `X-Crewlet-Token`. The practical difference is real and worth stating plainly:

- a replayed delivery is indistinguishable from a fresh one
- anyone holding the token can forge an alert

Treat the token as a secret on the same terms as a signing key, and rotate it the same way. This is not a simplification Crewlet chose; it is what the provider offers.

## Setting it up in Datadog

1. Open **Integrations → Webhooks** and add a webhook.
2. Set the URL to `https://<your-engine>/webhooks/datadog`.
3. Under **Headers**, add `X-Crewlet-Token` with the value of your `webhook_secret`.
4. Reference the webhook from a monitor's notification message with `@webhook-<name>`.

## Deduplication

Datadog stamps each notification with an id that is stable across its own retries, and the route claims on it, so a retried alert is answered as a duplicate rather than waking an agent twice. A payload carrying no id is processed without a dedupe claim — there is nothing stable to key on, and dropping it would be worse than delivering it twice.
