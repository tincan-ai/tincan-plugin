# Muse data-processing questionnaire draft

Fill this from the deployed release and provider agreements. Every **pending**
cell requires owner verification before submission. Do not copy assumed regions,
retention periods, encryption settings or no-training commitments into the portal.

| Topic | Implemented behavior / required deployment answer |
| --- | --- |
| Operator and role | Anchor Within LLC operates Tincan. Owner/legal reviewer confirms Muse-specific controller/processor roles and the accepted platform terms. Pending. |
| Data collected | Authorized identity/profile, OAuth client/grant and hashed credentials, room membership, messages, request state, shared page revisions, attachments, private vault entries, private contact notes and operational/usage metadata. Tools return only data allowed by identity and room access. |
| Purposes | Requested agent collaboration, continuity/private memory, storage/search/export, access control, quota accounting, service reliability and incident response. No purchases/trades/payment tools are submitted. Owner attests that operational processing stays within permitted connector purposes and excludes sale, unrelated advertising/profiling and general-purpose model training. Pending. |
| Participants | Only authorized participants in a shared room receive its content. Private vaults/contact notes remain scoped to their owning identity and authorized human account access. Other participants may retain content they previously received. |
| Hosting/database/storage | Owner lists deployed providers, regions, account access, encrypted storage/transport configuration, agreements, retention and subprocessors. Pending. Source code does not prove infrastructure settings. |
| Authentication | OAuth credentials stay in the host's private credential flow; Tincan stores credential hashes. Google/optional Facebook identity processing applies when chosen, as described in the privacy policy. Do not include review credentials in this questionnaire. |
| Analytics | Local activation/usage accounting exists. Optional PostHog export sends bounded pseudonymous milestones and sanitized browser diagnostics, excluding conversation content, prompts, credentials and vault entries. Owner verifies connector-purpose compatibility, recipients, residency and retention; disable export if not verified. Pending. |
| Model providers | Initial submission keeps `MUSE_MODEL_PROCESSING_APPROVED=false`. Server evaluation is blocked for an entire workspace with a Muse profile authorization, even after disconnect, preventing peer context from exporting Muse data through those features. Sharing checks fall back to human review. Any later enabling requires separately reviewed recipient purposes, retention, deletion, no-training terms, disclosures, feature flags and owner opt-ins. TypeSafe/OpenRouter are possible configured recipients, not automatically approved recipients. |
| Retention | Content remains available until authorized deletion or other retention controls apply. Owner records actual active-data, queue, logs, telemetry, object storage and backup periods and any legal/accounting exceptions. Pending. No numeric production backup period is established by this repository. |
| Disconnect/revocation | `/oauth/revoke` or the signed-in owner's Agents panel revokes one family, access/refresh/session tokens and its persistent delivery. Already transmitted peer/provider copies are outside that authorization. Disconnect alone does not delete retained account content. |
| Deletion | Owner types an agent's name in the Agents panel. Its identity is revoked, profile cleared; credentials, private vault, authored messages/uploads/requests, contact data and every shared page it contributed to are removed. Entire affected pages are removed because later revisions may preserve copied text. Semantic caches in the workspace are cleared. Database access disappears at commit; object deletion is durably queued and retried. |
| Retained records | A revoked identity tombstone, minimal workspace Muse-processing marker and aggregate usage/financial records can remain for references/accounting and preventing unverified processing. The processing gate stays closed after data deletion. Other participants' independently authored content or copies they received are not automatically erased. Owner defines lawful grounds and periods for retained records, subject requests and downstream deletion requests. Pending. |
| Backups and restore | Operator must document expiry and ensure deletion/revocation records are reapplied before any restored backup is served. Perform a restoration/deletion drill. Pending. See [operations](OPERATIONS.md). |
| Requests/support | Existing privacy request route: hello@gotincan.com. Verify requester identity/authority, record decisions, delete or make data inaccessible, and coordinate with relevant recipients as required. Owner sets response targets and escalation coverage. Pending. |
| Security/incidents | PKCE, bounded sessions, grant isolation, room ACLs and redacted application errors are implemented. Owner verifies TLS, at-rest encryption, least-privilege production access, monitoring and actual incident readiness; notify vendor-incident@meta.com within 48 hours of a Muse user-data incident. Pending. |

Supporting tests: `TestMuseReadOnlyGrantSurvivesEveryCredentialPath`,
`TestMuseOAuthRevocationIsolatesClientAndStopsSessions`,
`TestMuseReadOnlyCannotCreateWebhooksAndRevocationStopsOnlyItsDelivery`,
`TestMuseDisconnectStopsAnEventWaitAlreadyInProgress`,
`TestMuseAgentDeletionRequiresTheOwnerBrowserAndCleansFiles`,
`TestMuseAgentDataDeletionIsConfirmedAndIsolated`, and
`TestMuseModelProcessingRequiresVerificationForEveryWorkspaceMember`.
These test implementation behavior on synthetic local data; they do not attest
to a production deployment or third-party processing agreements.
