---
paths:
  - "internal/{queue,coord,objstore,jsapi,jsinflight,jsprovision}/**"
  - "internal/engine/fleetbroker.go"
  - "internal/config/bootstrap.go"
  - ".claude/worktrees/*/internal/{queue,coord,objstore,jsapi,jsinflight,jsprovision}/**"
  - ".claude/worktrees/*/internal/engine/fleetbroker.go"
  - ".claude/worktrees/*/internal/config/bootstrap.go"
---

# The Broker
- **NATS JetStream** (`github.com/nats-io/nats-server`) is embedded by default: in the engine's process, with no listener. A fleet clusters the embedded members (`stream.cluster`) or dials an external NATS cluster (`stream.type: nats`). A node without the `data` role joins the members' broker as a LEAF, JetStream off.
- Every route, leaf link and client connection to an embedded broker presents a key derived from `secrets.keys` (ADR-0094). A member's client port, on loopback, admits only the operator's key.
- The coordination KV lives on the stream's own broker on every topology: over the queue's connection on an external broker, over a second watched connection on the embedded one (ADR-0001). If NATS closes either connection for good, the node stops.
