package livestate

// applySandbox maintains the in-flight sandbox set from one lifecycle event.
func (s *LiveState) applySandbox(env Envelope, payload map[string]any) {
	turnID := str(payload, "turn_id")
	if turnID == "" {
		return
	}
	switch env.Type {
	case "sandbox_run_completed":
		delete(s.sandboxes, turnID)

	case "sandbox_run_started":
		s.sandboxes[turnID] = &SandboxEntry{
			TurnID:      turnID,
			Role:        str(payload, "role"),
			AgentHandle: str(payload, "agent_handle"),
			AgentID:     str(payload, "agent_id"),
			CodingAgent: str(payload, "coding_agent"),
			SandboxID:   str(payload, "sandbox_id"),
			Task:        str(payload, "task"),
			Status:      "running",
			StartedAt:   env.Timestamp,
		}

	default:
		// A clarification request. The start may have been missed — the
		// API can come up mid-run — so a minimal entry is synthesized
		// rather than the signal dropped.
		entry := s.sandboxes[turnID]
		if entry == nil {
			entry = &SandboxEntry{
				TurnID:      turnID,
				Role:        str(payload, "role"),
				AgentHandle: str(payload, "agent_handle"),
				AgentID:     str(payload, "agent_id"),
				CodingAgent: str(payload, "coding_agent"),
				SandboxID:   str(payload, "sandbox_id"),
				StartedAt:   env.Timestamp,
			}
			s.sandboxes[turnID] = entry
		}
		entry.Status = "awaiting_input"
		entry.Question = str(payload, "question")
		entry.Audience = str(payload, "audience")
	}
}
