/**
 * How the dashboard addresses a page, as the engine's backlinks read it.
 *
 * A page's link is its ID in the dashboard's own address — `#/knowledge/pages/`
 * then the id — and `pages.Links` (`internal/pages/links.go`) is what finds
 * that address in a body to list the page as "linked from". The screen that
 * MINTS the address (the editor's "Link a page") and the grammar that READS it
 * back are two halves of one rule, held together by
 * `internal/pages.TestTheDashboardAddressesAPageTheWayTheBacklinksReadIt`: a
 * route moved here and not there would leave every link written afterwards
 * invisible to "linked from".
 */

/** The address of a page, before its id. */
export const PAGE_ADDRESS_PREFIX = "#/knowledge/pages/";
