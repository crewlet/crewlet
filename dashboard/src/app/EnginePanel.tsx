/**
 * Engine health, as a panel rather than a coloured dot.
 *
 * The dot was the only health surface in the product and it could show three
 * colours; everything behind it (whether a company config is even active,
 * which node this is, what epoch it has applied, how many turns are in flight,
 * whether the event store is durable) was on the wire and reached no screen.
 *
 * It also read seven fields that exist on NO server type. The 5-second push
 * carries `{status, in_flight, shutting_down}` and nothing else; the rest comes
 * from the `stream` query, which answers the full `api.Health`. Reading one off
 * the other is how an engine with no active configuration, dropping every
 * inbound webhook, came to render identically to a healthy idle one.
 *
 * It is a modal on the layer stack (`useModal`), owning its veil the way the
 * shared `Dialog` does. The shell used to wrap it in a veil of its own and
 * close it from a window-level Escape listener that ran beside the stack, so
 * the panel trapped no Tab and returned focus nowhere.
 */

import { useRef } from "react";
import { useConnection, useEngineHealth } from "~/lib/store-hooks.ts";
import { fmtDateTime } from "~/lib/format.ts";
import {
  Button,
  Callout,
  DescriptionList,
  EmptyValue,
  InlineCode,
  Inline,
  Modal,
  RelativeTime,
  Tag,
  Text,
  useNow,
} from "@crewlethq/ui";
import { KeyGlyph, PowerSettingsNewGlyph } from "@crewlethq/icons/glyphs";

export function EnginePanel({
  onClose,
  onSetToken,
}: {
  onClose: () => void;
  onSetToken: () => void;
}) {
  const { connected, authRejected, health } = useConnection();
  const now = useNow();
  // ONE read, shared with the rail: `useEngineHealth` polls at the cadence the
  // push itself ticks at, so the two halves of this panel never disagree by
  // more than one interval, and neither does the pill that opened it.
  const { data: engine, error } = useEngineHealth();

  // WHERE FOCUS STARTS. The first control is Close, and a panel that opens
  // on Close has made leaving the first thing it offers. The one action in
  // the body (Set token, when the token was refused) is what the reader came
  // for; without it the panel itself takes focus, so its name is announced
  // and the readout that follows is read in order.
  return (
    <Modal
      open
      stackBody
      title="Engine"
      icon={<PowerSettingsNewGlyph />}
      size="sm"
      onClose={onClose}
    >
      <Inline gap={2} wrap>
        <Tag variant={connected ? "success" : authRejected ? "danger" : "warning"} dot>
          {connected ? "connected" : authRejected ? "refused" : "unreachable"}
        </Tag>
        {health.shutting_down && <Tag variant="warning">draining</Tag>}
        {engine?.configured === false && <Tag variant="danger">no active config</Tag>}
        {engine?.posture && engine.posture !== "serve" && (
          <Tag variant="warning">posture: {engine.posture}</Tag>
        )}
      </Inline>

      {authRejected && (
        <Callout
          variant="danger"
          icon={<KeyGlyph />}
          action={
            <Button variant="secondary" size="small" onClick={onSetToken}>
              Set token
            </Button>
          }
        >
          This browser&apos;s API token was refused. Reads and writes are both blocked.
        </Callout>
      )}
      {!connected && !authRejected && (
        <Callout variant="warning">
          Reconnecting. The page is showing the last state it received and polling the REST snapshot
          meanwhile.
        </Callout>
      )}
      {error && connected && (
        <Callout variant="info">
          The engine is reachable but did not answer the health query ({error}). The fields below
          may be stale.
        </Callout>
      )}

      <DescriptionList
        items={[
          ["Status", engine?.status ?? health.status ?? "unknown"],
          [
            "Node",
            <InlineCode key="n">{engine?.node || <EmptyValue label="Not reported" />}</InlineCode>,
          ],
          ["Version", engine?.version || <EmptyValue label="Not reported" />],
          [
            "Company config",
            engine?.configured === false ? (
              // A TAG rather than red words: the state is what carries the
              // colour in this design system, and a paragraph tinted with a
              // feedback hue is colour spent on something a reader cannot
              // press, compare or filter by.
              <Tag key="c" variant="danger">
                none active, so every inbound webhook is dropped
              </Tag>
            ) : (
              "active"
            ),
          ],
          [
            "Applied epoch",
            <InlineCode key="e">
              {engine?.applied_epoch || <EmptyValue label="Not reported" />}
            </InlineCode>,
          ],
          ["Control-plane posture", engine?.posture || <EmptyValue label="Not reported" />],
          [
            "Turns in flight",
            <Text key="f" numeric>
              {health.in_flight ?? engine?.in_flight ?? 0}
            </Text>,
          ],
          ["Seats held here", engine?.seats?.length ?? <EmptyValue label="Not reported" />],
          ["Stream", engine?.queue || <EmptyValue label="Not reported" />],
          ["Dashboard clients", engine?.clients ?? <EmptyValue label="Not reported" />],
          [
            "Engine up since",
            engine?.engine_started_at ? (
              <>
                {fmtDateTime(engine.engine_started_at)} (
                <RelativeTime value={engine.engine_started_at} now={now} />)
              </>
            ) : (
              <EmptyValue label="Not reported" />
            ),
          ],
          [
            "Process up since",
            engine?.started_at ? (
              <>
                {fmtDateTime(engine.started_at)} (
                <RelativeTime value={engine.started_at} now={now} />)
              </>
            ) : (
              <EmptyValue label="Not reported" />
            ),
          ],
        ]}
      />
    </Modal>
  );
}
