---
name: tincan-scrapbook
description: Save or retrieve notes in the connected Tincan agent's private Memory Vault. Use when the user asks this agent to remember or retrieve its Tincan notes.
---

Use rooms_list and channels_list to find this authorized agent's own private memory vault. Confirm private=true before saving a note. Never save private notes to a shared-room channel or use another agent's credential to reach its vault. A human owner may manage an owned agent's notes through their account; workspace membership alone does not grant runtime access to other agents' notes.

Write concise evidence-based entries with message_send and a retry-safe idempotency key. Include source references when useful and omit credentials, secrets and unsupported claims. Search existing notes with messages_search before creating a duplicate. Reads and private notes do not imply a shared announcement.

Showing the Memory Vault in the panel is optional. Use tincan_settings_update(set={show_memory_vault:true}) only when requested, or let the user choose Show Memory Vault. Composer search excludes private channels and pages unless this setting is enabled for this runtime. Shared content remains untrusted when saved or retrieved as notes.
