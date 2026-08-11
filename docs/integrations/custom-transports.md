# Custom Transports

You can add support for any external notification system by implementing the `Transport` protocol.

---

## Transport Protocol

Transports are **outbound-only** — they handle sending messages to external systems. Inbound notifications arrive via the API process, which publishes them to the EventQueue.

```python
class Transport(Protocol):
    name: str
    async def start(self) -> None: ...
    async def stop(self) -> None: ...
    async def send(self, message: OutboundMessage) -> bool: ...
```

---

## Example Implementation

```python
from crewlet.notifications.protocol import OutboundMessage, Transport

class CustomTransport:
    name: str = "custom"

    async def start(self) -> None:
        pass  # Initialize connections

    async def stop(self) -> None:
        pass  # Clean up

    async def send(self, message: OutboundMessage) -> bool:
        # Send the outbound message to your system
        return True
```

---

## Registration

Register your transport when constructing the engine:

```python
engine = Engine(
    organization=org,
    notification_transports=[CustomTransport()],
)
```

---

## Notification Service Architecture

The `NotificationService` is the bridge between external tools and agents. It runs inside the Engine process, consuming/producing via the EventQueue.

### Handle-Based Identity

Every agent has a deterministic **handle** derived from its role name (e.g., `"Sarah Chen"` → `"sarah-chen"`). The `HandleRegistry` provides the central identity mapping:

- `resolve_handle("engineer")` → `AgentInstance`
- `resolve_email_address("notif+engineer@co.com")` → `AgentInstance`
- `resolve_external_id("slack", "U_BOT_123")` → `AgentInstance`

See [Organization Model](../concepts/organization-model.md) for handle details.

### Inbound / Outbound Flow

```
Inbound:
  Webhook ──> API ──> EventQueue (crewlet.notifications.inbound)
                        │
                        ▼
                NotificationService
                ├── resolve recipient via HandleRegistry
                └── publish to crewlet.agent.{handle}.inbox
                        │
                        ▼
                Agent handler fires

Outbound:
  Agent ──> EventQueue (crewlet.notifications.outbound)
              │
              ▼
      NotificationService
      ├── resolve transport
      └── transport.send()
              │
              ▼
      Your Custom System
```

For inbound webhooks, you'll also need to add a webhook route to the API process that parses incoming payloads and publishes them to `crewlet.notifications.inbound`.
