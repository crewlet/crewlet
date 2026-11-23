/**
 * Words a person follows at a vendor (`internal/vendortext`), held to the
 * engine's `Part` by `internal/api/setupapi.TestTheSetupFormReadsWhatTheSetupWireSends`.
 */

/** Plain words, or a name on the vendor's screen (several a menu path), either linked to a vendor page. */
export interface VendorTextPart {
  text?: string;
  ui?: string[];
  url?: string;
}
