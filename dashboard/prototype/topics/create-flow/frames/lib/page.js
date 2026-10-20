// What every demo page's renderer shares, as a classic script: a page in a
// sandboxed frame has an opaque origin, so it loads no module a server does
// not serve with CORS. It reads the page's content from its JSON, never code,
// and builds every element from text.
(function () {
  "use strict";

  function content() {
    var el = document.getElementById("content");
    return el ? JSON.parse(el.textContent || "{}") : {};
  }

  function append(el, children) {
    children.forEach(function (child) {
      if (Array.isArray(child)) return append(el, child);
      if (child === null || child === undefined || child === false) return;
      el.append(child instanceof Node ? child : document.createTextNode(String(child)));
    });
  }

  function h(tag, attrs) {
    var el = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      var v = attrs[k];
      if (v === undefined || v === null || v === false) return;
      if (k === "class") el.className = v;
      else if (k.slice(0, 2) === "on") el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? "" : String(v));
    });
    append(el, Array.prototype.slice.call(arguments, 2));
    return el;
  }

  function s(tag, attrs) {
    var el = document.createElementNS("http://www.w3.org/2000/svg", tag);
    Object.keys(attrs || {}).forEach(function (k) {
      el.setAttribute(k, String(attrs[k]));
    });
    append(el, Array.prototype.slice.call(arguments, 2));
    return el;
  }

  // Says where the reader is, on a page whose version declared presence.
  function at(region) {
    var crewlet = window.crewlet;
    if (!crewlet || document.documentElement.dataset.presence !== "on") return;
    crewlet.ready
      .then(function () {
        crewlet.presence.set({ activity: "viewing", focus: { region: region } });
      })
      .catch(function () {});
  }

  function block(b) {
    switch (b.kind) {
      case "title":
        return h("h2", { class: "b-title" }, b.text);
      case "text":
        return h("p", { class: "b-text" }, b.text);
      case "amount":
        return h("div", { class: "b-amount" }, b.text);
      case "row":
        return h("div", { class: "b-row" }, h("span", null, b.label), h("span", null, b.value));
      case "input":
        return h("label", { class: "b-input" }, h("span", null, b.label), h("span", null, b.value));
      case "chips":
        return h(
          "div",
          { class: "b-chips" },
          b.items.map(function (c) {
            return h("span", null, c);
          }),
        );
      case "button":
        return h("div", { class: "b-button" }, b.text);
      case "note":
        return h("div", { class: "b-note" }, b.text);
      default:
        return null;
    }
  }

  window.demoPage = { content: content, h: h, s: s, at: at, block: block };
})();
