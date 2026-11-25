---
paths:
  - "internal/{providers,agent,toolsession,sandbox,mcprelay,mcp,assistant,compact,learning,tools,engine}/**"
  - "internal/api/mcpbridge/**"
  - "docs/concepts/llm-backends.md"
  - ".claude/worktrees/*/internal/{providers,agent,toolsession,sandbox,mcprelay,mcp,assistant,compact,learning,tools,engine}/**"
  - ".claude/worktrees/*/internal/api/mcpbridge/**"
  - ".claude/worktrees/*/docs/concepts/llm-backends.md"
---

# Harnesses
- **Why harnesses are the only LLM backend** (`CLAUDE.md`): vendors change request shapes, caching and sign-in with every model, and their harnesses absorb that. Every model call is driven through `internal/providers/llm/harness`: Claude Code for Anthropic's models (plan or API key), Codex for OpenAI's, OpenCode for open models, and the others `docs/concepts/llm-backends.md` lists under "Supported harnesses".
- **The harness runs its OWN loop.** A phase's tools reach it as ONE MCP server, `crewlet`, which it calls natively.
- **A seat's executor** is always its harness's own fully permissioned run in a box (Claude Code, Codex, Gemini CLI, Qwen Code, OpenCode, Cursor Agent, Copilot, Grok Build, Muse Code, Kimi Code, Hermes Agent or Pi, the twelve with an in-box runner; ADR-0066). It reaches the seat's tools over the bridge (`internal/api/mcpbridge`), which holds every call to the seat's surface.
- **Every other phase** (reviewer, delegated workers, onboarding pass, side passes) runs on the engine host with the harness's local tools denied, through `crewlet mcp-relay` into a per-run unix socket (`internal/toolsession`); the tool loop holds every call to the phase's gates.
- No tool rides a prompt, and no call is read out of a model's prose (ADR-0049).
