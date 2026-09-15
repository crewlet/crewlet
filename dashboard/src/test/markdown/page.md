# Migration off Pulsar

The broker estate is **one NATS cluster** now, and the coordination KV rides
its own connection. The *second* estate every Pulsar deployment ran is gone.

## What changed

1. `stream.type: pulsar` is retired
2. `coordination.nats` is retired
3. The KV rides the stream's connection on every topology

- [x] Drop the Pulsar client
- [x] Move the KV onto the stream connection
- [ ] Delete the compose service
  - it still holds a volume
  - and the volume holds a topic nobody reads

> Pulsar has no compare-and-set, which is the whole reason every deployment
> ran a second NATS estate anyway.

| Backend | CAS | Ran a second estate |
|---|:---:|---:|
| Pulsar | no | always |
| NATS | yes | never |

See [the deployment guide](https://docs.crewlet.ai/guides/deployment) and
[ENG-214](#/work/ENG-214). Contact <mailto:ops@example.com> if a node refuses
to start.

```go
if err := q.Publish(ctx, topics.NotificationsInbound, ev); err != nil {
    return fmt.Errorf("publish: %w", err)
}
```

---

A line ending in two spaces breaks hard,  
like this.
