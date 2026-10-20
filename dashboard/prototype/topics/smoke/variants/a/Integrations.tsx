/**
 * The smoke topic's wrapper of Settings › Integrations: the shipped screen
 * under a callout. The shipped screen's known bug is still drawn here, by the
 * shipped module, so the route check excuses it as it does on Today.
 */

import { Callout } from "@crewlethq/ui";
import { Integrations as Shipped } from "@original/routes/settings/Integrations.tsx";

export * from "@original/routes/settings/Integrations.tsx";

export function Integrations(props: { kind?: string }) {
  return (
    <div className="smoke-swapped">
      <Callout variant="info" title="Swapped by the smoke topic">
        This screen is the shipped Integrations screen, wrapped by a replacement module.
      </Callout>
      <Shipped {...props} />
    </div>
  );
}
