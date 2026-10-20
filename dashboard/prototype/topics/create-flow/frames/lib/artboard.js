// One artboard of a design: the screen in the page's JSON, its blocks drawn
// as the canvas draws them, filling the frame the canvas gives it.
(function () {
  "use strict";
  var page = window.demoPage;
  var screen = page.content();
  (screen.blocks || []).forEach(function (b) {
    var el = page.block(b);
    if (el) document.body.append(el);
  });
  page.at(screen.title || "Artboard");
})();
