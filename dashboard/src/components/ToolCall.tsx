/**
 * "Change this with your assistant" — the block a read-only product offers
 * where another would put an edit button.
 *
 * NOTHING HERE SENDS ANYTHING, and that is the point rather than a limitation.
 * Every change in this company is attributed to whoever made it; a browser
 * form posting as "the dashboard" would be the one actor an audit trail cannot
 * name. So the block shows the call an operator's own assistant would make,
 * with this object's ids already in it, and says whose name would land on the
 * change.
 *
 * The calls themselves are `lib/toolcall.ts`, which a Go gate checks against
 * the real operator catalogue — see its own note on why a table written in
 * TypeScript needs one.
 */

import { useState } from "react";
import { callText, callsFor, type Subject, type ToolCall as Call } from "~/lib/toolcall.ts";
import { Button, InlineCode, Tag, cx, useClipboard } from "@crewlethq/ui";
import {
  ChevronRightGlyph,
  ContentCopyGlyph,
  KeyboardArrowDownGlyph,
} from "@crewlethq/icons/glyphs";

/** How long the copy button says it worked. */
const COPIED_MS = 1_400;

/**
 * The block, or nothing at all.
 *
 * ABSENT RATHER THAN EMPTY where an object has no calls — a disclosure that
 * opens on "there is nothing you can do to this" is worse than no disclosure,
 * and the two objects in that state (a notice with no inbox, an object with no
 * id yet) are both transient.
 */
export function ToolCallBlock({ subject, viewer }: { subject: Subject; viewer?: string }) {
  const [open, setOpen] = useState(false);
  const calls = callsFor(subject);
  if (calls.length === 0) return null;
  return (
    <div className="toolcall">
      <button
        type="button"
        className="toolcall-toggle"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        {open ? <KeyboardArrowDownGlyph size="sm" /> : <ChevronRightGlyph size="sm" />}
        <span>Change this with your assistant</span>
      </button>
      {open && (
        <div className="toolcall-body">
          <p className="toolcall-note">
            This dashboard only reads. Paste one of these to the assistant you have connected to{" "}
            <InlineCode>/operator/mcp</InlineCode>, edited as you need it.
          </p>
          {calls.map((call) => (
            <CallRow key={call.tool} call={call} />
          ))}
          {/* WHO WOULD BE ATTRIBUTED. The reason this is not a form is that
              every change carries a name, so the block says which one. */}
          <p className="toolcall-note">
            The change would be recorded as {viewer ? <strong>{viewer}</strong> : "your token"}{" "}
            (operator), not as a seat.
          </p>
        </div>
      )}
    </div>
  );
}

function CallRow({ call }: { call: Call }) {
  // UILET'S CLIPBOARD, NOT OURS, AND IT FIXES A DEAD BUTTON.
  //
  // This used to `await navigator.clipboard.writeText` and swallow the throw.
  // The Clipboard API is gated on a SECURE CONTEXT, so `navigator.clipboard`
  // is simply undefined on the plain-http origin an operator reads a remote
  // dashboard at — which is exactly the reader this block exists for, since
  // they are the one with an assistant on the other side of it. There, the
  // press did nothing, said nothing, and left the operator to select the call
  // by hand. `useClipboard` keeps the deprecated `execCommand` path for that
  // origin and honours its boolean rather than assuming it worked.
  //
  // The wording is unchanged: "Copy", then "Copied". A failure still makes no
  // claim, which is what ours meant to do and could not.
  const clip = useClipboard({ resetMs: COPIED_MS });
  const text = callText(call);
  return (
    <div className={cx("toolcall-row", call.destructive && "is-destructive")}>
      <div className="toolcall-head">
        <span className="toolcall-label">{call.label}</span>
        {call.destructive && (
          <Tag variant="danger" appearance="outline">
            irreversible
          </Tag>
        )}
        <span className="spacer" />
        <Button
          size="small"
          variant="tertiary"
          leadingIcon={<ContentCopyGlyph />}
          onClick={() => void clip.copy(text)}
        >
          {clip.state === "copied" ? "Copied" : "Copy"}
        </Button>
      </div>
      <code className="toolcall-code">{text}</code>
    </div>
  );
}
