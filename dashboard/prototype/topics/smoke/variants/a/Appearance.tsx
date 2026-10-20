/**
 * The smoke topic's swap: Settings › Appearance as it ships, under a callout
 * saying it was swapped, so a build that swapped nothing is visible at once.
 */

import { Callout } from "@crewlethq/ui";
import { Appearance as Shipped } from "@original/routes/settings/Appearance.tsx";

export function Appearance() {
  return (
    <div className="smoke-swapped">
      <Callout variant="info" title="Swapped by the smoke topic">
        This screen is the shipped Appearance screen, wrapped by a replacement module.
      </Callout>
      <Shipped />
    </div>
  );
}
