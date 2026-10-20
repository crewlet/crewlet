/**
 * The smoke topic's swap of the whole sidebar: a rail holding the brand mark
 * and nothing else, written WITHOUT importing the shipped sidebar. That
 * sidebar is the one module that spells out the engine's own base for the
 * mark, so in this variant the literal lives only in a replacement, which the
 * build rewrites as it rewrites the original.
 */

import { AppShell, BrandLockup } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";

export function Sidebar(_props: { onSearch: () => void; onSetToken: () => void }) {
  return (
    <AppShell.Rail
      header={
        <BrandLockup
          name="Smoke"
          href={href(["home"])}
          mark={<img src="/static/dashboard/crewlet-icon.svg" alt="" />}
        />
      }
    />
  );
}
