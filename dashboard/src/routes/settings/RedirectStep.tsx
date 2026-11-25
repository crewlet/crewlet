/**
 * A `redirect` requirement: the address a person registers at the vendor for
 * the engine's sign-in (`setup.KindRedirect`), copied whole, with its words.
 */

import { Callout, Copyable, FormField, InlineCode } from "@crewlethq/ui";
import type { SetupRequirement } from "~/protocol/index.ts";
import { VendorWords } from "~/ui/VendorWords.tsx";

export function RedirectStep({ r }: { r: SetupRequirement }) {
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
        <Copyable
          value={url}
          variant="block"
          monospace
          truncate={false}
          ariaLabel={`Copy ${r.label}`}
        />
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
