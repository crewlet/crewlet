"""Crewlet's runner plugin for Hermes Agent 0.23.4 (internal/sandbox/executor's hermesconfig.go).

Loaded in the run's gateway, it does three things for the run its settings file names
(CREWLET_HERMES_PLUGIN: paths and digests, no secret):

- records every response the model gives, one JSON line in the settings' record: its usage, model,
  stop and window, which the runner reads back as the run's spend;
- reports each load of one of the run's skills to the skill route, judged by the digest of the file
  as the runner installed it, so a file the model rewrote never counts;
- takes the run's notes: while a turn runs it polls the notes route and steers each note into that
  turn through Hermes's own gateway client (session.steer), on the endpoint Hermes's discovery names,
  so it never starts a gateway. The engine confirms a note from the session's own record.

The routes carry the job's token, so each is read from its file at each use and never logged.
"""

import asyncio
import contextlib
import hashlib
import json
import logging
import os
import sqlite3
import threading
import time
import urllib.error
import urllib.request

logger = logging.getLogger("crewlet")

# How long the steps before a note's steer may take (finding the gateway, connecting, session.resume), which a
# retry repeats harmlessly. Measured on 0.23.4 (two cores): 0.1 to 0.8 s idle, 0.3 to 0.6 s with three busy loops
# a core (session.resume most of it). Half the notes route's 10 s hold: a gateway that accepts and never answers
# costs a note at most this and the steer's bound below.
_REACH_SECONDS = 5.0
# How long session.steer itself may take, which no retry may repeat. Measured on 0.23.4: 0.004 to 0.008 s idle,
# 0.002 to 0.005 s with three busy loops a core. Past it the steer's outcome is unknown and the session's record
# decides (a note that landed late is held; one that did not is served again), so waiting longer buys nothing
# but a later note's delay; 10 s is some 1,250 times the worst.
_STEER_SECONDS = 10.0

_lock = threading.Lock()
_turn = {"running": False, "session": None}
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
    """Whether session is pinned's compaction continuation in the store, as 0.23.4's lineage.is_compression_child
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
    # the run's turn (measured on 0.23.4); the notes are the run's session's, so only its own turns count: the
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

    ctx.register_hook("pre_llm_call", turn_started)
    ctx.register_hook("post_llm_call", turn_ended)
    ctx.register_hook("post_api_request", response)
    ctx.register_hook("post_tool_call", tool_done)
    if notes_file:
        threading.Thread(target=_notes, args=(notes_file,), name="crewlet-notes", daemon=True).start()


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
    # 0.23.4's session.steer answers queued (kept for the turn) or rejected (no turn running, nothing kept);
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
