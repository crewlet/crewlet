/**
 * The page bar: where you are, and what you can do about it.
 *
 * On every screen, and 52 px whenever it can be: the height is a FLOOR rather
 * than a fixed value, because the bar grows a second row once its own width
 * cannot hold the trail beside the page's controls — see `@container page` in
 * frame.css. It replaces a topbar that carried a single `<h1>` derived from
 * the route's first segment — so an item page said "Work", a project said
 * "Work", and a page nested four levels deep said "Knowledge".
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
import { Button, IconButton, Kbd, SearchTrigger, cx } from "@crewlethq/ui";
import { CopyGlyph, MenuGlyph, StarGlyph } from "@crewlethq/icons/glyphs";
// THE STAR HAS NO GLYPH IN UILET — see `StarPage` below.
import { PAGE_ACTIONS_SLOT } from "./PageActions.tsx";
import { MaxStars, starredIn, useStarred, useToggleStar, type Star } from "~/lib/starred.ts";

export interface Crumb {
  label: string;
  /** Absent on the last segment: the object is not a link to itself. */
  path?: string[];
  query?: Record<string, string>;
  /** Rendered in the mono face — a key, a handle, an id. */
  mono?: boolean;
}

/**
 * The trail, and it is a SCROLLPORT — so it is a tab stop.
 *
 * `.crumbs` scrolls sideways past its floor rather than clipping the ancestry
 * as a box (see frame.css), and its scrollbar is not drawn, because on a bar
 * this short it would sit on the baseline the trail is written on. Both of
 * those together are what make the tab stop load-bearing rather than
 * decorative: a pointer can drag a hidden scrollport and a trackpad can swipe
 * it, but a keyboard reaches a scroll container's arrow keys only once it can
 * be FOCUSED, and there is no other route to the overflowed crumbs — the links
 * inside it are focusable and scroll into view, but the deepest trail this
 * route table builds overflows on its ancestor SPANS, which are not links and
 * take no focus of their own.
 *
 * `tabIndex={0}` on a region with no interactive role is the documented way to
 * do it, and it is a stop the reader only meets where it does something: a
 * trail that fits is not scrollable, so the browser gives focus and the arrow
 * keys have nowhere to go. The `aria-label` was already here, which is what
 * makes the stop announce itself as the breadcrumb rather than as a bare
 * group.
 */
export function Breadcrumb({ crumbs }: { crumbs: Crumb[] }) {
  return (
    <nav className="crumbs" aria-label="Breadcrumb" tabIndex={0}>
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
  // NOT `Copyable`, which is the right part for an identifier: it DRAWS the
  // value it copies, and what this copies is the page's whole URL. A page bar
  // that rendered `http://host:8000/#/work/ENG-42?peek=seat:ada` where "Copy
  // link" is would push the breadcrumb off the bar.
  return (
    <Button
      size="small"
      variant="ghost"
      leadingIcon={<CopyGlyph size="sm" />}
      onClick={copy}
      title={label}
    >
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
  const kept = starredIn(stars, path);
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
  // THE KEPT STATE IS THE SAME STAR FILLED. `star` is the one glyph the
  // design system lists as `FILLABLE` — a closed silhouette whose inside
  // reads as the mark — and it is listed for exactly this toggle, so the
  // pressed state is `filled` rather than a second drawing. `pressed` says
  // the same thing to assistive technology.
  return (
    <Button
      size="small"
      variant="ghost"
      onClick={onClick}
      title={said || (kept ? "Remove from Starred" : "Keep in Starred")}
      pressed={kept}
      leadingIcon={<StarGlyph filled={kept} size="sm" />}
    >
      {/* `undefined` RATHER THAN `""` WHEN THERE IS NOTHING TO SAY: their
          Button falls back to `title` for the accessible name only when it has
          no children at all, and an empty label span is children. Without this
          the star is an unnamed button for as long as it is not refusing. */}
      {said || undefined}
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
          <IconButton
            label="Navigation"
            icon={<MenuGlyph size="sm" />}
            variant="ghost"
            size="sm"
            title="Navigation"
            onClick={onToggleSidebar}
          />
        </span>
      )}
      {/* AND IT IS WHAT PUSHES EVERYTHING ELSE RIGHT. A `.spacer` used to sit
          between the trail and the controls doing that job, which was fine
          while the trail was rigid — and the trail being rigid is exactly
          what broke the bar: a flex container assigns items to lines by their
          UNSHRUNK size, so a 427px trail forced a second line that a trail
          willing to give up 250px would not have. `.crumbs` is the bar's one
          flexible item now (see frame.css), so it both absorbs the free space
          and yields it, and a second grower beside it would halve what it
          gets for nothing. */}
      <Breadcrumb crumbs={crumbs} />
      {/* WHAT THIS PAGE CAN DO, IN ONE ELEMENT.
          The portalled slot and the frame's own actions were two siblings of
          the bar, which is fine until the bar has to fit a phone: there they
          move together onto a line of their own, and a rule cannot name a
          prop. `actions` is an arbitrary node with no wrapper of its own, so
          the wrapper is here. See `.page-controls` at the narrow breakpoint in
          frame.css. */}
      <div className="page-controls">
        {/* THE SLOT IS ALWAYS RENDERED, whether or not a screen has controls:
            a portal needs a node to land in, and one that appears only when
            the frame already knows there are controls could never be found by
            the screen that has them. */}
        <div className="row gap-2 wrap page-actions" id={PAGE_ACTIONS_SLOT} />
        {actions}
      </div>
      {viewer}
      {/* `SearchTrigger` IS `.omni`, down to the breakpoint that drops the
          label and the hint — and it fixes the bug that shape has: ours was
          named by its text, which `display: none` takes away, so under the
          breakpoint a screen reader announced "button". Its name is on the
          button.

          AND THE KEYCAP IS NOW HONEST. `lib/keys.ts` matches `metaKey ||
          ctrlKey`, so the chord has always worked on both platforms while the
          hardcoded `⌘K` named a key a Windows or Linux reader does not have.
          `Kbd keys={["Mod", "k"]}` draws the platform's own and reads it as a
          sentence rather than as "place of interest sign K".

          `toolbar`, NAMED, because the kit's default is `rail` — the
          sidebar's field, `width: 100%` and never giving up its words. As the
          default here it took every pixel the trail left at a desktop width
          and, under 1024px, a line of its own below the trail, where this bar
          is written to keep it beside the viewer chip. `toolbar` is the bar's
          field: a resting width, the card's ground, the glyph alone below the
          shell breakpoint.

          INSIDE `.page-search`, which is what lets it give way on a desktop
          bar too: the kit's resting width is a floor, and a route whose
          controls fill the line pushed the keycap past the edge. The wrapper
          makes the width flexible and collapses the field by its own width —
          see `.page-search` in frame.css. */}
      <span className="page-search">
        <SearchTrigger
          variant="toolbar"
          label="Search"
          title="Search everything"
          onClick={onSearch}
          keyshortcuts="Meta+K Control+K"
          shortcut={<Kbd keys={["Mod", "k"]} subtle />}
        />
      </span>
    </header>
  );
}
