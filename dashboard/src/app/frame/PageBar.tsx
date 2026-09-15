/**
 * The page bar: where you are, and what you can do about it.
 *
 * Sticky, 52 px, on every screen. It replaces a topbar that carried a single
 * `<h1>` derived from the route's first segment — so an item page said "Work",
 * a sprint said "Work", and a page nested four levels deep said "Knowledge".
 *
 * # The breadcrumb is the address, not a title
 *
 * Every segment but the last is a link, and the last is the object itself.
 * That is the whole rule: a trail whose final segment is a link to the page
 * you are already on teaches a reader that the control does nothing.
 *
 * # Actions report
 *
 * Copy link, star, open. Each says what it did — a control with no outcome is
 * indistinguishable from one that failed.
 */

import { useCallback, useState, type ReactNode } from "react";
import { href } from "../router.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { Button, cx } from "~/ui/primitives.tsx";
import { PAGE_ACTIONS_SLOT } from "./PageActions.tsx";
import { MaxStars, useStarred, useToggleStar, type Star } from "~/lib/starred.ts";

export interface Crumb {
  label: string;
  /** Absent on the last segment: the object is not a link to itself. */
  path?: string[];
  query?: Record<string, string>;
  /** Rendered in the mono face — a key, a handle, an id. */
  mono?: boolean;
}

export function Breadcrumb({ crumbs }: { crumbs: Crumb[] }) {
  return (
    <nav className="crumbs" aria-label="Breadcrumb">
      {crumbs.map((crumb, i) => {
        const last = i === crumbs.length - 1;
        return (
          <span key={`${crumb.label}-${i}`} className="crumb-part">
            {i > 0 && (
              <span className="crumb-sep" aria-hidden="true">
                /
              </span>
            )}
            {crumb.path && !last ? (
              <a
                className={cx("crumb-link", crumb.mono && "mono")}
                href={href(crumb.path, crumb.query)}
              >
                {crumb.label}
              </a>
            ) : (
              <span
                className={cx("crumb-here", crumb.mono && "mono")}
                aria-current={last ? "page" : undefined}
              >
                {crumb.label}
              </span>
            )}
          </span>
        );
      })}
    </nav>
  );
}

/**
 * Copy this page's own address.
 *
 * THE WHOLE URL, not the hash: what a reader pastes into chat has to open on
 * somebody else's machine, and a bare `#/work/ENG-42` opens nothing.
 */
export function CopyLink({ label = "Copy link" }: { label?: string }) {
  const [said, setSaid] = useState("");
  const copy = useCallback(() => {
    const url = location.href;
    const done = (word: string) => {
      setSaid(word);
      window.setTimeout(() => setSaid(""), 1_600);
    };
    // The clipboard is refused outright on an insecure origin and by
    // permission policy, and a control that silently did nothing would read
    // as a broken button rather than as a browser that said no.
    navigator.clipboard?.writeText(url).then(
      () => done("Copied"),
      () => done("Blocked"),
    );
  }, []);
  return (
    <Button size="sm" variant="ghost" icon="copy" onClick={copy} title={label}>
      {said || label}
    </Button>
  );
}

/**
 * Keep a shortcut to this page, or drop the one you have.
 *
 * THE ONLY PRODUCER OF A STAR. `lib/starred.ts` was written whole — the cap,
 * the refusal at it, the storage guards, its own suite — and nothing in the
 * product could create one or read one back, so every workspace sidebar's
 * Starred section had an empty list by construction.
 *
 * ONE BUTTON, one gesture: a filled star means "not this one", which is why
 * `toggleStar` is the only verb. It reports, including the refusal at the cap
 * — reaching fifty is not "it worked" and must not render as a filled star.
 */
export function StarPage({ path, label, workspace }: Omit<Star, "at">) {
  const stars = useStarred();
  const toggle = useToggleStar();
  const [said, setSaid] = useState("");
  const key = path.join("/");
  const kept = stars.some((s) => s.path.join("/") === key);
  // A PAGE WITH NO IDENTITY CANNOT BE KEPT. The inbox and each workspace's
  // landing page are one click from the rail, and a star on one is a shortcut
  // to somewhere the reader is never more than one click from.
  if (path.length < 2) return null;
  const onClick = () => {
    const what = toggle({ path, label, workspace });
    if (what !== "full") return;
    setSaid(`${MaxStars} is the limit`);
    window.setTimeout(() => setSaid(""), 2_400);
  };
  return (
    <Button
      size="sm"
      variant="ghost"
      onClick={onClick}
      title={said || (kept ? "Remove from Starred" : "Keep in Starred")}
      aria-pressed={kept}
    >
      <Icon name="star" size="sm" fill={kept ? "currentColor" : "none"} />
      {said}
    </Button>
  );
}

export function PageBar({
  crumbs,
  actions,
  viewer,
  onSearch,
  onToggleSidebar,
}: {
  crumbs: Crumb[];
  actions?: ReactNode;
  viewer?: ReactNode;
  onSearch: () => void;
  /** Present only where a workspace has a sidebar to open. */
  onToggleSidebar?: () => void;
}) {
  return (
    <header className="page-bar">
      {onToggleSidebar && (
        <span className="drawer-toggle">
          <Button
            icon="menu"
            variant="ghost"
            size="sm"
            title="Navigation"
            onClick={onToggleSidebar}
          />
        </span>
      )}
      <Breadcrumb crumbs={crumbs} />
      <span className="spacer" />
      {/* THE SLOT IS ALWAYS RENDERED, whether or not a screen has controls:
          a portal needs a node to land in, and one that appears only when the
          frame already knows there are controls could never be found by the
          screen that has them. */}
      <div className="row gap-1 wrap page-actions" id={PAGE_ACTIONS_SLOT} />
      {actions}
      {viewer}
      <button className="omni" onClick={onSearch} title="Search everything">
        <Icon name="search" size="sm" />
        <span className="omni-label">Search</span>
        <kbd>⌘K</kbd>
      </button>
    </header>
  );
}
