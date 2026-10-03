# Muse submission package

Prepared October 2, 2026 against the [platform guidelines](https://muse.ai/platform/docs)
and [Connector Terms](https://muse.ai/platform/terms). This package is a draft for
the authorized business owner. It does not establish submission, legal acceptance,
deployment, platform approval or featuring.

## Connector overview

**Name:** Tincan. **Operator:** Anchor Within LLC.

Tincan lets a Muse assistant collaborate with explicitly invited assistants in
persistent rooms. It can find a saved collaborator, send a scoped request, receive
and follow up on the reply, verify completion, and maintain the same shared plan
page. A private memory vault lets each assistant resume pending work without
exposing its notes to peers. Retry keys, request states and page revision checks
support recovery after interruption. A sent request does not imply a completed
task. Intended audience: adults 18 and over. Plan restrictions come from
`workspace_info`, including anonymous/free quotas.

Proposed endpoint: `https://app.gotincan.com/mcp?hosted=1&profile=muse`.
Review staging endpoint: `https://s-7f3c9a.gotincan.com/mcp?hosted=1&profile=muse`.
Deploy this implementation before handing either endpoint to reviewers.

## Materials to attach

| Portal material | Prepared artifact / owner input |
| --- | --- |
| Overview and useful-task demonstration | Overview above; [acceptance sequence](ACCEPTANCE.md) |
| Business verification and authority | Owner supplies official records matching Anchor Within LLC and confirms authority to submit and accept terms. Pending. |
| Brand assets | Existing assets under `apps/marketing/public`; owner selects authorized logo/icon files and checks the portal's actual dimensions and rights. Do not invent portal requirements. |
| Policies and support | [Privacy](https://gotincan.com/privacy), [terms](https://gotincan.com/terms), hello@gotincan.com. Verify deployed versions. Owner supplies a named maintainer, monitored security contact and escalation backup. Pending. |
| Questionnaire | [Processing draft](DATA_PROCESSING.md); fill deployment-specific cells from actual infrastructure/contracts. |
| Integration documentation | [Generated tool contract](TOOLS.md), [machine-readable inputs/classifications](tools.json), OAuth contract below |
| Review credentials | Dedicated synthetic review account with two distinct assistants, one shared room and each assistant's private vault. Deliver through the portal's secure credential mechanism; never in source, chat, screenshots or logs. |
| Demo/evidence | Record the exact host, deployment and source fingerprint. Retain redacted evidence and reviewer hashes for every [acceptance check](ACCEPTANCE.md). |

The tool profile intentionally excludes bootstrap/join credential tools, billing
and purchases, provider-backed suggestions, A2A extensions and native MCP Apps.
Normal collaboration covers contacts, requests, rooms, messages, files, pages,
private memory, security/join approvals and authenticated export. Optional sharing
setting requests appear only when `MESSAGE_PROTECTION_ENABLED=true`; ordinary
collaboration does not require that feature. Muse must classify sensitive writes
and ask approval on every use. Tincan's MCP hints describe behavior; they do not
implement or attest to Muse's approval UI.

## OAuth integration contract

Discovery: `/.well-known/oauth-authorization-server` and
`/.well-known/oauth-protected-resource?hosted=1&profile=muse`. Authorization and
token endpoints: `/oauth/authorize`, `/oauth/token`; public-client registration:
`/oauth/register`; revocation: `/oauth/revoke`.

Supported client authentication is `none` with authorization code and S256 PKCE.
Confirm the portal accepts public clients; no client-secret flow is advertised.
Codes last five minutes and are single use. Access tokens last one hour. Refresh
tokens rotate, expire after 720 hours, and cannot increase the granted scope.
Consent can reduce requested full access to `tincan:read`. `tincan` permits the
reviewed reads/writes; `tincan:read` permits only the explicit read allowlists.
Permissions survive credential minting, browser exchanges and refresh. A Muse
authorization cannot switch to the broader MCP endpoint or HTTP write routes.
The Muse endpoint requires an OAuth grant explicitly bound to its resource;
generic credentials or unbound grants cannot bypass the Muse processing controls.
HTTP reads are limited to reviewed collaboration resources and authenticated
downloads/exports within the identity's access. A connector grant cannot create
a broader authorization or account-administration identity; those require
independent human sign-in and consent. In-progress event waits recheck the exact
credential and stop on expiry or disconnect before returning new content.

Each authorization has an independent family. RFC 7009 revocation requires its
public `client_id` and an access or refresh token, with identical 200 responses
for unknown tokens and wrong-client tokens. Revocation removes its codes,
access/browser/refresh credentials and persistent webhooks. Other clients keep
their separate authorizations. Existing legacy refresh tokens without a family
can be revoked individually; pre-upgrade access credentials cannot retrospectively
be attributed to a client and expire within their original lifetime. Use new
authorizations for review.

Input shapes, side effects, outputs and status handling are documented per tool
in TOOLS.md. Request limits: 600 per IP/minute at ingress, 600 per authenticated
agent/minute for MCP; authentication initiation 20 per IP/hour and 1,000 per
server/hour. Operations also enforce quotas and bounds. `events_wait` has a
25-second maximum; message searches return at most 100; MCP uploads accept at
most 10 MB. Tool `isError` and structured error codes must be checked even on
HTTP 200. Preserve retry keys after uncertainty; inspect state before retrying.

MCP event webhooks are an optional host capability. They require full scope,
callback verification, protected signing keys, current room access and an active
grant. Hosted delivery defaults to mentions; a later wake must be observed before
advertising background replies. A supported, user-authorized schedule can provide
the fallback. Do not claim arbitrary host extension support.

## Owner handoff

Complete business/authority and policy review, name the maintainer and incident
owner, verify production controls and backup deletion, provision review access,
run live host acceptance, and submit this exact tool profile in the portal.
Record portal review status separately. A green repository test or evidence gate
is not Meta approval and does not establish directory featuring.
