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

export function run() {
  let failures = 0;
  for (const { name, fn } of registered) {
    try {
      fn();
      console.log(`ok   ${name}`);
    } catch (err) {
      failures++;
      console.error(`FAIL ${name}\n     ${err && err.message}`);
    }
  }
  console.log(`\n${registered.length - failures}/${registered.length} passed`);
  if (failures) process.exitCode = 1;
}
