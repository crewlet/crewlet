"""Crewlet's runner plugin for Hermes Agent 0.23.3 (internal/sandbox/executor's hermesconfig.go).

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
import hashlib
import json
import logging
import os
import threading
import time
import urllib.error
import urllib.request

logger = logging.getLogger("crewlet")

# How long one note's steer may take, from finding the gateway to its answer. Measured on 0.23.3 (two cores):
# 0.3 s idle, 1.0 s with both cores busy, 2.3 s with three busy loops a core (session.resume 1.9 s of it). Half
# the notes route's 10 s hold, so a gateway that accepts and never answers costs the notes no more than a poll.
_STEER_SECONDS = 5.0

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


def register(ctx):
    settings = _settings()
    if not settings:
        return
    record = settings.get("record")
    skills = settings.get("skills") or {}
    skills_file = settings.get("skills_url_file")
    notes_file = settings.get("notes_url_file")
    reporting = {"on": bool(skills and skills_file)}

    def turn_started(**kw):
        with _lock:
            _turn["running"] = True
            _turn["session"] = kw.get("session_id") or _turn["session"]
        _wake.set()

    def turn_ended(**kw):
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

    async with asyncio.timeout(_STEER_SECONDS):
        found = await asyncio.to_thread(discover_gateway_endpoint, gateway_profile_home(get_hermes_home()))
        if not attachable(found):
            return False
        async with connect_gateway(endpoint=found.endpoint) as client:
            snapshot = await client.rpc("session.resume", session_id=session)
            sent["steer"] = True
            answer = await client.rpc("session.steer", session_id=session, text=text,
                                      execution_generation=snapshot["execution_generation"])
    # 0.23.3 answers queued, redirected or rejected (no turn running, nothing kept); session.steer takes no
    # key that would make a repeat harmless.
    return (answer or {}).get("status") in ("queued", "redirected")


def _steer(session, text):
    """Whether the note was handed over: steered, or its steer sent and its answer lost, which the session's
    record decides (a repeat would put it in the turn twice); not where it failed before the steer was sent,
    or Hermes refused it, so it is tried again."""
    sent = {"steer": False}
    try:
        return asyncio.run(_steer_async(session, text, sent))
    except Exception as err:
        if sent["steer"]:
            logger.warning("crewlet: a note's steer had no answer (%s); its record decides", type(err).__name__)
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
