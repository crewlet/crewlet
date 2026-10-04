/**
 * What renaming a unit does, said before the rename is applied.
 *
 * A LITERAL CREDENTIAL CANNOT FOLLOW A RENAME. The configuration surface
 * never sends a literal credential: it sends the mask in its place, and a
 * write carrying the mask back is restored from the stored revision by the
 * identity of the entity holding it, a unit by its NAME (see
 * `config.RestoreRedacted`). A renamed unit has no stored twin to restore
 * from, so the engine refuses the save naming each masked field, and nothing
 * in the builder could fix that: no screen renders a credential, so none can
 * be typed back in. So the paths are named before the rename, with the one
 * way forward: move each literal into the secret store and reference it, which
 * a rename carries because a reference is a name, not a value.
 *
 * ONLY THE UNIT'S OWN FIELDS, as `model/document.ts` explains: a seat inside
 * restores by its handle and a child unit by its own name, and neither is
 * changed by this rename.
 */

import type { NodeKey } from "./model/keys.ts";
import { maskedCredentialPaths } from "./model/document.ts";

import { useBuilder } from "./BuilderContext.tsx";
import { ScreenLink } from "./dialogParts.tsx";
import { Callout, InlineCode } from "@crewlethq/ui";

export function RenameUnitPreflight({ unit, stored }: { unit: NodeKey; stored: boolean }) {
  const { state } = useBuilder();
  const paths = stored ? maskedCredentialPaths(state.draft, unit) : [];
  return (
    <div className="col gap-2">
      {stored && (
        <p className="t-caption">
          Renaming a unit re-keys what is attached to its name: agent seats in it and in its units
          onboard again, onboarding pages are looked up under the new name, and its schedules get a
          new identity, so a run due this minute may fire again.
        </p>
      )}
      {paths.length > 0 && (
        <Callout variant="warning">
          <div className="col gap-2">
            <span>
              These credentials are stored as literals. Move each one to the secret store and
              reference it as {"${NAME}"} before renaming, or the engine will refuse the save.
            </span>
            <ul className="builder-list">
              {paths.map((path) => (
                <li key={path}>
                  <InlineCode tone="inherit">{path}</InlineCode>
                </li>
              ))}
            </ul>
            <ScreenLink to="secrets" standalone>
              Open Secrets
            </ScreenLink>
          </div>
        </Callout>
      )}
    </div>
  );
}
