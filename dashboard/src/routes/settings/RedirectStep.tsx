/**
 * A `redirect` requirement: the address a person registers at the vendor for
 * the engine's sign-in (`setup.KindRedirect`), copied whole, with its words.
 */

import { Callout, Copyable, FormField, InlineCode, Stack } from "@crewlethq/ui";
import { RedirectStale } from "~/components/RedirectMoved.tsx";
import type { SetupRequirement } from "~/protocol/index.ts";
import { VendorWords } from "~/ui/VendorWords.tsx";

/** `integrations` are the kinds signing in as this address's client, whose stale redirect is said under it. */
export function RedirectStep({
  r,
  integrations,
}: {
  r: SetupRequirement;
  integrations: readonly string[];
}) {
  const url = r.redirect?.url ?? "";
  const key = r.redirect?.key ?? "";
  return (
    <FormField
      as="fieldset"
      label={r.label}
      helper={
        // EVERY WORD SHOWN: what to do with the address is the step itself.
        <span className="int-help">
          <span>
            <VendorWords text={r.hint} />
          </span>
          {(r.more ?? []).length > 0 && (
            <span>
              <VendorWords text={r.more} />
            </span>
          )}
          {url && key && (
            <span>
              Built on <InlineCode close=".">{key}</InlineCode> When that address changes, update it
              in the client before the next sign-in.
            </span>
          )}
        </span>
      }
    >
      {url ? (
        <Stack gap={2}>
          <Copyable
            value={url}
            variant="block"
            monospace
            truncate={false}
            ariaLabel={`Copy ${r.label}`}
          />
          <RedirectStale vendor="google" names={integrations} />
        </Stack>
      ) : (
        <Callout variant="warning">
          <span>
            This engine has no public address, so no sign-in can come back to it. Set{" "}
            {key ? <InlineCode>{key}</InlineCode> : "its public address"} first.
          </span>
        </Callout>
      )}
    </FormField>
  );
}
