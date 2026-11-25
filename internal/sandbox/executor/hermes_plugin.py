"""Crewlet's runner plugin for Hermes Agent 0.23.3 (internal/sandbox/executor's hermesconfig.go).

Loaded in the run's gateway, it does three things for the run its settings file names
(CREWLET_HERMES_PLUGIN: paths and digests, no secret):

- records every response the model gives, one JSON line in the settings' record: its usage, model,
  stop and window, which the runner reads back as the run's spend;
- reports each load of one of the run's skills to the skill route, judged by the digest of the file
  as the runner installed it, so a file the model rewrote never counts;
- takes the run's notes: while a turn runs it polls the notes route and steers each note into that
  turn through Hermes's own gateway client (session.steer), on the endpoint Hermes's discovery names,
  so it never starts a gateway. The engine confirms a note from the session's own record;
- hands a checkout cloned below the run's start directory during a turn its instructions and skills,
  in the result of the main loop's next tool call (transform_tool_result): the note helper names the
  checkouts it may claim, Hermes's own code renders each (the carry script's checkout_gate), and the
  helper claims and copies them, answering within what the result has room for before Hermes would
  spill it to a file. Hermes keeps the first plugin answer that rewrites a result, so where another plugin
  rewrites the same one (security guidance on a risky write), that clone's instructions are not handed during
  the run; the plugin never orders itself ahead of another, which would drop that plugin's words.

What it reads of Hermes's own code beyond its hooks (the carry script's checkout_gate, Hermes's result
budget and its subdirectory hint tracker) is in the carry script's CHECKOUT_INTERNALS, checked at every
launch: a Hermes release bump re-checks that list, and a launch on a release that moved a part hands no
checkout and marks the hand off (nested-off).

The routes carry the job's token, so each is read from its file at each use and never logged.
"""

import asyncio
import contextlib
import hashlib
import importlib.util
import json
import logging
import os
import sqlite3
import subprocess
import threading
import time
import urllib.error
import urllib.request

logger = logging.getLogger("crewlet")

# How long the steps before a note's steer may take (finding the gateway, connecting, session.resume), which a
# retry repeats harmlessly. Measured on 0.23.3 (two cores): 0.3 s idle, 1.0 s with both cores busy, 2.3 s with
# three busy loops a core (session.resume 1.9 s of it). Half the notes route's 10 s hold: a gateway that accepts
# and never answers costs a note at most this and the steer's bound below.
_REACH_SECONDS = 5.0
# How long session.steer itself may take, which no retry may repeat. Measured on 0.23.3: 0.003 to 0.028 s idle,
# 0.005 to 0.055 s with both cores busy, 0.006 to 0.015 s with three busy loops a core. Past it the steer's
# outcome is unknown and the session's record decides (a note that landed late is held; one that did not is
# served again), so waiting longer buys nothing but a later note's delay; 10 s is some 180 times the worst.
_STEER_SECONDS = 10.0

# The smallest result Hermes 0.23.3 keeps whole on any model (tools/budget_config.py's _MIN_RESULT_SIZE_CHARS):
# a nested hand's room where Hermes's own budget cannot be read, so its answer never makes a result spill.
_FLOOR_RESULT_CHARS = 8_000

_lock = threading.Lock()
_turn = {"running": False, "session": None, "window": None, "calls": 0}
_wake = threading.Event()


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """A route never redirects, and one followed would carry the token's path elsewhere."""

    def redirect_request(self, *args, **kwargs):
        return None


_opener = urllib.request.build_opener(_NoRedirect)


def _settings():
    path = os.environ.get("CREWLET_HERMES_PLUGIN")
    if not path:
        return None
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        logger.warning("crewlet: the run's plugin settings could not be read; the plugin does nothing")
        return None


def _route(path):
    try:
        with open(path, encoding="utf-8") as f:
            return f.read().strip().rstrip("/")
    except OSError:
        return ""


def _status(err):
    return err.code if isinstance(err, urllib.error.HTTPError) else None


def _continues(session, pinned):
    """Whether session is pinned's compaction continuation in the store, as 0.23.3's lineage.is_compression_child
    reads it (the carry script's continues): pinned ended "compression", session names it and is no tool's, and no
    branch, delegation or reset marker names it. The store is read, never written; one that cannot be is no."""
    home = os.environ.get("HERMES_HOME")
    if not home:
        return False
    try:
        db = sqlite3.connect("file:%s?mode=ro" % os.path.join(home, "state.db"), uri=True)
        try:
            child = db.execute("select parent_session_id, source, model_config from sessions where id = ?",
                               (session,)).fetchone()
            end = db.execute("select end_reason from sessions where id = ?", (pinned,)).fetchone()
        finally:
            db.close()
        config = json.loads(child[2]) if child and child[2] else {}
    except (sqlite3.Error, ValueError):
        return False
    config = config if isinstance(config, dict) else {}
    markers = {config.get(k) for k in ("_branched_from", "_delegate_from", "_reset_from")}
    return (child is not None and child[0] == pinned and end is not None and end[0] == "compression" and
            child[1] != "tool" and pinned not in markers)


def register(ctx):
    settings = _settings()
    if not settings:
        return
    record = settings.get("record")
    skills = settings.get("skills") or {}
    skills_file = settings.get("skills_url_file")
    notes_file = settings.get("notes_url_file")
    reporting = {"on": bool(skills and skills_file)}

    # A DELEGATED SUBAGENT'S TURN fires these hooks too, under its own session with parent_session_id set, inside
    # the run's turn (measured on 0.23.3); the notes are the run's session's, so only its own turns count: the
    # first top-level session the gateway turns for, then each session a compaction continues it in. A box whose
    # compression.in_place is false has a compaction rotate the session mid-turn (measured): the turn's end and
    # every later turn name the child, with parent_session_id empty, so the store says whose it is.
    def ours(session):
        with _lock:
            pinned = _turn["session"]
        if not pinned or session == pinned:
            return True
        if not _continues(session, pinned):
            return False
        with _lock:
            _turn["session"] = session
        return True

    def turn_started(**kw):
        session = kw.get("session_id")
        if kw.get("parent_session_id") or not session or not ours(session):
            return
        with _lock:
            if _turn["session"] and _turn["session"] != session:
                return
            _turn["running"] = True
            _turn["session"] = session
        _wake.set()

    def turn_ended(**kw):
        session = kw.get("session_id")
        if session and not ours(session):
            return
        with _lock:
            _turn["running"] = False

    def response(**kw):
        if kw.get("session_id") and kw.get("context_length") and ours(kw["session_id"]):
            with _lock:
                _turn["window"] = kw["context_length"]
                _turn["calls"] = kw.get("assistant_tool_call_count") or 0
        if not record:
            return
        usage = kw.get("usage") or {}
        reply = kw.get("response") or {}
        message = reply.get("assistant_message") or {}
        line = {
            "api_request_id": kw.get("api_request_id"),
            "turn_id": kw.get("turn_id"),
            "model": kw.get("response_model") or kw.get("model"),
            "finish_reason": kw.get("finish_reason"),
            "context_length": kw.get("context_length"),
            "tool_calls": len(message.get("tool_calls") or []),
            "usage": {k: usage.get(k) for k in ("input_tokens", "output_tokens", "cache_read_tokens",
                                                "cache_write_tokens")},
        }
        with _lock, open(record, "a", encoding="utf-8") as f:
            f.write(json.dumps(line, separators=(",", ":")) + "\n")

    def tool_done(**kw):
        if not reporting["on"] or kw.get("tool_name") != "skill_view":
            return
        name = (kw.get("args") or {}).get("name")
        want = skills.get(name)
        if not want:
            return
        try:
            result = json.loads(kw.get("result") or "")
        except (TypeError, ValueError):
            return
        content = result.get("content") if isinstance(result, dict) and result.get("success") else None
        if not isinstance(content, str) or hashlib.sha256(content.encode("utf-8")).hexdigest() != want:
            return
        url = _route(skills_file)
        if not url:
            return
        body = json.dumps({"skill": name}).encode()
        request = urllib.request.Request(url, data=body, method="POST", headers={"Content-Type": "application/json"})
        try:
            _opener.open(request, timeout=5).close()
        except Exception as err:  # the run goes on; the load is not reported
            if _status(err) in (401, 404):
                reporting["on"] = False
                logger.warning("crewlet: the skill route refused the plugin; no more loads are reported this run")
            else:
                logger.warning("crewlet: reporting skill %s failed (%s)", name, _status(err) or type(err).__name__)

    hand = settings.get("checkouts")

    def nested(**kw):
        # THE MAIN LOOP'S CALLS ALONE: a delegated subagent's calls carry no marker but their own session id, and a
        # checkout handed into a subagent's result never reaches the run (measured on 0.23.4, and with this plugin
        # on 0.23.3: rig g2, the subagent's clone result untouched, the main loop's next result handed it).
        session, result = kw.get("session_id"), kw.get("result")
        with _lock:
            pinned, window, calls = _turn["session"], _turn["window"], _turn["calls"]
        if not isinstance(result, str) or not session or not pinned or not ours(session):
            return None
        added = _hand_checkouts(hand, kw.get("tool_name") or "", kw.get("args"), result, window, calls)
        return result + "\n\n" + added if added else None

    ctx.register_hook("pre_llm_call", turn_started)
    ctx.register_hook("post_llm_call", turn_ended)
    ctx.register_hook("post_api_request", response)
    ctx.register_hook("post_tool_call", tool_done)
    if hand:
        ctx.register_hook("transform_tool_result", nested)
    if notes_file:
        threading.Thread(target=_notes, args=(notes_file,), name="crewlet-notes", daemon=True).start()


_carry = {}
_carry_lock = threading.Lock()


def _carry_module(path):
    """The run's carry script as a module, loaded once: its checkout_gate is what the launch renders with."""
    with _carry_lock:
        if path not in _carry:
            spec = importlib.util.spec_from_file_location("crewlet_carry", path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            _carry[path] = module
        return _carry[path]


def _room(tool, args, result, window, calls):
    """The characters result has left before Hermes would spill it to a file, as 0.23.3's tool executor
    judges it: the per-result threshold of its budget for the run's window (budget_for_context_window,
    resolve_threshold), a tool it never spills held to the default result size; and the result's share of the
    turn budget among the response's calls (turn_budget, all of it for one call), less the subdirectory hints
    Hermes adds to this result after us ([_hints]), since past that budget Hermes spills the batch's largest
    results first (measured on 0.23.3: a clone handed 86,283 characters beside four 45,687-character reads was
    spilled). Within its share it is never the batch's largest while the batch passes its budget."""
    try:
        from tools.budget_config import budget_for_context_window
        budget = budget_for_context_window(int(window) if window else None)
        room = min(budget.resolve_threshold(tool), budget.default_result_size,
                   budget.turn_budget // max(calls, 1) - _hints(tool, args))
    except Exception:  # a release whose budget or hints moved: the floor every budget keeps
        room = _FLOOR_RESULT_CHARS
    return int(room) - len(result)


def _hints(tool, args):
    """The characters of the subdirectory hints Hermes 0.23.3 adds to this call's result after the plugin's answer
    (agent/tool_executor.py, check_tool_call: one for each directory the call's paths newly reach, ancestors
    included, subdirectory_hints.py's _add_path_candidate), as a tracker of Hermes's own answers for the call
    from the run's working directory (where agent_init.py starts the session's). It has loaded nothing the
    session's tracker has, so it answers for every directory that one would, never fewer."""
    from agent.runtime_cwd import scope_terminal_cwd
    from agent.subdirectory_hints import SubdirectoryHintTracker
    tracker = SubdirectoryHintTracker(working_dir=scope_terminal_cwd() or None)
    return len(tracker.check_tool_call(tool, args if isinstance(args, dict) else {}) or "")


# A checkout whose renders failed this many times is claimed as left out, said once: a render fails once for each
# hand's bound it outlives, by its age, whichever firings wait on it (0.23.3 fires every call of a parallel batch at
# once, so counting each firing that found it running would give up a render on time), and once where it raised.
# One render took 0.21 to 0.40 s for three checkouts (measured on 0.23.3) against a bound of seconds, so a busy
# moment gets two more bounds and a render that hangs or fails for a cause a retry does not change is said by the
# third, never retried at every call.
_RENDER_TRIES = 3

# Hermes 0.23.3's own hook bound where plugins.hook_callback_timeout sets none (hermes_cli/plugins.py's default), which
# the hand holds itself to where the box has Hermes wait on a hook for good.
_HOOK_DEFAULT_SECONDS = 30.0

# root -> {"worker", "started", "span", "answer", "stamp", "error", "failures"}, under _lock: "worker" is the
# running render (None once it finished), "failures" what finished renders counted, "error" the last one raised.
_renders = {}


def _hook_bound():
    """The seconds Hermes waits on a hook callback before it abandons it (its own resolver), its default where it
    sets none or cannot be read."""
    try:
        from hermes_cli.plugins import _resolve_hook_callback_timeout
        bound = float(_resolve_hook_callback_timeout())
    except Exception:  # a release whose resolver moved: the launch's gate has turned the hand off already
        return _HOOK_DEFAULT_SECONDS
    return bound if bound > 0 else _HOOK_DEFAULT_SECONDS


def _failures(record, now):
    """What record's renders have failed by now: the finished ones' count, and each bound the running one has
    outlived since the firing that started it."""
    if record["worker"] is None:
        return record["failures"]
    return record["failures"] + int((now - record["started"]) // record["span"])


def _start_render(hand, root, window, started, span, record):
    """A worker rendering root (the carry script's checkout_gate) for a firing that started at `started` and gives
    it `span` seconds, its outcome kept in record for whichever firing comes after it, with the stamp of what it
    read (checkout_stamp, taken first, so a change during the render shows). Called under _lock."""

    def work():
        try:
            carry = _carry_module(hand["carry"])
            stamp = carry.checkout_stamp(root)
            answer, error = carry.checkout_gate([root], int(window) if window else None)["checkouts"][root], None
        except Exception as err:  # said as the render's error once it has failed enough
            answer, stamp, error = None, None, "%s: %s" % (type(err).__name__, str(err)[:200])
        late = int((time.monotonic() - started) // span)
        with _lock:
            if error is None:
                record.update(answer=answer, stamp=stamp)
            else:
                record["error"], late = error, max(late, 1)
            record["failures"] += late
            record["worker"] = None

    worker = threading.Thread(target=work, name="crewlet-checkouts", daemon=True)
    record.update(worker=worker, started=started, span=span, answer=None)
    worker.start()


def _gate(hand, roots, window, started, until):
    """The gate for a firing that started at `started`: each root's kept answer, unless what it read changed since
    (then rendered again); a render started for a root with none (never while one runs); every running render
    waited on until `until`; a root whose renders failed _RENDER_TRIES times claimed as left out, said with the last
    error, else as late; the rest left out of the gate, so they wait unclaimed. The home's taken names are read now,
    for the claim: a person may have added a skill since a kept answer was rendered."""
    carry = _carry_module(hand["carry"])
    with _lock:
        kept = [root for root in roots if (_renders.get(root) or {}).get("answer") is not None]
    stamps = {root: carry.checkout_stamp(root) for root in kept}
    waits = []
    with _lock:
        for root in roots:
            record = _renders.setdefault(root, {"worker": None, "answer": None, "stamp": None, "error": None,
                                                "failures": 0})
            if root in stamps and record["answer"] is not None and record["stamp"] != stamps[root]:
                record["answer"] = None
            if record["worker"] is None and record["answer"] is None and record["failures"] < _RENDER_TRIES:
                _start_render(hand, root, window, started, until - started, record)
            if record["worker"] is not None:
                waits.append(record["worker"])
    for worker in waits:
        worker.join(max(0.0, until - time.monotonic()))
    now = max(time.monotonic(), until) if waits else time.monotonic()  # a render still running outlived this bound
    gate = {"checkouts": {}, "taken": {}}
    with _lock:
        for root in roots:
            record = _renders[root]
            if record["answer"] is not None:
                gate["checkouts"][root] = record["answer"]
            elif _failures(record, now) >= _RENDER_TRIES:
                why = ("its render failed: %s" % record["error"]) if record["error"] else \
                    "Hermes did not render it within the hand's bound %d times" % _RENDER_TRIES
                gate["checkouts"][root] = {"text": "", "files": [], "skills": [],
                                           "said": ["the instructions and skills of %s are left out: %s" % (root, why)]}
    if gate["checkouts"]:
        gate["taken"] = carry.skill_names()
    return gate


def _hand_checkouts(hand, tool, args, result, window, calls):
    """What a nested firing adds to result, "" for nothing, within Hermes's own hook bound less hand["margin"]: the
    checkouts the note helper may claim (hermes-peek), Hermes's own answer for each ([_gate], waited on until
    hand["reserve"] is left), then the helper's claim and answer within the result's room (hermes-nested, bounded
    by the hand's end), started only while half the reserve is left; nothing where the reserve was gone before a
    render could start, or where the launch turned the hand off (its gate's nested-off)."""
    if os.path.exists(os.path.join(os.path.dirname(hand["config"]), "nested-off")):
        return ""
    started = time.monotonic()
    end = started + _hook_bound() - hand["margin"]
    until = end - hand["reserve"]

    def helper(*args, stdin=None):
        done = subprocess.run([hand["helper"], *args, "-config", hand["config"]], input=stdin, capture_output=True,
                              timeout=max(0.1, end - time.monotonic()), check=False)
        if done.stderr:
            logger.warning("crewlet: the note helper said: %s", done.stderr.decode("utf-8", "replace")[-2000:])
        return done.stdout if done.returncode == 0 else b""

    try:
        roots = (json.loads(helper("-adapter", "hermes-peek") or b"{}").get("roots") or [])
        if not roots or time.monotonic() >= until:
            return ""  # no room left for a render and a claim: every checkout waits
        gate = _gate(hand, roots, window, started, until)
        if not gate["checkouts"]:
            return ""
        room = _room(tool, args, result, window, calls)
        # HALF THE RESERVE: a wait ends with the whole reserve left; a box that held the firing past its bound by
        # more than half of it (1.5 s of the runner's 3, some 12 times the claim's measured 0.12 s) leaves the
        # claim to a later firing.
        if end - time.monotonic() < hand["reserve"] / 2:
            return ""
        return helper("-adapter", "hermes-nested", "-room", str(room),
                      stdin=json.dumps(gate).encode("utf-8")).decode("utf-8", "replace")
    except Exception as err:  # the run goes on; a checkout claimed is in the run's log, a later one waits
        logger.warning("crewlet: handing a checkout cloned during the run failed (%s)", type(err).__name__)
        return ""


async def _steer_async(session, text, sent):
    from hermes_constants import get_hermes_home
    from hermes_cli.gateway_client import connect_gateway
    from hermes_cli.gateway_runtime import attachable, discover_gateway_endpoint, gateway_profile_home

    async with contextlib.AsyncExitStack() as stack:
        async with asyncio.timeout(_REACH_SECONDS):
            found = await asyncio.to_thread(discover_gateway_endpoint, gateway_profile_home(get_hermes_home()))
            if not attachable(found):
                return False
            client = await stack.enter_async_context(connect_gateway(endpoint=found.endpoint))
            snapshot = await client.rpc("session.resume", session_id=session)
        sent["steer"] = True
        async with asyncio.timeout(_STEER_SECONDS):
            answer = await client.rpc("session.steer", session_id=session, text=text,
                                      execution_generation=snapshot["execution_generation"])
    # 0.23.3's session.steer answers queued (kept for the turn) or rejected (no turn running, nothing kept);
    # redirected is session.redirect's. It takes no key that would make a repeat harmless.
    return (answer or {}).get("status") == "queued"


def _steer(session, text):
    """Whether to move past the note: steered, or its steer sent with its outcome unknown (its answer lost or
    late), which the session's record decides, since a repeat could put it in the turn twice; not where it failed
    before the steer was sent, or Hermes rejected it, so it is tried again."""
    sent = {"steer": False}
    try:
        return asyncio.run(_steer_async(session, text, sent))
    except Exception as err:
        if sent["steer"]:
            logger.warning("crewlet: a note's steer has an unknown outcome (%s); its record decides", type(err).__name__)
            return True
        logger.warning("crewlet: a note could not be steered into the turn (%s)", type(err).__name__)
        return False


def _notes(notes_file):
    """Polls only while a turn runs, at the pace the other runners' plugins keep: a failed poll or a
    refused note waits a second, doubling to thirty, reset by the next success; a poll that handed
    nothing over starts the next no sooner than a second after it began."""
    cursor, pause = 0, 1.0
    while True:
        with _lock:
            running, session = _turn["running"], _turn["session"]
        if not running or not session:
            _wake.wait(1.0)
            _wake.clear()
            continue
        began = time.monotonic()
        url = _route(notes_file)
        try:
            # The route holds a poll ten seconds while no note waits (mcpbridge's steerHold).
            with _opener.open(url + "/notes?after=%d" % cursor, timeout=20) as answer:
                notes = json.loads(answer.read(1 << 20)).get("notes") or []
        except Exception as err:
            if _status(err) in (401, 404):
                logger.warning("crewlet: the notes route refused the plugin; its notes wait for the run's review")
                return
            time.sleep(pause)
            pause = min(pause * 2, 30.0)
            continue
        handed = False
        for note in notes:
            seq, text = note.get("seq"), note.get("message")
            if not isinstance(seq, int) or seq <= cursor or not isinstance(text, str) or not text:
                continue
            if not _steer(session, text):
                break
            cursor, handed = seq, True
        if notes and not handed:
            time.sleep(pause)
            pause = min(pause * 2, 30.0)
            continue
        pause = 1.0
        time.sleep(max(0.0, 1.0 - (time.monotonic() - began)))
