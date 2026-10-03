# Muse platform readiness

Audit and implementation date: October 2, 2026. Sources:
[Connector guidelines](https://muse.ai/platform/docs),
[Connector Terms](https://muse.ai/platform/terms) (effective September 18, 2026),
and the [submission portal](https://muse.ai/platform). These were read in the
browser; account-specific intake and approval status were not inspected.

**Engineering requirements implemented; deployment and owner/host evidence remain pending.**
This repository is not an attestation of legal compliance, Muse approval or
featuring. Existing compatibility evidence does not replace platform review.

## Requirement assessment

| Requirement | Implemented evidence / remaining verification |
| --- | --- |
| Complete useful tasks, beyond browser links (§1) | Stable Muse tool profile includes contacts, rooms, messages, files, requests, follow-ups, verified status, shared page revisions and private memory. Demonstrate the complete distinct-assistant collaboration in Muse. |
| Optional read-only OAuth (§3.1) | Consent offers `tincan:read`; persisted across codes, access/session/refresh credentials. Explicit HTTP and MCP allowlists reject writes and persistent webhooks. Scope cannot escalate on refresh or credential substitution. Local integration tests cover each path. |
| Accurate classifications (§3.2) | Read annotations use an explicit allowlist, unknown tools default to write, and mixed/provider operations remain writes. The credential-free Muse allowlist and generated contract document read/write/sensitive-write behavior. Meta review labels and live permission mapping remain pending. |
| Approval on each sensitive write (§3.3–3.4) | Guide and submitted contract require approval on each use and after changed details, including schedules. Local annotations do not implement host approval. Actual Muse prompts, denials/no effects, reapproval and peer-injection behavior require observed evidence. |
| Terms/authority/truthful claims (§4.1, 4.3, 4.5) | Submission overview and pending evidence gate distinguish compatibility, submission, approval and featuring. Authorized owner must review/accept terms and verify business authority. |
| Data minimization/permitted purposes (§4.2) | Identity/room ACLs, agent vaults and bounded outputs apply. Initial profile excludes provider suggestion tools, A2A/UI extensions and billing. `MUSE_MODEL_PROCESSING_APPROVED=false` blocks provider evaluations throughout Muse-connected workspaces, including after disconnect. Questionnaire and public policy updated; deployed recipients, analytics, contracts and no-training commitments need owner verification. |
| Revocation and deletion (§4.2, 4.4) | Independent OAuth families with RFC 7009 revocation remove access/session/refresh credentials and their webhooks. Owner UI manages disconnect and confirmed data deletion. Deletion clears authored content/vaults/uploads/requests, affected pages and caches; durable object cleanup retries. Local tests verify client isolation, access removal and object absence. Backup expiry, restore controls and recipient handling remain operational evidence. |
| Security (§4.4) | S256 PKCE, single-use codes, rotating refresh, fixed token/session lifetime, profile binding, room ACLs and internal-error redaction are implemented and tested. Verify actual production TLS, encrypted storage, least privilege and monitoring. |
| Financial/transactional tools (§4.6–4.7) | Billing/purchases/trades are excluded from the Muse profile; grants cannot expand into generic MCP or HTTP write routes. Any future transactional extension requires a separate reviewed scope. |
| Incidents (§4.8) | Runbook includes containment, investigation, owner/backup and notification to vendor-incident@meta.com within 48 hours. Owner assignments, monitored coverage and a drill remain pending. |
| Overview/business materials (§5.1–5.2) | Overview, public operator/policy/support links and authorized asset selection instructions prepared. Owner supplies matching business records, brand rights, named maintainer and monitored security contacts. |
| Processing questionnaire (§5.3) | Code-backed draft includes data, purposes, recipients, optional models/analytics, retention, deletion, copies, backup restore and incidents. Deployment-specific cells are explicitly pending. |
| Integration/tool docs (§5.4, 5.6) | Generated input schemas, outputs/status, classifications, side effects, errors, quotas, feature gates and OAuth lifecycle are in the submission package. CI detects stale contracts; actual deployed tools must match. |
| Review account/demo (§5.5) | Synthetic two-assistant review sequence and secure credential handoff instructions prepared. Owner must provision and keep dedicated review access available. |
| QA/review/discovery (§5.7–7) | Local database tests and evidence validator implemented. Live acceptance has 22 mandatory checks and rejects pending, stale or changed-source evidence. Actual Muse execution and portal approval remain pending. |

## Submission artifacts

- [Submission overview, OAuth contract and handoff](muse/SUBMISSION.md)
- [Generated per-tool review contract](muse/TOOLS.md) and [JSON schemas/classifications](muse/tools.json)
- [Data-processing questionnaire draft](muse/DATA_PROCESSING.md)
- [Disconnect/deletion, backup restoration and incident runbook](muse/OPERATIONS.md)
- [Live acceptance sequence, test commands and evidence gate](muse/ACCEPTANCE.md)

Proposed production endpoint: `https://app.gotincan.com/mcp?hosted=1&profile=muse`.
Review staging: `https://s-7f3c9a.gotincan.com/mcp?hosted=1&profile=muse`.
Deploy the implementation before review. The stable profile preserves complete
collaboration capabilities while requiring an explicit reviewed change before
any generic tool enters the Muse scope. Read-only grants expose only reads.
Authenticated export/download remains available within existing room access.

Public-client registration uses `token_endpoint_auth_method=none`, S256 PKCE,
rotating refresh and revocation; no confidential-client secret flow is claimed.
Verify that this configuration is accepted by the actual portal. Hosted tools
omit credential bootstrap/join and the private connection argument. Keep review
credentials in the portal's secure flow, out of source/chat/logs/results/URLs.

## Verification and remaining handoff

Run `npm run test:muse-platform` for isolated core, HTTP and migration suites
with race detection, `npm run check:muse-tools` for generated-contract freshness,
`npm run test:muse-evidence` for fail-closed acceptance validation, and the normal
build/onboarding checks. Synthetic tests establish server behavior; they cannot
establish host approval prompts or a production deployment.

Run the real collaboration, permissions and data-lifecycle sequence from
ACCEPTANCE.md. Record redacted evidence hashes, exact deployment/host/account
policy and the named reviewer. A later wake or scheduled run must actually
process work before advertising background replies. Native UI/extensions and
provider-enabled enhancements need separate host/processing review.

Remaining owner inputs are deployed security/retention and processing evidence,
backup expiry/restore and downstream-copy handling, business authority/brand
rights, monitored contacts/incident ownership, terms acceptance, review access,
actual Muse permissions/demo observations and portal submission. The repository
prepares these steps without accepting terms, sending incident email, creating
review credentials or asserting unobserved approval. The pending evidence gate
must fail until observations exist.
