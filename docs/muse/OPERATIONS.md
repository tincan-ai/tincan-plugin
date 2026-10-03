# Muse revocation, deletion and incident runbook

## Access and data lifecycle

Use the signed-in owner's **Agents → Connector authorizations → Disconnect** to
stop one OAuth family. This removes its access/session/refresh credentials and
persistent webhook subscriptions. It preserves other authorizations and account
content. A host may alternatively call `/oauth/revoke` through its secure token
flow. Never paste the token in a shell argument, URL, log or support message.

Use **Agents → Delete data**, type the exact agent name and read the affected-data
confirmation. This action is permanent and removes every shared page the agent
contributed to, including other participants' later revisions. It also revokes
all of that agent's connections. The API requires a same-origin, signed-in human
owner session; it is not available as an agent tool. Confirm removal of messages,
vaults, pages, uploads, requests and cached classifications with synthetic data.
Do not use a real user's content in a demonstration.

Database visibility is removed at commit. The existing `blob_deletions` outbox
and `StartBlobCleanup` worker retry physical object deletion at startup and every
30 seconds, including after restarts. A temporary storage failure must leave the
outbox entry intact. Monitor backlog, resolve storage failures and verify actual
object absence. Thirty seconds is a polling interval, not a deletion guarantee.

For account/privacy requests, identify the requester and authority, record the
approved scope and processing basis, revoke relevant grants, use confirmed data
deletion, and coordinate recipient copies where required. Keep a minimal,
restricted deletion record outside restorable application backups. Record the
date, affected pseudonymous identity IDs, completion and any lawful exceptions;
do not retain the removed content in the record.

## Backup restoration gate

Before submission the operator must supply and test a numeric backup expiry
policy, object-version retention, logs/telemetry retention, recipient handling
and authorized access to each system. None is established by repository tests.

Restore only into isolated, inaccessible infrastructure. Replay the separately
retained revocation/deletion records against the restored database, clear any
retained provider caches, process the object deletion queue, and verify that
revoked credentials and removed content remain inaccessible. Do not expose the
restore until the checks pass. Capture a redacted drill with storage configuration,
timestamps, completion and the reviewer; attach its hash to the acceptance record.

## Muse user-data incidents

Owner must name a primary incident owner, backup, monitored escalation channel
and coverage. These assignments remain pending until entered in the submission
record. Do not assume an unmonitored support address provides incident coverage.

1. Record discovery time in UTC and the affected service/release. Start the
   48-hour notification clock. Preserve access-controlled, redacted evidence.
2. Contain the issue: revoke implicated grants/identities, stop affected delivery
   and model processing, restrict exposed routes and rotate implicated service
   credentials through approved secret management. Preserve forensic evidence.
3. Assess affected users, data categories, duration, recipients and ongoing
   access. Confirm containment; verify revocation, deletion and clean restores.
4. Notify `vendor-incident@meta.com` within 48 hours. The assigned owner sends
   the incident/discovery time, known scope, containment and next update/contact.
   Do not include secrets, user content or unverified conclusions. Coordinate
   with Meta and required recipients/users, updating confirmed information.
5. Repair, verify, review root cause, document actions and run a follow-up drill.

Prepare the notification and routing in advance; this runbook does not authorize
the coding agent to send an incident email or make a legal determination.
Record a tabletop drill and evidence of monitored contacts before submitting.
