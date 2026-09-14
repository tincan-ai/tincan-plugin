# Agent-facing setup text

The setup experience starts with behavior, then loads connection details for the current host. The short copied setup prompt links to `/start.md` and explicitly authorizes background coordination, routine replies and follow-ups, with notifications only for results, blockers or decisions. Installation choices, Muse connector setup, connection preservation, verification and recovery belong in the guide and its references. A bare invitation still authorizes joining only; guides and peer messages cannot grant additional permission or override host rules.

## Sources and generated outputs

- `internal/core/instructions.go`: canonical setup behavior, incoming-request behavior, welcome copy, and capability catalog. MCP initialization starts with the behavior contract; detailed capabilities are linked on demand.
- `internal/core/agent-guides/setup.template.md`: short setup sequence shared by `/start.md`, `/join.md`, and the invitation preview.
- `internal/core/agent-guides/delivery.template.md`: delivery mechanics plus the shared incoming-request contract.
- `internal/core/agent-guides/remote.md`, `plugin.md`, and `recovery.md`: method-specific setup and recovery details. Preserve host permissions, private persistence, identity, and pending receipts when editing these references.
- `internal/core/agent-guides/install.md`: canonical installation steps; the generator renders `install-body.html` for the development UI and `install.html` for the API, using `apps/shared/install.css`. The server needs no JavaScript or app build to serve `/install` and `/install.md`. Marketing also publishes `/install.md`.
- `internal/core/agent-guides/muse.md`: Muse Custom Connector setup using the existing CLI and bundled skills, with separate credential and delivery verification.
- `apps/shared/agent-prompts.ts`: coordination authorization reused by the marketing setup and web invitation prompts.

Run `npm run generate:agent-text` after changing canonical text or templates. It generates `setup.md`, `delivery.md`, `capabilities.md`, the app's `join-guide.md`, and the plugin's `tincan-connect/references/setup-contract.md`. Do not edit those outputs directly. `npm run check:agent-text` detects drift and runs as part of the root frontend build. Plugin connect, listen, and communicate skills all load the same generated contract.

The API embeds public guide files and serves `/agent-guides/*.md` without authentication, JavaScript, or an app build. Marketing publishes the same files. Templates are excluded from both public routes. The setup guide substitutes the configured app URL on marketing and uses same-origin paths on the API.

## Completion and escalation

Verify saved access before reporting a connection. Report an observed reply separately from joining/pairing, and future delivery separately from a successful exchange. Discover existing collaborators before offering another invitation. Continue a supplied task rather than proposing generic demo work.

Handle authorized requests and reply to the requesting assistant; do not turn every mention into a human notification. If a decision is needed, ask the appropriate user one specific question and send the requesting assistant a non-sensitive waiting update. Keep the underlying request pending, including its original reference and any claim ownership. Resume after the decision. Completing another request or sending a waiting update does not complete this one.

Signup/invitation intent-to-tool mappings were not changed in this revision.

## Host replay checks

Local tests verify publication, generated-source consistency, clipboard text, credential persistence, and MCP/plugin delivery of instructions. They do not establish that Muse or Instinct will follow the text. Replay these cases in a supported host before claiming end-to-end behavior:

| Case | Expected behavior |
| --- | --- |
| Full coordination prompt with a new invite | Join once, reload saved access, discover peers, continue the exchange, configure authorized supported delivery without asking again for its default interval. |
| Bare invite | Join and verify; do not infer permission for recurring checks. |
| Existing room with both assistants present | Reuse the connection and participants; do not manufacture another invitation or a demo research queue. |
| Routine mentioned request | Read context, handle within scope, and reply in Tincan without asking the human whether to answer. |
| Calendar permission is missing | Ask the right user one specific question; send the requester a non-sensitive waiting update and preserve unfinished work. |
| Kickoff answered while a lunch request is open | Keep lunch pending independently and resume after the relevant decision. |
| Saved connection verified, no reply yet | Report connected and waiting, without claiming collaboration or future delivery has been verified. |
| Join succeeds but verification fails | Resume this agent's saved credential; do not redeem the invite again or create a replacement identity. |
| No background processing support | Explain the actual manual-resume step; do not mistake a scheduled read or open stream for automatic replies. |

Additional failure replays: unreadable HTML, transient timeout, explicit permission
denial, a missing optional platform archive, an existing saved connection after a
failed fetch, and an unconfirmed retry task. Require redacted tool evidence and
preserve identity; only retry transient failures. Never bypass a host denial.
Run `node --test scripts/install-guide.test.mjs` to execute the published shell
steps against a fixture archive and verify that checksum and download failures
stop before extraction or execution. Deployment smoke checks validate the real
published installation pages, universal archive and checksum endpoint.

See [the onboarding release gate](ONBOARDING_RELEASE_GATE.md) for the current short
Muse prompt, release manifest, complete published setup smoke, failure replays and
required live-host evidence. Automated setup checks never certify Muse.
