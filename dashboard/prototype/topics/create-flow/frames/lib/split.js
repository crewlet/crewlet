// The split calculator: an amount split between people, exact to the cent,
// with the leftover cents named, as FAIR-4's rule writes them.
(function () {
  "use strict";
  var h = window.demoPage.h;
  var people = ["Ana", "Ben", "Chen", "Dita"];
  var root = document.getElementById("tool");
  var amount = h("input", {
    type: "number",
    min: "0",
    step: "0.01",
    value: "100.00",
    inputmode: "decimal",
  });
  var count = h(
    "select",
    null,
    [2, 3, 4].map(function (n) {
      return h("option", { value: n, selected: n === 3 }, n + " people");
    }),
  );
  var rule = h(
    "select",
    null,
    h("option", { value: "first" }, "First names in order"),
    h("option", { value: "payer" }, "The payer"),
  );
  var out = h("div", { class: "result", "aria-live": "polite" });

  function draw() {
    var cents = Math.round(Number(amount.value || 0) * 100);
    var n = Number(count.value);
    var base = Math.floor(cents / n);
    var left = cents - base * n;
    var rows = [];
    for (var i = 0; i < n; i++) {
      var extra = rule.value === "payer" ? (i === 0 ? left : 0) : i < left ? 1 : 0;
      rows.push(
        h(
          "div",
          null,
          h("span", null, people[i] + (i === 0 ? " (paid)" : "")),
          h("b", null, ((base + extra) / 100).toFixed(2)),
        ),
      );
    }
    out.replaceChildren(...rows);
    note.textContent =
      left === 0
        ? "It splits evenly."
        : left +
          (left === 1 ? " leftover cent goes to " : " leftover cents go to ") +
          (rule.value === "payer" ? "the payer." : "the first names in order.");
  }

  var note = h("p", { class: "hint" });
  [amount, count, rule].forEach(function (el) {
    el.addEventListener("input", draw);
  });
  root.append(
    h(
      "main",
      { class: "tool" },
      h("h1", null, "Split calculator"),
      h(
        "p",
        null,
        "Try an amount against the leftover-cent rule before it ships in the command line.",
      ),
      h(
        "section",
        { class: "card" },
        h(
          "div",
          { class: "row" },
          h("label", { class: "field" }, h("span", null, "Amount"), amount),
          h("label", { class: "field" }, h("span", null, "Split between"), count),
          h("label", { class: "field" }, h("span", null, "Leftover cents go to"), rule),
        ),
      ),
      h("section", { class: "card" }, out, note),
    ),
  );
  draw();
})();
