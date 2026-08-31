/**
 * Engine health, as a panel rather than a coloured dot.
 *
 * The dot was the only health surface in the product and it could show three
 * colours; everything behind it — whether a company config is even active,
 * which node this is, what epoch it has applied, how many turns are in flight,
 * whether the event store is durable — was on the wire and reached no screen.
 *
 * It also read seven fields that exist on NO server type. The 5-second push
 * carries `{status, in_flight, shutting_down}` and nothing else; the rest comes
 * from the `stream` query, which answers the full `api.Health`. Reading one off
 * the other is how an engine with no active configuration — dropping every
 * inbound webhook — came to render identically to a healthy idle one.
 */

import { Badge, Button, KeyValue } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";

export function EnginePanel({
  onClose,
  onSetToken,
}: {
  onClose: () => void;
  onSetToken: () => void;
}) {
  const { connected, authRejected, health } = useConnection();
  const now = useNow();
  // The `stream` query is deliberately not named `health`: a query name must
  // never collide with a push kind. It is polled at the same 5 s cadence the
  // push ticks at, so the two halves of this panel never disagree by more than
  // one interval.
  const { data: engine, error } = useQuery("stream", undefined, { pollMs: 5000 });

  return (
    <div className="dialog" style={{ width: "min(440px, 100%)" }}>
      <header className="dialog-head">
        <Icon name="power" size="sm" />
        <strong style={{ fontSize: "var(--fs-sm)" }}>Engine</strong>
        <span className="spacer" />
        <Button icon="x" variant="ghost" size="sm" onClick={onClose} title="Close" />
      </header>
      <div className="dialog-body col gap-3">
        <div className="row">
          <Badge tone={connected ? "positive" : authRejected ? "critical" : "caution"} dot>
            {connected ? "connected" : authRejected ? "refused" : "unreachable"}
          </Badge>
          {health.shutting_down && <Badge tone="caution">draining</Badge>}
          {engine?.configured === false && <Badge tone="critical">no active config</Badge>}
          {engine?.posture && engine.posture !== "serve" && (
            <Badge tone="caution">posture: {engine.posture}</Badge>
          )}
        </div>

        {authRejected && (
          <div className="banner critical">
            <Icon name="key" size="sm" />
            <span style={{ flex: 1 }}>
              This browser's API token was refused. Reads and writes are both blocked.
            </span>
            <Button size="sm" onClick={onSetToken}>
              Set token
            </Button>
          </div>
        )}
        {!connected && !authRejected && (
          <div className="banner caution">
            <Icon name="refresh" size="sm" />
            <span>
              Reconnecting. The page is showing the last state it received and polling the REST
              snapshot meanwhile.
            </span>
          </div>
        )}
        {error && connected && (
          <div className="banner neutral">
            <Icon name="info" size="sm" />
            <span>
              The engine is reachable but did not answer the health query ({error}). The fields
              below may be stale.
            </span>
          </div>
        )}

        <KeyValue
          items={[
            ["Status", engine?.status ?? health.status ?? "unknown"],
            [
              "Node",
              <code key="n" className="inline">
                {engine?.node || "—"}
              </code>,
            ],
            ["Version", engine?.version || "—"],
            [
              "Company config",
              engine?.configured === false ? (
                <span style={{ color: "var(--critical-ink)" }}>
                  none active — every inbound webhook is dropped
                </span>
              ) : (
                "active"
              ),
            ],
            [
              "Applied epoch",
              <code key="e" className="inline">
                {engine?.applied_epoch || "—"}
              </code>,
            ],
            ["Control-plane posture", engine?.posture || "—"],
            [
              "Turns in flight",
              <span key="f" className="t-num">
                {health.in_flight ?? engine?.in_flight ?? 0}
              </span>,
            ],
            ["Seats held here", engine?.seats?.length ?? "—"],
            ["Stream", engine?.queue || "—"],
            ["Dashboard clients", engine?.clients ?? "—"],
            [
              "Engine up since",
              engine?.engine_started_at
                ? `${fmtDateTime(engine.engine_started_at)} (${relTime(engine.engine_started_at, now)})`
                : "—",
            ],
            [
              "Process up since",
              engine?.started_at
                ? `${fmtDateTime(engine.started_at)} (${relTime(engine.started_at, now)})`
                : "—",
            ],
          ]}
        />
      </div>
    </div>
  );
}
