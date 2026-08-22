// Dialogs that belong to the page.
//
// Token entry was a `window.prompt` and reverting a config revision was a
// `window.confirm` — the only unstyled UI in the product, and the two
// places where the reader is being asked to do something consequential.
// A browser dialog also cannot say what a revert will actually do, which
// is the entire question being asked.
//
// Both return promises so a caller reads as a sequence rather than as a
// callback. Neither resolves until the reader chooses.

import { esc } from "./format.js";

let host = null;
let escapeHandler = null;

function ensureHost() {
  if (host && host.isConnected) return host;
  host = document.getElementById("modal-host");
  return host;
}

function close(resolve, value) {
  const el = ensureHost();
  if (el) {
    el.hidden = true;
    el.innerHTML = "";
  }
  if (escapeHandler) {
    document.removeEventListener("keydown", escapeHandler);
    escapeHandler = null;
  }
  resolve(value);
}

function open({ markup, onMount, resolve, cancelValue }) {
  const el = ensureHost();
  if (!el) {
    // No host element means the shell is not there to hold a dialog. A
    // caller must still get an answer rather than a promise that never
    // settles, and refusing is the safe one for a destructive action.
    resolve(cancelValue);
    return;
  }
  el.hidden = false;
  el.innerHTML = markup;

  escapeHandler = (ev) => {
    if (ev.key === "Escape") close(resolve, cancelValue);
  };
  document.addEventListener("keydown", escapeHandler);

  el.querySelector("[data-modal-cancel]")?.addEventListener("click", () =>
    close(resolve, cancelValue),
  );
  // A click on the backdrop, but not one that started inside the dialog:
  // dragging to select text in the body and releasing outside it used to
  // count as a dismissal.
  el.querySelector(".modal-veil")?.addEventListener("mousedown", (ev) => {
    if (ev.target.classList.contains("modal-veil")) close(resolve, cancelValue);
  });

  onMount(el, (value) => close(resolve, value));
}

/**
 * Ask a yes/no question. Resolves `true` only on an explicit confirm.
 *
 * `danger` paints the confirm control in the reserved status hue — the
 * same red that means "this broke" everywhere else, here meaning "this
 * cannot be undone".
 */
export function confirmModal({ title, body, confirm = "Confirm", danger = false }) {
  return new Promise((resolve) => {
    open({
      resolve,
      cancelValue: false,
      markup: `
        <div class="modal-veil">
          <div class="modal" role="alertdialog" aria-modal="true"
               aria-labelledby="modal-title">
            <h2 id="modal-title" class="modal-title">${esc(title)}</h2>
            <div class="modal-body">${esc(body)}</div>
            <div class="modal-actions">
              <button class="btn" type="button" data-modal-cancel>Cancel</button>
              <button class="btn ${danger ? "danger" : "primary"}" type="button"
                      data-modal-confirm>${esc(confirm)}</button>
            </div>
          </div>
        </div>`,
      onMount(el, done) {
        const button = el.querySelector("[data-modal-confirm]");
        button.addEventListener("click", () => done(true));
        button.focus();
      },
    });
  });
}

/**
 * Ask for one value. Resolves the string, or `null` if dismissed.
 *
 * Used for the API token, which is why the field is a password input and
 * why the note explains what the token is for — a bare prompt asking for
 * a secret, with no indication of who wants it, is a phishing lesson.
 */
export function promptModal({ title, body, label, value = "", secret = false }) {
  return new Promise((resolve) => {
    open({
      resolve,
      cancelValue: null,
      markup: `
        <div class="modal-veil">
          <form class="modal" role="dialog" aria-modal="true"
                aria-labelledby="modal-title">
            <h2 id="modal-title" class="modal-title">${esc(title)}</h2>
            ${body ? `<div class="modal-body">${esc(body)}</div>` : ""}
            <label class="modal-label" for="modal-input">${esc(label)}</label>
            <input class="modal-input" id="modal-input"
                   type="${secret ? "password" : "text"}"
                   value="${esc(value)}" autocomplete="off" spellcheck="false" />
            <div class="modal-actions">
              <button class="btn" type="button" data-modal-cancel>Cancel</button>
              <button class="btn primary" type="submit">Save</button>
            </div>
          </form>
        </div>`,
      onMount(el, done) {
        const form = el.querySelector("form");
        const input = el.querySelector("#modal-input");
        form.addEventListener("submit", (ev) => {
          ev.preventDefault();
          const entered = input.value.trim();
          done(entered || null);
        });
        input.focus();
        input.select();
      },
    });
  });
}
