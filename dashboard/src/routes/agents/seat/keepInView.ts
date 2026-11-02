/**
 * Scroll `row` into its list's own view, moving nothing outside the list.
 *
 * NOT `scrollIntoView`, which scrolls every scrollable ancestor to reach the
 * row — the page included, so reopening a profile on a thread near the list's
 * end would open the page there. Shared by the lists a seat's Memory tab
 * chooses from (its threads and its sessions), whose chosen row stays in view
 * as the address names it.
 *
 * A MODULE OF THE MEMORY TAB'S OWN, not of `~/lib/scroller.ts`: the shell
 * imports that one, so whatever it holds is in the initial load every reader
 * downloads before the first screen draws, and this serves two lists of one
 * lazily loaded tab.
 */
export function keepInView(box: HTMLElement, row: HTMLElement): void {
  const top = row.getBoundingClientRect().top - box.getBoundingClientRect().top + box.scrollTop;
  const bottom = top + row.offsetHeight;
  if (top < box.scrollTop) box.scrollTop = top;
  else if (bottom > box.scrollTop + box.clientHeight) box.scrollTop = bottom - box.clientHeight;
}
