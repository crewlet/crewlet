/**
 * The smoke topic's own Settings › Integrations: a screen of its own in place
 * of the shipped one, keeping the shipped module's other exports for the
 * screens that import them. The shipped screen's known bug is never drawn
 * here, so the route check does not expect it in this variant.
 */

import { Callout } from "@crewlethq/ui";

export * from "@original/routes/settings/Integrations.tsx";

export function Integrations({ kind }: { kind?: string }) {
  return (
    <Callout variant="info" title="Swapped by the smoke topic">
      {kind ? `The ${kind} integration` : "Every integration"} is drawn here by the topic's own
      screen instead of the shipped one.
    </Callout>
  );
}
