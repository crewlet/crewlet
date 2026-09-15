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
import { Badge, Button, cx } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";

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
        <Icon name={open ? "chevronDown" : "chevronRight"} size="sm" />
        <span>Change this with your assistant</span>
      </button>
      {open && (
        <div className="toolcall-body">
          <p className="toolcall-note">
            This dashboard only reads. Paste one of these to the assistant you have connected to{" "}
            <code className="inline">/operator/mcp</code>, edited as you need it.
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
  const [copied, setCopied] = useState(false);
  const text = callText(call);
  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), COPIED_MS);
    } catch {
      // A blocked clipboard is ordinary — a page served over plain HTTP, or
      // a permission the reader declined. The call is on screen and
      // selectable either way, so the button simply stops claiming it
      // worked rather than raising anything.
      setCopied(false);
    }
  }
  return (
    <div className={cx("toolcall-row", call.destructive && "is-destructive")}>
      <div className="toolcall-head">
        <span className="toolcall-label">{call.label}</span>
        {call.destructive && (
          <Badge tone="critical" outline>
            irreversible
          </Badge>
        )}
        <span className="spacer" />
        <Button size="sm" variant="ghost" icon="copy" onClick={copy}>
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <code className="toolcall-code">{text}</code>
    </div>
  );
}
