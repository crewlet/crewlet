/**
 * Copy this page's whole address, and say what the browser did with it.
 *
 * THE WHOLE URL, not the hash: what a reader pastes into chat has to open on
 * somebody else's machine, and a bare `#/worklet/ENG-42` opens nothing.
 *
 * The clipboard is refused outright on an insecure origin and by permission
 * policy, and a command that silently did nothing would read as a broken one
 * rather than as a browser that said no. BOTH REFUSALS ARE SAID: permission
 * policy rejects the write, but an insecure origin (this engine serves plain
 * HTTP on `api.port` unless something terminates TLS in front of it) has no
 * `navigator.clipboard` at all, and an optional call on it skipped the whole
 * chain, so the ordinary deployment was exactly the one where the command did
 * nothing and said nothing.
 */
export async function copyPageLink(): Promise<"Copied" | "Blocked"> {
  return copyLink(location.href);
}

/**
 * Copy the whole address of a page in this dashboard, given its href (a
 * `#/…` hash, as `app/router.tsx`'s `href` builds one, or a whole URL): a
 * room's own address from a menu, which is not the address the reader is on
 * when a pane is open beside it. Resolved against this page's URL, so what
 * is copied opens on somebody else's machine.
 */
export async function copyLink(address: string): Promise<"Copied" | "Blocked"> {
  if (!navigator.clipboard) return "Blocked";
  try {
    await navigator.clipboard.writeText(new URL(address, location.href).href);
    return "Copied";
  } catch {
    return "Blocked";
  }
}
