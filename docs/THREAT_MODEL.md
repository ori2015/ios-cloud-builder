# Threat model

## Security goals

Central mode is designed so a public repository never commits private application source or private repository mappings, uploads a plaintext private IPA or detailed build log, or keeps a persistent private-repository credential in code. Public dispatch carries only a random build ID, an opaque random project ID, operation, and public AGE recipient. The secret-backed registry resolves private metadata inside trusted code and masks it before native Actions receive it. The local CLI pushes the working tree only to a derived temporary ref in the private source repository. A repository-scoped GitHub App installation token performs the private checkout. Only AGE ciphertext is uploaded. Optional Apple signing material and its transport AGE identity exist only as secrets of the protected `apple-production` Environment.

## Trust boundaries

- The local workstation and its AGE identity are trusted.
- The private source repository and its write-authorized operator are trusted to initiate builds.
- GitHub's runner and Actions control plane necessarily see plaintext while the job runs.
- Public Actions logs, artifacts, caches, inputs, summaries, annotations, and public pull requests are untrusted/public territory.
- Maintainers with write/admin access to the public builder are highly trusted. They can modify the workflow or helper and can dispatch builds. Keep this group minimal.
- Private application build scripts and dependencies execute arbitrary code on the runner. Environment scrubbing prevents accidental access to known Actions channels and tokens, but is not a sandbox and cannot prevent all network exfiltration by a malicious project.
- The TestFlight signing job trusts the reviewed public-builder revision, GitHub Environment controls, GitHub Actions/Sigstore control plane, Apple signing material, and the authenticated unsigned IPA produced by the isolated `trusted-package` job. It never checks out or executes private project source.

## Threats and mitigations

### Malicious public pull request

The private-source workflow is `workflow_dispatch` only. PR CI has read-only permissions and no central secrets. External actions are pinned to full commit SHAs. CODEOWNERS identifies security-sensitive paths. Default-branch protection and required review must also be enabled in repository settings.

### Workflow modification or hostile maintainer

An authorized writer could change the trusted helper, select a private repository installed for the App, or substitute their own AGE recipient. Protect `main`, require review for `.github/workflows/ios-build.yml`, `internal/runner`, `internal/security`, and central coordination code, and audit dispatches. Repository administrators remain trusted because they can bypass repository controls.

For TestFlight, restrict `apple-production` to the protected default branch. The
deployment starts automatically after trusted packaging, so branch protection,
CODEOWNERS review, minimal repository write access, and dispatch auditing are
the controls against unauthorized workflow changes. A compromised authorized
dispatch path can cause an automatic TestFlight upload; this is an accepted
single-operator tradeoff. Environment controls cannot defend against a
repository administrator who can change or bypass them.

### Stolen GitHub App private key

The App has only Metadata read and Contents read, and is installed using **Only select repositories**. Every workflow requests a token for exactly one validated repository. Rotate the App key and review installations immediately after suspected compromise. A stolen key can read every selected repository until revoked; scoping a single job does not change the App installation's overall blast radius.

### Token persistence and project-code access

Both checkouts use `persist-credentials: false`. The App token is passed only to the private checkout and is explicitly revoked before dependency or build code runs. The build helper removes GitHub/Actions token and file-command variables from child environments. The workflow must never print environment variables, remotes, tokens, or secret values.

Snapshot files approaching GitHub's blob limit are split into bounded chunks by the local CLI. After token revocation, the trusted runner validates the versioned manifest, relative paths, file and chunk sizes, SHA-256 digests, duplicate paths, symlink-free parents, and aggregate limits before reconstructing those files. It removes the reserved transport namespace before any private project tooling executes.

### Input, expression, path, and shell injection

The trusted Go helper validates UUID, opaque project ID, registry schema and authorization, owner, repository, derived exact snapshot ref, relative iOS path, scheme, configuration, framework enum, and X25519 recipient before token minting. Private paths are resolved after checkout and must remain inside the source root. Build processes receive fixed argv arrays; there is no `eval`, arbitrary command, arbitrary script, or generic shell input.

### Artifact, log, and cache disclosure

Compiler/dependency output starts redirected into a private build log. Both IPA and log are AGE-encrypted before upload, plaintext files are deleted, and the artifact step uses an exact ciphertext allowlist with one-day retention. The CLI binds the artifact to the exact run/build UUID, decrypts locally, validates IPA structure, and attempts remote artifact deletion. Central mode uses no Actions cache and never uploads DerivedData, dSYMs, archives, source, or plaintext diagnostics.

AGE protects artifact confidentiality and integrity after encryption. For
TestFlight, authenticity is added by a separate hosted-runner job that receives
only encrypted project output, holds a distinct packaging AGE identity, and
executes no project code. It validates/copies the app, creates and hashes the
exact IPA, encrypts it, and emits a private-name-free manifest. GitHub Sigstore
attests both exact files with job-scoped OIDC. Only this job has `id-token` and
`attestations` write permissions.

In TestFlight mode project output is first encrypted to the packaging recipient.
The authenticated unsigned IPA is then encrypted to a second transport
recipient whose identity is available only inside `apple-production`. Before
that identity or any Apple value is injected into a step, the protected job
rejects extra files, symlinks, missing files, manifest/ID/operation/commit/ref
mismatches, ciphertext changes, non-hosted provenance, or invalid Sigstore
signatures. After decryption it checks the authenticated plaintext digest before
reading P12/profile/ASC credentials. It also rejects traversal, special files,
multiple apps, and embedded applications requiring extra profiles. It
sets a GitHub-run-derived `CFBundleVersion`, signs without executing the app,
validates the signed IPA with App Store Connect, and uploads it directly to Apple
before deleting it with the ephemeral runner; only an AGE-encrypted diagnostic log is
uploaded to GitHub.

### Apple credential compromise

The distribution certificate, its password, provisioning profile bundle, App Store
Connect key, and transport AGE identity are Environment secrets. Secret values
are removed from the trusted runner's environment before child processes start,
sensitive command arguments are not written to diagnostics, and credential
files/keychains live only under runner-temporary paths. Exact default-branch
restriction is a mandatory operational control. Revoke Apple or AGE
credentials immediately after suspected disclosure; encryption at rest does not
repair a previously exposed credential.

### Concurrent build confusion

Build IDs are full random UUIDs and project IDs are independent random 128-bit handles. Run lookup uses the exact run title and dispatch workflow, and artifact lookup is restricted to that run with the build ID in its exact name. Manifest binding includes build ID, project ID, operation, builder commit, and workflow ref. The CLI and protected runner reject unexpected ZIP/artifact members and validate decrypted IPA structure, preventing concurrent-build or cross-project artifact confusion.

### Snapshot leaks and stale refs

Snapshots live only under `refs/ios-builder/jobs/<private-namespace>/<uuid>` in the configured private repository. The random per-project namespace is stored in the private caller configuration and protected registry, never in public dispatch inputs, so a public build UUID does not reveal the actual ref. Creation uses an alternate Git index, respects `.gitignore`, and leaves branch/index/working tree untouched. Cleanup uses the observed SHA as a lease so it cannot delete a replaced ref. `builder cleanup` removes refs older than 24 hours. An abrupt process kill can still prevent immediate cleanup; scheduled/manual cleanup remains necessary. Untracked non-ignored secrets are included by design, so projects must maintain `.gitignore` carefully.

### Dependency and supply-chain compromise

Pinned Actions reduce tag-retargeting risk. Package managers and private project dependencies still execute code and may contact networks. Because they run as the hosted-runner user, intentionally malicious project code can deliberately exfiltrate its own source, race pre-boundary files, or try to write runner file-command paths; environment allowlisting is not an OS sandbox. The clean `trusted-package` VM prevents that code from modifying the final attested ciphertext or manifest, but provenance proves exactly what crossed that boundary—not that the source or dependency graph was semantically benign. Lockfiles, dependency review, provenance controls, and minimal private-project install scripts remain the source project's responsibility. No dependency cache crosses builds.

### Metadata disclosure

The public workflow schema exposes only build ID, opaque project ID, operation,
and the caller's public AGE recipient. The secret registry contains private
repository and build settings. Trusted resolution registers GitHub masks for
owner, repository, owner/repository forms and URLs, derived snapshot ref, iOS
path, and scheme before token creation or checkout. Build diagnostics containing
workspace/project names or Bundle IDs remain encrypted. Framework category can
appear because public setup actions are selected from it. GitHub administrators
and the Actions control plane remain able to inspect secret values by design;
the goal is unauthenticated/public metadata privacy.

If a private source repository and the public builder use the same GitHub
account, that account name is inherently visible in the public builder URL and
its own checkout metadata. Masking can hide the resolved private-repository use,
but cannot make the public builder's owner anonymous. Use a distinct public
builder account when owner-name privacy is required; repository name and the
private `owner/repository` pair must still have zero public occurrences.

| Value | Classification | Handling |
|---|---|---|
| Build ID, opaque project ID, operation, caller AGE recipient | Required public correlation/transport | Four-field dispatch schema |
| Owner, repository, URL, private snapshot namespace/ref | Must remain private | Secret registry; masked before token/checkout actions |
| iOS path, scheme, configuration | Must remain private | Registry outputs masked before use; detailed tool output encrypted |
| Workspace/project name and Bundle ID | Must remain private where avoidable | Inferred only after checkout; diagnostics encrypted |
| Framework category | Residual, potentially inferable | Resolved after checkout; selected public toolchain setup can reveal the category |
| Artifact names | Generic public metadata | Random build ID only; no repository/project naming |

## Explicit non-goals

- Protecting source from GitHub's runner/control plane.
- Safely building intentionally malicious private projects in a strong sandbox.
- Automatic provisioning and multi-profile signing for extensions, Watch apps, App Clips, or XPC services.
- Hiding Apple credentials from GitHub's protected signing runner/control plane.
- Guaranteeing GitHub policy approval; see [COMPLIANCE.md](../COMPLIANCE.md).
