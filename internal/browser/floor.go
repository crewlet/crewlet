package browser

// MinChromiumMajor is the oldest Chromium major the engine accepts and the
// suite certifies: the artifact render's client refuses an older browser, and
// the floor pin (scripts/chromium-pin.env) is its newest patch, which every
// case that drives Chromium runs on beside the newest (chromiumtest.Each),
// unless its doc says why the floor adds nothing to its claim. The package doc
// argues the value; raising it moves the floor pin in the same change.
const MinChromiumMajor = 153
