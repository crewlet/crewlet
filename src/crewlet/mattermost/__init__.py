"""Mattermost integration — the self-hosted, open-source chat backend.

Two halves, mirroring how GitLab and Plane are laid out:

- **Here**: the REST client, the websocket event fleet, and the
  ``crewlet mattermost provision`` seat provisioner.
- **Engine-side** (``config`` / ``notifications``): ``MattermostConfig``,
  the ``MattermostTransport``, its notification prompt, and identity
  registration.

The structural difference from every other inbound integration is that
Mattermost has no usable webhook: its outgoing webhooks fire only in
public channels and carry neither thread id nor channel type, so inbound
events arrive over an authenticated **websocket per agent seat** instead
of an HTTP callback.  See ``docs/integrations/mattermost.md``.
"""

from crewlet.mattermost.client import MattermostClient, MattermostError

__all__ = ["MattermostClient", "MattermostError"]
