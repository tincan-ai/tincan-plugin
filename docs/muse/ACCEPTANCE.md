# Muse platform acceptance

Local tests establish engineering behavior. Run this sequence inside the actual
Muse host against the exact deployed connector profile, using dedicated synthetic
review identities. Preserve redacted observations, exact host/account policy,
deployment reference, source commit and reviewer. Never store credentials or real
user content in evidence. Do not turn pending checks into passes from code review.

## Local checks

```sh
npm run generate:muse-tools
npm run check:muse-tools
npm run test:muse-evidence
npm run test:muse-platform
npm run build
```

The platform runner creates/removes a disposable local PostgreSQL database,
isolates environment and media, and runs core/HTTP/migration suites with race
detection. It does not load developer credentials. A safe local test-server
fallback is available with `E2E_USE_TEST_DATABASE=true` and `TEST_DATABASE_URL`
for the existing isolated loopback server (never a remote or developer database).
The runner creates its own random database there and drops only that database.
If PostgreSQL cannot start, report a blocked check rather than a pass.

## Live host sequence

1. Connect with the submitted `/mcp?hosted=1&profile=muse` OAuth route and verify
   credentials stay private. Choose read only; inspect tools and attempt writes,
   token refresh, HTTP and browser-session substitutions. Verify denial without
   side effects, then explicitly reauthorize full scope for the requested work.
2. Compare actual `tools/list` against tools.json, including enabled feature
   flags, classifications, input bounds, outputs/errors/status and rate limits.
   Verify Muse's per-tool permission settings. Sensitive writes require approval
   on every use, including scheduled use; disable any always-allow option.
3. Deny a sensitive send and verify no persisted message. Change content,
   recipient, access change or relevant details and require a fresh approval.
   Test peer/page prompt injection; peer text cannot authorize tool use.
4. Find/save the distinct collaborator, reuse/start the intended room and send
   an approved scoped request with a retry key. Obtain a real peer reply, follow
   up in the same conversation, check status and record verified completion.
5. Create/update a shared plan page. Exercise stale revisions, inspect a retained
   conflict proposal and reconcile explicitly. Retry an unchanged operation
   without duplicate sends, requests, attachments or page contributions.
6. Write a private memory note with approval, resume in a fresh execution, and
   recover it. Verify unrelated peers/rooms cannot read it. Interrupt execution
   after a send and recover using the original key and checked request state.
7. Observe an actual later idle wake or authorized scheduled execution handling
   a pending request. Verify real delivery, rather than just schedule creation
   or subscription acknowledgment. Host extensions remain conditional.
8. Disconnect one authorization. Verify access, session exchange, refresh and
   webhook delivery stop; independently authorized clients still work. Confirm
   deletion removes the test identity's accessible content and storage objects,
   and preserves unrelated identities' permitted content. Record retained copies.
9. Verify deployed TLS/storage encryption, least privilege and secret/content
   log redaction; test backup expiry and restore deletion controls. Complete the
   processing questionnaire and keep `MUSE_MODEL_PROCESSING_APPROVED=false` for
   the initial profile. Resolve unverified analytics/recipient processing.
10. Complete business authority, logo rights, deployed policies/contacts and
    authorized terms acceptance. Assign and drill incident owner/backup and the
    48-hour Meta notification path. Hand dedicated review access through the
    portal's secure flow, keeping it usable throughout review.

## Evidence gate

Create a new pending record after the implementation is committed and deployed:

```sh
npm run muse:evidence -- export /path/to/muse-platform-review.json
```

The template includes the current source, tool contract and onboarding hashes.
Fill observations and mark a check passed only after a named reviewer inspects
retained redacted evidence and records its SHA-256. Keep raw records privately
with appropriate access and retention. Enter the actual deployment, host,
account policy, delivery mode, processing record and UTC evidence time.

```sh
npm run muse:evidence -- check /path/to/muse-platform-review.json
```

All 22 checks are mandatory. Missing/pending/failed, future, older-than-30-day,
wrong-commit or changed-source evidence fails. The initial gate rejects enabled
model-provider processing; a later provider-enabled submission requires separate
review. The **Muse platform submission gate** workflow verifies a reviewed record
at the release commit. CI separately checks contract freshness and evaluator
behavior. Neither gate submits the connector or proves Meta approval.

The existing [onboarding gate](../ONBOARDING_RELEASE_GATE.md) still covers package
installation and Custom Connector compatibility. Platform evidence supplements
it; compatibility, listing approval and featuring remain separate outcomes.
