// The settle-up explainer: a group's balances, and the fewest payments that
// bring every balance to zero, worked greedily from the largest debts.
(function () {
  "use strict";
  var h = window.demoPage.h;
  var group = window.demoPage.content();
  var balances = group.balances.map(function (b) {
    return { name: b.name, cents: Math.round(b.amount * 100) };
  });
  var root = document.getElementById("tool");
  var list = h("div", { class: "result" });
  var pays = h("div", { class: "result", "aria-live": "polite" });

  function settle(rows) {
    var debtors = rows
      .filter(function (r) {
        return r.cents < 0;
      })
      .map(function (r) {
        return { name: r.name, cents: -r.cents };
      });
    var creditors = rows
      .filter(function (r) {
        return r.cents > 0;
      })
      .map(function (r) {
        return { name: r.name, cents: r.cents };
      });
    var out = [];
    debtors.sort(function (a, b) {
      return b.cents - a.cents;
    });
    creditors.sort(function (a, b) {
      return b.cents - a.cents;
    });
    while (debtors.length && creditors.length) {
      var d = debtors[0];
      var c = creditors[0];
      var x = Math.min(d.cents, c.cents);
      out.push({ from: d.name, to: c.name, cents: x });
      d.cents -= x;
      c.cents -= x;
      if (!d.cents) debtors.shift();
      if (!c.cents) creditors.shift();
    }
    return out;
  }

  function money(c) {
    return (c < 0 ? "−" : "") + (Math.abs(c) / 100).toFixed(2);
  }

  function draw() {
    list.replaceChildren.apply(
      list,
      balances.map(function (b) {
        return h("div", null, h("span", null, b.name), h("b", null, money(b.cents)));
      }),
    );
    var plan = settle(balances);
    pays.replaceChildren.apply(
      pays,
      plan.map(function (p) {
        return h(
          "div",
          null,
          h("span", null, p.from + " pays " + p.to),
          h("b", null, money(p.cents)),
        );
      }),
    );
    count.textContent =
      plan.length +
      (plan.length === 1 ? " payment settles the group." : " payments settle the group.");
  }

  var count = h("p", { class: "hint" });
  root.append(
    h(
      "main",
      { class: "tool" },
      h("h1", null, group.title),
      h("p", null, group.intro),
      h(
        "section",
        { class: "card", "aria-label": "Balances" },
        h("strong", null, "Balances"),
        list,
      ),
      h(
        "section",
        { class: "card", "aria-label": "Payments" },
        h("strong", null, "Fewest payments"),
        pays,
        count,
      ),
    ),
  );
  draw();
})();
