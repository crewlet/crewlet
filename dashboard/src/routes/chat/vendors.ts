/**
 * Where a company's chat is when it is not the engine's own.
 *
 * ONE SPELLING FOR TWO PLACES: the chat screen's own answer to a company on a
 * vendor's chat (`Chat.tsx`'s `ChatElsewhere`) and the line its Home widgets
 * draw in place of rooms (`home.widgets.tsx`). Two copies of the names would
 * call one surface two things.
 */

/** A vendor surface's name as a person reads it. */
const VENDOR_NAMES: Record<string, string> = { mattermost: "Mattermost", slack: "Slack" };

/**
 * The vendor surfaces the `viewer` answer names, as one phrase: "Slack",
 * "Mattermost and Slack", or "a vendor's chat" where the answer names none.
 */
export function vendorChatName(vendors: readonly string[]): string {
  const names = vendors.map((vendor) => VENDOR_NAMES[vendor] ?? vendor);
  return names.length ? names.join(" and ") : "a vendor's chat";
}
