/** The engine's vendor text, drawn through the kit: field help, Setup steps, Have ready and identity sheets. */

import { VendorText as KitVendorText } from "@crewlethq/ui";
import type { VendorTextPart } from "~/contract/vendortext.ts";
import { marked } from "./Problems.tsx";

/**
 * Vendor text with each vendor UI name and page link drawn by the kit, and
 * values in its words marked ([marked]). `fill` fills a `{field}` in the
 * words; `resolveUrl` fills one in a link, which waits ("") until it can.
 */
export function VendorWords({
  text,
  fill,
  resolveUrl,
}: {
  text: VendorTextPart[] | undefined;
  fill?: ((words: string) => string) | undefined;
  resolveUrl?: ((url: string) => string) | undefined;
}) {
  return (
    <KitVendorText
      parts={text ?? []}
      renderWords={(words) => marked(fill ? fill(words) : words)}
      resolveUrl={resolveUrl}
    />
  );
}
