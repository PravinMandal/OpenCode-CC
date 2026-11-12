// Free-tier 429 triage decision table.
//
// Zen returns the same generic FreeUsageLimitError for three unrelated
// conditions: a request whose shape its fingerprint does not recognize, a
// conversation state/length gate, and genuine quota exhaustion. The probe
// result is the discriminator: when a minimal canonical request succeeds on
// the same credential, quota is available and the real request was rejected
// for its own shape or history.

/**
 * @param {{probePassed: boolean, shapeRepairPassed?: boolean, conversationRebuildPassed?: boolean}} input
 * @returns {"shape"|"conversation"|"quota"}
 */
export function classifyGate({ probePassed, shapeRepairPassed = false, conversationRebuildPassed = false }) {
  if (!probePassed) return "quota";
  if (shapeRepairPassed) return "shape";
  if (conversationRebuildPassed) return "conversation";
  // The probe proved quota is available, so a still-failing request is
  // request- or conversation-specific even when both repairs were already
  // spent. Never report this as quota.
  return "conversation";
}

/**
 * Truncate message content to a total character budget, newest first, while
 * keeping every message and tool_call_id intact. Dropping messages would
 * orphan tool results, which is itself a shape failure; truncating content
 * preserves a valid history and still pulls an oversized prompt under the
 * free-tier gate.
 *
 * @param {Array<{role: string, content: any}>} messages
 * @param {number} maxChars
 * @param {string} marker
 * @returns {number} characters actually pruned
 */
export function compactMessages(messages, maxChars, marker) {
  if (!Array.isArray(messages) || !(maxChars > 0)) return 0;
  let budget = maxChars;
  let pruned = 0;
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i];
    if (!message || message.role === "system") continue;
    const content = message.content;
    if (typeof content === "string") {
      if (content.length <= budget) {
        budget -= content.length;
        continue;
      }
      const keep = Math.max(0, budget);
      pruned += content.length - keep;
      message.content = keep > 0 ? content.slice(0, keep) + marker : marker;
      budget = 0;
      continue;
    }
    if (Array.isArray(content)) {
      for (let part = content.length - 1; part >= 0; part--) {
        const item = content[part];
        if (!item || item.type !== "text" || typeof item.text !== "string") continue;
        if (item.text.length <= budget) {
          budget -= item.text.length;
          continue;
        }
        const keep = Math.max(0, budget);
        pruned += item.text.length - keep;
        item.text = keep > 0 ? item.text.slice(0, keep) + marker : marker;
        budget = 0;
      }
    }
  }
  return pruned;
}

/**
 * Whether a normalized request differs from the stock fingerprint in a way
 * that a canonical rebuild can remove. Only client-supplied system messages
 * and extra tools are repairable; a fully canonical request must not spin on
 * a pointless repair.
 */
export function shapeRepairable(shape) {
  return Boolean(shape && (shape.systemExtra > 0 || shape.toolsExtras > 0));
}
