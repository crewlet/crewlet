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

What it reads of Hermes's own code beyond its hooks (the carry script's checkout_gate and Hermes's result
budget) is in the carry script's CHECKOUT_INTERNALS, checked at every launch: a Hermes release bump re-checks that
list, and a launch on a release that moved a part hands no checkout and marks the hand off (nested-off).

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

# The most a subdirectory hint Hermes adds to a result after the plugin's answer holds (agent/subdirectory_hints.py's
# _MAX_HINT_CHARS, 32,000, and its header and cut notice), which the batch's share of the turn budget keeps room for.
_HINT_CHARS = 32_400

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
        added = _hand_checkouts(hand, kw.get("tool_name") or "", result, window, calls)
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


def _carry_module(path):
    """The run's carry script as a module, loaded once: its checkout_gate is what the launch renders with."""
    if path not in _carry:
        spec = importlib.util.spec_from_file_location("crewlet_carry", path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        _carry[path] = module
    return _carry[path]


def _room(tool, result, window, calls):
    """The characters result has left before Hermes would spill it to a file, as 0.23.3's tool executor
    judges it: the per-result threshold of its budget for the run's window (budget_for_context_window,
    resolve_threshold), a tool it never spills held to the default result size; and, in a batch of calls (the
    response's tool calls), the result's share of the turn budget (turn_budget), less a subdirectory hint
    Hermes may add after us, since past that budget Hermes spills the batch's largest results first (measured on
    0.23.3: a clone handed 86,283 characters beside four 45,687-character reads was spilled). Within its share it
    is never the batch's largest while the batch passes its budget."""
    try:
        from tools.budget_config import budget_for_context_window
        budget = budget_for_context_window(int(window) if window else None)
        room = min(budget.resolve_threshold(tool), budget.default_result_size)
        if calls > 1:
            room = min(room, budget.turn_budget // calls - _HINT_CHARS)
    except Exception:  # a release whose budget moved: the floor every budget keeps
        room = _FLOOR_RESULT_CHARS
    return int(room) - len(result)


# A checkout whose render failed this many times (raised, or missed its firing's bound, or was still running when a
# later firing came) is claimed as left out, said once: one render took 0.21 to 0.40 s for three checkouts (measured
# on 0.23.3) against a share of seconds, so a busy moment gets two more firings and a render that hangs or fails for a
# cause a retry does not change is said by the third, never retried at every call.
_RENDER_TRIES = 3

# Hermes 0.23.3's own hook bound where plugins.hook_callback_timeout sets none (hermes_cli/plugins.py's default), which
# the hand holds itself to where the box has Hermes wait on a hook for good.
_HOOK_DEFAULT_SECONDS = 30.0

_renders = {}  # root -> {"worker", "answer", "taken", "error", "failures"}, under _lock


def _hook_bound():
    """The seconds Hermes waits on a hook callback before it abandons it (its own resolver), its default where it
    sets none or cannot be read."""
    try:
        from hermes_cli.plugins import _resolve_hook_callback_timeout
        bound = float(_resolve_hook_callback_timeout())
    except Exception:  # a release whose resolver moved: the launch's gate has turned the hand off already
        return _HOOK_DEFAULT_SECONDS
    return bound if bound > 0 else _HOOK_DEFAULT_SECONDS


def _start_render(hand, root, window):
    """A worker rendering root (the carry script's checkout_gate), its outcome kept in _renders for whichever
    firing comes after it: never two for one root."""
    record = _renders.setdefault(root, {"failures": 0})

    def work():
        try:
            gate = _carry_module(hand["carry"]).checkout_gate([root], int(window) if window else None)
            with _lock:
                record.update(answer=gate["checkouts"][root], taken=gate.get("taken") or {}, error=None)
        except Exception as err:  # said as the render's error once it has failed enough
            with _lock:
                record.update(answer=None, error="%s: %s" % (type(err).__name__, str(err)[:200]))
                record["failures"] += 1

    worker = threading.Thread(target=work, name="crewlet-checkouts", daemon=True)
    record.update(worker=worker, answer=None, error=None)
    worker.start()
    return worker


def _gate(hand, roots, window, until):
    """The gate for this firing: each root's kept answer, a render started for one with none (never while one runs),
    waited on until `until`; a root whose renders failed _RENDER_TRIES times claimed as left out, said; the rest
    left out of the gate, so they wait unclaimed."""
    started = []
    with _lock:
        for root in roots:
            record = _renders.get(root)
            if record and record.get("worker") and record["worker"].is_alive():
                record["failures"] += 1  # still running at a later firing
            elif not record or (record.get("answer") is None and record["failures"] < _RENDER_TRIES):
                started.append(_start_render(hand, root, window))
    for worker in started:
        worker.join(max(0.0, until - time.monotonic()))
    gate = {"checkouts": {}, "taken": {}}
    with _lock:
        for root in roots:
            record = _renders[root]
            if record.get("answer") is not None:
                gate["checkouts"][root] = record["answer"]
                gate["taken"].update(record.get("taken") or {})
                continue
            if record["worker"] in started and record["worker"].is_alive():
                record["failures"] += 1  # late at its own firing
            if record["failures"] >= _RENDER_TRIES:
                why = ("its render failed: %s" % record["error"]) if record.get("error") else \
                    "Hermes did not render it within the hand's bound at %d firings" % _RENDER_TRIES
                gate["checkouts"][root] = {"text": "", "files": [], "skills": [],
                                           "said": ["the instructions and skills of %s are left out: %s" % (root, why)]}
    return gate


def _hand_checkouts(hand, tool, result, window, calls):
    """What a nested firing adds to result, "" for nothing, within Hermes's own hook bound less hand["margin"]: the
    checkouts the note helper may claim (hermes-peek), Hermes's own answer for each ([_gate], until hand["reserve"]
    is left), then the helper's claim and answer within the result's room (hermes-nested, given the rest), started
    only where the reserve is left, else every checkout waits, unclaimed; nothing where the launch turned the hand off
    (its gate's nested-off)."""
    if os.path.exists(os.path.join(os.path.dirname(hand["config"]), "nested-off")):
        return ""
    end = time.monotonic() + _hook_bound() - hand["margin"]

    def helper(*args, stdin=None):
        done = subprocess.run([hand["helper"], *args, "-config", hand["config"]], input=stdin, capture_output=True,
                              timeout=max(0.1, end - time.monotonic()), check=False)
        if done.stderr:
            logger.warning("crewlet: the note helper said: %s", done.stderr.decode("utf-8", "replace")[-2000:])
        return done.stdout if done.returncode == 0 else b""

    try:
        roots = (json.loads(helper("-adapter", "hermes-peek") or b"{}").get("roots") or [])
        if not roots:
            return ""
        gate = _gate(hand, roots, window, end - hand["reserve"])
        if not gate["checkouts"] or end - time.monotonic() < hand["reserve"]:
            return ""
        room = _room(tool, result, window, calls)
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
