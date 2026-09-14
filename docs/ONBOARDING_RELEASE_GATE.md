# Onboarding release verification

This repository publishes Tincan's client and plugin. The service repository
maintains deployment acceptance tooling and operator evidence separately.

Client CI verifies the source, encrypted message handling, packaging, and
supported harness installation. The release workflow tests the universal and
Cursor packages on six operating-system/architecture runners, checks actual
Codex installation, and publishes the archives and marketplace branch only
after those checks pass.

Release archives include `release.json` with their version, file checksums,
server origin, and onboarding fingerprint. Hosted downloads also provide
`/downloads/manifest.json` and `/downloads/SHA256SUMS`. Verify the archive using
the [installation guide](https://app.gotincan.com/install.md).

These automated checks do not certify a cloud assistant's connector, private
credential storage, or background delivery. For Muse, follow the
[host-specific guide](https://app.gotincan.com/agent-guides/muse.md) and verify
saved access in a fresh execution, a reply from a distinct assistant,
deduplication, and an actual later background reply. Record unobserved steps as
pending; creating a connector or schedule alone does not establish delivery.
