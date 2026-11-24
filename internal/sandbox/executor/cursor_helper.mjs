// The Cursor Agent run's helper (cursor.go), run on the node Cursor's own
// package ships beside its binary. Every command fails closed: a store, a
// file or a line it does not know is refused with a reason on stderr, exit 3.
//
//   export <store.db> <out.jsonl> [live]
//       the chat's conversation as the messages of Cursor's own
//       ConversationHistory, one JSON message a line (the session's archive
//       member); live reads a store Cursor still writes, without recovering it
//   doc <in.jsonl> <out.json> [fragment.json]
//       those lines as the document --conversation-history-file reads, each
//       call to one of Crewlet's servers under an earlier run's name renamed
//       to this run's, the fragment's
//   mcp-add <mcp.json> <fragment.json> <state.json>
//       the run's servers laid into the home's mcp.json beside its own
//   mcp-remove <mcp.json> <state.json>
//       exactly the servers a run added taken out again, nothing else
//   cap <stream> <pid file> <cap> <marker>
//       the run's model responses counted off its stream, Cursor interrupted
//       at the cap-th that calls a tool
import fs from 'node:fs'
import path from 'node:path'

const refuse = (why) => {
  process.stderr.write(`crewlet: ${why}\n`)
  process.exit(3)
}

const readJSON = (file, what) => {
  let raw
  try {
    raw = fs.readFileSync(file, 'utf8')
  } catch (e) {
    if (e.code === 'ENOENT') return undefined
    refuse(`${what} (${file}) could not be read: ${e.message}`)
  }
  try {
    return JSON.parse(raw)
  } catch (e) {
    refuse(`${what} (${file}) is not JSON: ${e.message}`)
  }
}

// writeWhole writes text beside file and moves it over file, so a reader
// never sees half of it.
const writeWhole = (file, text) => {
  fs.mkdirSync(path.dirname(file), { recursive: true })
  const next = `${file}.crewlet-next`
  fs.writeFileSync(next, text, { mode: 0o600 })
  fs.renameSync(next, file)
}

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)

// ---- export

// The store keeps the chat as content-addressed blobs: meta row '0' (hex of
// a JSON object) names the latest root blob, a protobuf whose field 1 lists,
// in order, the ids of the conversation's messages, each a JSON blob.
function varint(b, i) {
  let x = 0
  let shift = 1
  for (let n = 0; n < 10; n++) {
    if (i >= b.length) refuse('the chat store holds a truncated root record')
    const c = b[i++]
    x += (c & 127) * shift
    if (!(c & 128)) return [x, i]
    shift *= 128
  }
  return refuse('the chat store holds a root record no protobuf reader reads')
}

function rootMessages(root) {
  const ids = []
  let i = 0
  while (i < root.length) {
    let key
    ;[key, i] = varint(root, i)
    const field = Math.floor(key / 8)
    const wire = key % 8
    if (wire === 0) {
      ;[, i] = varint(root, i)
    } else if (wire === 1 || wire === 5) {
      i += wire === 1 ? 8 : 4
      if (i > root.length) refuse('the chat store holds a truncated root record')
    } else if (wire === 2) {
      let len
      ;[len, i] = varint(root, i)
      if (i + len > root.length) refuse('the chat store holds a truncated root record')
      if (field === 1) {
        if (len !== 32) refuse(`the chat store's root names a message by a ${len}-byte id, not a sha256`)
        ids.push(root.subarray(i, i + len).toString('hex'))
      }
      i += len
    } else {
      refuse(`the chat store's root record holds a field of wire type ${wire}, which this run does not read`)
    }
  }
  return ids
}

const text = (t) => ({ text: { text: t } })

function userContent(content, at) {
  if (typeof content === 'string') return [text(content)]
  if (!Array.isArray(content)) refuse(`message ${at}: a user message's content is neither text nor parts`)
  return content.map((p) => {
    if (isObject(p) && p.type === 'text' && typeof p.text === 'string') return text(p.text)
    if (isObject(p) && p.type === 'image' && typeof p.image === 'string') {
      const image = { data: p.image }
      if (typeof p.mimeType === 'string') image.mimeType = p.mimeType
      return { image }
    }
    return refuse(`message ${at}: a user message holds a part of type ${JSON.stringify(p?.type)}, which this run does not carry`)
  })
}

function assistantContent(content, at) {
  if (typeof content === 'string') return [text(content)]
  if (!Array.isArray(content)) refuse(`message ${at}: an assistant message's content is neither text nor parts`)
  return content.map((p) => {
    if (!isObject(p)) return refuse(`message ${at}: an assistant message holds a part that is no object`)
    switch (p.type) {
      case 'text':
        if (typeof p.text === 'string') return text(p.text)
        break
      case 'reasoning':
        if (typeof p.text === 'string') {
          const reasoning = { text: p.text }
          const signature = p.signature ?? p.providerOptions?.anthropic?.signature
          if (typeof signature === 'string') reasoning.signature = signature
          return { reasoning }
        }
        break
      case 'redacted-reasoning':
        if (typeof p.data === 'string') return { redactedReasoning: { data: p.data } }
        break
      case 'tool-call':
        if (typeof p.toolCallId === 'string' && typeof p.toolName === 'string') {
          return { toolCall: { toolCallId: p.toolCallId, toolName: p.toolName, argsJson: JSON.stringify(p.args ?? {}) } }
        }
        break
    }
    return refuse(`message ${at}: an assistant message holds a part of type ${JSON.stringify(p.type)} of a shape this run does not carry`)
  })
}

function toolMessages(content, at) {
  if (!Array.isArray(content)) refuse(`message ${at}: a tool message's content is no list of results`)
  return content.map((p) => {
    if (!isObject(p) || p.type !== 'tool-result' || typeof p.toolCallId !== 'string' || typeof p.toolName !== 'string') {
      return refuse(`message ${at}: a tool message holds a part of type ${JSON.stringify(p?.type)} of a shape this run does not carry`)
    }
    let result
    if (typeof p.result === 'string') result = [text(p.result)]
    else if (Array.isArray(p.result) && p.result.every((r) => isObject(r) && r.type === 'text' && typeof r.text === 'string')) {
      result = p.result.map((r) => text(r.text))
    } else return refuse(`message ${at}: a tool result is of a shape this run does not carry`)
    const tool = { toolCallId: p.toolCallId, toolName: p.toolName, content: result }
    if (p.isError === true) tool.isError = true
    return { tool }
  })
}

async function exportChat(store, out, live) {
  if (!fs.existsSync(store)) refuse(`the chat store ${store} is not in the box`)
  let sqlite
  try {
    sqlite = await import('node:sqlite')
  } catch (e) {
    refuse(`this node has no SQLite, so the chat cannot be read: ${e.message}`)
  }
  // A STORE CURSOR STILL WRITES is read as it stands; one it left (a run
  // ended, or killed with its WAL beside it) is opened to be recovered.
  const db = new sqlite.DatabaseSync(store, { readOnly: live })
  try {
    const row = db.prepare("select value from meta where key = '0'").get()
    if (!row) refuse("the chat store holds no meta record '0'")
    let meta
    try {
      meta = JSON.parse(Buffer.from(String(row.value), 'hex').toString('utf8'))
    } catch (e) {
      refuse(`the chat store's meta record does not read: ${e.message}`)
    }
    if (!isObject(meta) || typeof meta.latestRootBlobId !== 'string') refuse('the chat store names no root')
    const blob = db.prepare('select data from blobs where id = ?')
    const get = (id) => {
      const r = blob.get(id)
      if (!r) refuse(`the chat store names the blob ${id}, which it does not hold`)
      return Buffer.from(r.data)
    }
    const messages = []
    let first = ''
    rootMessages(get(meta.latestRootBlobId)).forEach((id, at) => {
      const raw = get(id)
      let m
      try {
        m = JSON.parse(raw.toString('utf8'))
      } catch {
        refuse(`message ${at} is a blob of a kind this run does not carry (not a JSON message)`)
      }
      if (!isObject(m)) refuse(`message ${at} is no JSON object`)
      if (at === 0) first = m.role
      switch (m.role) {
        case 'system':
          // Cursor writes its system prompt again on every start, an
          // imported conversation's included.
          return
        case 'user':
          // CURSOR'S OWN ACCOUNT OF THE BOX, written afresh on every start
          // with the standing rules in it: carried, it would stand beside the
          // new one and keep the old rules (measured on 2026.11.22). Known by
          // where Cursor writes it, the message after its system prompt, as
          // text where a person's message is parts; one anywhere else refuses.
          if (typeof m.content === 'string' && m.content.startsWith('<user_info>')) {
            if (at === 1 && first === 'system') return
            refuse(`message ${at} is an account of the box (<user_info>) where Cursor writes none`)
          }
          messages.push({ user: { content: userContent(m.content, at) } })
          return
        case 'assistant':
          messages.push({ assistant: { content: assistantContent(m.content, at) } })
          return
        case 'tool':
          for (const t of toolMessages(m.content, at)) messages.push(t)
          return
        default:
          refuse(`message ${at} has the role ${JSON.stringify(m.role)}, which this run does not carry`)
      }
    })
    if (messages.length === 0) refuse('the chat holds no conversation to carry')
    writeWhole(out, messages.map((m) => JSON.stringify(m)).join('\n') + '\n')
  } finally {
    db.close()
  }
}

// ---- doc

const kinds = new Set(['user', 'assistant', 'tool'])

// renamer maps each of Crewlet's servers, by its name without the run's
// suffix, to this run's name for it: a history calls them under an earlier
// run's names, which this run's Cursor does not serve (measured: "MCP server
// does not exist"). Renamed here rather than named once per session: a
// session's id is in its run's own environment (CURSOR_CONVERSATION_ID), so
// code a run left could declare that name in the working directory, which
// stands in for the home's server of the same name, ahead of the next run.
function renamer(fragment) {
  const ours = new Map()
  if (fragment === undefined) return ours
  const f = readJSON(fragment, "the run's own MCP servers")
  if (!isObject(f?.mcpServers)) refuse("the run's own MCP servers name no mcpServers")
  for (const name of Object.keys(f.mcpServers)) {
    const m = /^(.+)-[0-9a-f]{8}$/.exec(name)
    if (!m) refuse(`the run's own MCP server ${JSON.stringify(name)} carries no run suffix`)
    ours.set(m[1], name)
  }
  return ours
}

// renamed is m with each of its calls to one of ours under an earlier run's
// name (`<server>-<8 hex>`) naming this run's instead.
function renamed(m, ours) {
  if (ours.size === 0 || !m.assistant) return m
  for (const c of m.assistant.content) {
    if (!c.toolCall) continue
    let args
    try {
      args = JSON.parse(c.toolCall.argsJson)
    } catch {
      continue
    }
    const was = isObject(args) && typeof args.server === 'string' && /^(.+)-[0-9a-f]{8}$/.exec(args.server)
    if (was && ours.has(was[1])) {
      args.server = ours.get(was[1])
      c.toolCall.argsJson = JSON.stringify(args)
    }
  }
  return m
}

function doc(lines, out, fragment) {
  const ours = renamer(fragment)
  let raw
  try {
    raw = fs.readFileSync(lines, 'utf8')
  } catch (e) {
    refuse(`the session's messages (${lines}) could not be read: ${e.message}`)
  }
  const messages = []
  raw.split('\n').forEach((line, at) => {
    if (line.trim() === '') return
    let m
    try {
      m = JSON.parse(line)
    } catch (e) {
      refuse(`line ${at + 1} of the session's messages is not JSON: ${e.message}`)
    }
    const keys = isObject(m) ? Object.keys(m) : []
    if (keys.length !== 1 || !kinds.has(keys[0])) refuse(`line ${at + 1} of the session's messages is no message`)
    messages.push(renamed(m, ours))
  })
  if (messages.length === 0) refuse("the session's messages hold no conversation")
  writeWhole(out, JSON.stringify({ messages }) + '\n')
}

// ---- mcp-add, mcp-remove

function mcpAdd(file, fragment, state) {
  const ours = readJSON(fragment, "the run's own MCP servers")
  if (!isObject(ours?.mcpServers)) refuse("the run's own MCP servers name no mcpServers")
  const home = readJSON(file, "the home's Cursor MCP servers (~/.cursor/mcp.json)")
  const created = home === undefined
  const doc = created ? {} : home
  if (!isObject(doc)) refuse("the home's Cursor MCP servers (~/.cursor/mcp.json) are no JSON object; fix the file in the box")
  if (doc.mcpServers === undefined) doc.mcpServers = {}
  if (!isObject(doc.mcpServers)) refuse("the home's Cursor MCP servers (~/.cursor/mcp.json) hold mcpServers as no object; fix the file in the box")
  const names = Object.keys(ours.mcpServers)
  // THE STATE FIRST, so a run cut off once its servers are in the file
  // leaves their names for the next run's removal.
  writeWhole(state, JSON.stringify({ names, created }) + '\n')
  for (const name of names) doc.mcpServers[name] = ours.mcpServers[name]
  writeWhole(file, JSON.stringify(doc, null, 2) + '\n')
}

function mcpRemove(file, state) {
  const s = readJSON(state, "the record of a run's own MCP servers")
  if (s === undefined) return
  if (!isObject(s) || !Array.isArray(s.names)) refuse("the record of a run's own MCP servers does not read")
  const doc = readJSON(file, "the home's Cursor MCP servers (~/.cursor/mcp.json)")
  if (isObject(doc) && isObject(doc.mcpServers)) {
    for (const name of s.names) delete doc.mcpServers[name]
    const emptied = Object.keys(doc.mcpServers).length === 0 && Object.keys(doc).length === 1
    if (s.created && emptied) fs.rmSync(file, { force: true })
    else writeWhole(file, JSON.stringify(doc, null, 2) + '\n')
  }
  fs.rmSync(state, { force: true })
}

// ---- cap

// A response that calls tools carries its id on each of its calls
// (model_call_id); a response that calls none is the run's answer. At the
// cap-th that calls a tool the run is interrupted, as Codex's cap ends its
// turn at the cap-th response (code-sandbox.md, "Codex").

// capCounter counts the responses stream holds, read from where its last
// read stopped; read reports whether the limit-th was reached.
function capCounter(stream, n) {
  const limit = Number(n)
  if (!Number.isInteger(limit) || limit < 1) refuse(`the turn cap ${n} is no positive whole number`)
  const seen = new Set()
  let offset = 0
  let rest = ''
  const read = () => {
    let fd
    try {
      fd = fs.openSync(stream, 'r')
    } catch {
      return false
    }
    try {
      const buf = Buffer.alloc(1 << 16)
      for (;;) {
        const got = fs.readSync(fd, buf, 0, buf.length, offset)
        if (got <= 0) return false
        offset += got
        rest += buf.subarray(0, got).toString('utf8')
        let nl
        while ((nl = rest.indexOf('\n')) >= 0) {
          const line = rest.slice(0, nl)
          rest = rest.slice(nl + 1)
          if (!line.includes('"tool_call"')) continue
          let ev
          try {
            ev = JSON.parse(line)
          } catch {
            continue
          }
          if (ev.type !== 'tool_call' || ev.subtype !== 'started' || typeof ev.model_call_id !== 'string') continue
          seen.add(ev.model_call_id)
          if (seen.size >= limit) return true
        }
      }
    } finally {
      fs.closeSync(fd)
    }
  }
  return { limit, read }
}

// cap follows a running Cursor's stream, and at the cap-th response marks the
// run capped and interrupts Cursor with SIGINT, which leaves its chat whole.
// It ends once Cursor does; the script then counts the whole stream again
// (capped), so a response the follower had not yet read is never missed.
function cap(stream, pidFile, n, marker) {
  const counter = capCounter(stream, n)
  const alive = (pid) => {
    try {
      process.kill(pid, 0)
      return true
    } catch {
      return false
    }
  }
  let pid = 0
  const tick = () => {
    if (!pid) {
      try {
        pid = Number(fs.readFileSync(pidFile, 'utf8').trim()) || 0
      } catch {}
    }
    if (pid && counter.read()) {
      fs.writeFileSync(marker, `${counter.limit}\n`)
      try {
        process.kill(pid, 'SIGINT')
      } catch {}
      process.exit(0)
    }
    if (pid && !alive(pid)) process.exit(0)
    // A TENTH OF A SECOND between reads: the stream's lines arrive a model
    // response at a time, seconds apart, so the cap stops a response's calls
    // well before they finish, at a read of a few bytes each time.
    setTimeout(tick, 100)
  }
  tick()
}

// capped marks the run capped when its whole stream holds the cap-th
// response, read once, after Cursor and the follower have ended.
function capped(stream, n, marker) {
  const counter = capCounter(stream, n)
  if (counter.read()) fs.writeFileSync(marker, `${counter.limit}\n`)
}

const [cmd, ...args] = process.argv.slice(2)
switch (cmd) {
  case 'export':
    await exportChat(args[0], args[1], args[2] === 'live')
    break
  case 'doc':
    doc(args[0], args[1], args[2])
    break
  case 'mcp-add':
    mcpAdd(args[0], args[1], args[2])
    break
  case 'mcp-remove':
    mcpRemove(args[0], args[1])
    break
  case 'cap':
    cap(args[0], args[1], args[2], args[3])
    break
  case 'capped':
    capped(args[0], args[1], args[2])
    break
  default:
    refuse(`the helper has no command ${JSON.stringify(cmd)}`)
}
