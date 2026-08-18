// A three-function test harness for the dashboard's JavaScript.
//
// The dashboard ships as static ES modules with no build step and no
// package.json, which is a deliberate property of the project — the API
// serves the files as they are written. Pulling in a test runner would
// undo that for the sake of `describe`/`it`, so tests here register with
// `test()` and a pytest wrapper (tests/test_dashboard/test_dashboard_js.py)
// runs each file under whatever `node` is on PATH.

const registered = [];

export function test(name, fn) {
  registered.push({ name, fn });
}

/**
 * Run every registered test, in order.
 *
 * `run()` returns a promise and each test body is AWAITED. That is not a
 * nicety: a view's data loading is async, so its tests are too, and a
 * harness that called `fn()` without awaiting reported every async test
 * as passing the instant it started. A failing one then surfaced as an
 * unhandled rejection *after* the summary line had already claimed a
 * clean run — the process still exited non-zero, so CI would catch it,
 * but the output pointed at nothing and the count was a lie.
 *
 * A caller may `await run()`; the pytest wrapper only reads the exit
 * code and the summary, both of which are now written after the last
 * test has actually finished.
 */
export async function run() {
  let failures = 0;
  for (const { name, fn } of registered) {
    try {
      await fn();
      console.log(`ok   ${name}`);
    } catch (err) {
      failures++;
      console.error(`FAIL ${name}\n     ${err && err.message}`);
    }
  }
  console.log(`\n${registered.length - failures}/${registered.length} passed`);
  if (failures) process.exitCode = 1;
}
