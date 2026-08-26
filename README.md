# iOS Cloud Builder

Build unsigned iOS applications, or deploy signed releases to TestFlight, from Linux, WSL, or Windows using a narrowly scoped remote macOS build. This is a truthful open-source remote-build/orchestration project derived from [MobAI-App/ios-builder](https://github.com/MobAI-App/ios-builder), not a generic compute service or a disguised workload.

The central backend lets multiple private application repositories use one public builder repository without committing private source or publishing plaintext output:

```text
private app working tree
  -> temporary private refs/ios-builder/jobs/<private-namespace>/<uuid>
  -> public dispatch with opaque project ID only
  -> secret registry resolution + immediate metadata masks
  -> repository-scoped GitHub App checkout
  -> unsigned iOS build with private console redirection
  -> AGE-encrypted IPA + log artifact
  -> local download, decryption, IPA validation, and cleanup
```

The optional TestFlight path adds two clean hosted-runner boundaries. The build
job encrypts untrusted project output to an isolated packaging job. That job
executes no project code, recreates the IPA under a trusted temporary root, and
uses GitHub Sigstore to attest the exact ciphertext and provenance manifest.
The `apple-production` signing job starts automatically, verifies that
attestation before any Apple credential is injected, then signs and uploads
directly to App Store Connect. GitHub stores no signed IPA artifact.

The original repository backend remains available for existing MobAI users and repository-local builds. Simulator sharing, MobAI integration, Flutter/React Native/KMP development commands, framework detection, working-tree snapshots, signing tools, and public Go wrappers are retained. See [the upstream relationship](docs/UPSTREAM.md).

> [!IMPORTANT]
> GitHub's hosted-runner terms contain a repository-association limitation, and GitHub has not explicitly approved this exact cross-repository architecture. Read [COMPLIANCE.md](COMPLIANCE.md) before using the central hosted-runner backend. The backend boundary is intentionally replaceable by a self-hosted Mac, Codemagic, or another macOS CI implementation.

## Security properties

- Private source is pushed only to the private source repository, under a temporary non-branch ref.
- The GitHub App has Metadata read and Contents read only and is installed on explicitly selected repositories.
- The build job requests an installation token for exactly one registry-authorized source repository, checks out with `persist-credentials: false`, then revokes the token before project code runs.
- Unsigned build remains the default; Apple credentials are available only to the branch-restricted `apple-production` job.
- The signing job never checks out private source and never runs project scripts or dependencies.
- Detailed dependency/compiler output is redirected to a private log from process start.
- Build logs and locally downloaded IPAs are encrypted to the caller's local-only AGE identity. TestFlight uses distinct build-to-package and package-to-signing AGE identities.
- Public artifacts contain exact allowlists of ciphertext/manifest files, are retained for one day, and are deleted early when the caller completes cleanup. No plaintext or signed IPA is uploaded.
- Central mode creates no Actions caches and uploads no DerivedData, dSYM, archive, source, or plaintext diagnostics.
- The public schema contains only build ID, opaque project ID, operation, and public artifact recipient. Registry values are validated and masked before credential creation; build commands use fixed argv arrays, never `eval` or user-provided scripts.
- Full UUIDv4 correlation plus authenticated project ID, operation, builder commit, and workflow ref bind the TestFlight artifact to one trusted packaging execution.

These controls protect against accidental public disclosure; they do not sandbox intentionally malicious private project code or hide plaintext from GitHub's active runner. Read the full [threat model](docs/THREAT_MODEL.md).

## Prerequisites

Local workstation (Linux/WSL/macOS; Windows through PowerShell/WSL):

- Git 2.30+
- GitHub CLI (`gh`) authenticated to the source and builder repositories, recommended
- A Git remote using an explicit `github.com` HTTPS or SSH URL
- Builder CLI

The public builder workflow uses the stable `macos-15` runner image. No local Mac, Xcode, certificate, or provisioning profile is required for an unsigned central build. TestFlight deployment requires an Apple Distribution certificate, one or more App Store distribution provisioning profiles, and an App Store Connect API key.

## Install the CLI

From a published release:

```bash
curl -fsSL https://raw.githubusercontent.com/ori2015/ios-cloud-builder/main/install.sh | bash
```

Or build from source with Go 1.24 or newer:

```bash
git clone https://github.com/ori2015/ios-cloud-builder.git
cd ios-cloud-builder
go build -o builder ./cmd/builder
install -m 0755 builder ~/.local/bin/builder
```

Windows users can download `builder-windows-amd64.exe` from Releases and place it on `PATH`.

Authentication resolution order is `BUILDER_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`, an authenticated `gh`, then the legacy Builder credential store. Tokens are never written to `builder.json` or logged. Usually this is enough:

```bash
gh auth login
gh auth status
```

The legacy device flow remains available for repository-mode compatibility:

```bash
builder auth github
```

## One-time public builder setup

This repository itself is the public builder. Fork it publicly without rewriting history, retain `.github/workflows/ios-build.yml`, and restrict write access to trusted operators.

Create a dedicated GitHub App in **Settings -> Developer settings -> GitHub Apps -> New GitHub App** with these exact settings:

| Setting | Value |
|---|---|
| GitHub App name | `ios-cloud-builder-<your-account>` (globally unique) |
| Homepage URL | URL of your public builder repository |
| Webhook | Inactive |
| Repository permissions: Contents | Read-only |
| Repository permissions: Metadata | Read-only (implicit) |
| Every other repository permission | No access |
| Organization permissions | No access |
| Where can this GitHub App be installed? | Only on this account |

Generate one private key. In the **public builder repository**, configure:

```text
Repository variable  APP_CLIENT_ID   = GitHub App client ID
Repository secret    APP_PRIVATE_KEY = complete generated PEM private key
```

Install the App using **Only select repositories** and choose only private applications this builder may read. Do not grant Actions, Issues, Pull requests, Administration, or write permissions. Delete the downloaded PEM after the repository secret is verified, or retain a recovery copy only in an appropriate secrets manager. Never commit it.

Protect the builder's default branch, require review for CODEOWNERS paths, keep administrators minimal, and leave the default workflow token permission at read-only.

## Optional protected TestFlight setup

Create a GitHub Environment named `apple-production` in the **public builder**.
Do not configure required reviewers or a wait timer. Restrict deployment to the
protected default branch and do not allow unreviewed workflow changes. After a
successful trusted packaging job, signing and TestFlight deployment start
automatically. Apple credentials remain scoped to the Environment and are made
available only to the isolated signing job.

Generate two dedicated AGE identities: one for the untrusted build-to-package
transport and one for package-to-signing transport. Put the first public
recipient in repository variable `PACKAGING_RECIPIENT` and its identity in
repository secret `PACKAGING_AGE_IDENTITY`. Put the second public recipient in
`APPLE_SIGNING_RECIPIENT` and its identity only in the protected Environment
secret `APPLE_SIGNING_AGE_IDENTITY`. These identities must be distinct from one
another and from every caller's local artifact identity.

The isolated `trusted-package` job has no private checkout and executes no
project code. It decrypts and validates project output, copies the `.app` into a
fresh temporary root, creates the exact unsigned IPA, hashes plaintext and
ciphertext, and emits `provenance.json`. GitHub's pinned `actions/attest` action
then signs both ciphertext and manifest using a short-lived Sigstore certificate
issued from that job's OIDC identity. No long-lived provenance signing key is
required. The protected signing job verifies repository, workflow, commit, ref,
hosted-runner origin, manifest fields, artifact allowlist, and both digests
before its Apple-secret step can run.

Put the Apple transport identity's
public recipient in the repository variable `APPLE_SIGNING_RECIPIENT`; put the
identity itself only in the Environment secret `APPLE_SIGNING_AGE_IDENTITY`.
Never reuse the caller's local AGE identity and never commit the transport
identity. For example, on a trusted workstation with `age-keygen` installed:

```bash
umask 077
age-keygen -o /tmp/apple-signing.agekey
age-keygen -y /tmp/apple-signing.agekey
```

Copy the printed `age1...` recipient into the repository variable. Copy the
complete `AGE-SECRET-KEY-...` line into the Environment secret, then securely
remove the temporary file.

Configure these values in `apple-production`:

| Kind | Name | Format |
|---|---|---|
| Environment variable | `APPLE_TEAM_ID` | 10-character Apple Team ID |
| Environment secret | `TESTFLIGHT_BETA_GROUPS` | optional JSON map of Bundle IDs to existing beta groups; secret-scoped so GitHub masks the map before the signing step starts |
| Environment secret | `APPLE_SIGNING_AGE_IDENTITY` | dedicated AGE identity |
| Environment secret | `APPLE_DISTRIBUTION_P12` | base64-encoded `.p12` |
| Environment secret | `APPLE_DISTRIBUTION_P12_PASSWORD` | `.p12` password |
| Environment secret (optional fallback) | `APPLE_PROVISIONING_PROFILES` | base64-encoded ZIP of App Store `.mobileprovision` files |
| Environment secret (legacy optional fallback) | `APPLE_PROVISIONING_PROFILE` | base64-encoded single App Store `.mobileprovision`; used when the bundle secret is absent |
| Environment secret | `ASC_API_KEY_P8` | complete `.p8` PEM or its base64 encoding |
| Environment secret | `ASC_KEY_ID` | App Store Connect API key ID |
| Environment secret | `ASC_ISSUER_ID` | App Store Connect issuer UUID for a Team key; omit for an Individual key |

The central TestFlight path requires a Team App Store Connect key with
`ASC_ISSUER_ID`. Inside the protected `apple-production` job, the signer lists
active App Store profiles for the exact Bundle ID, verifies them against the
imported distribution-certificate fingerprint, and creates an `IOS_APP_STORE`
profile through Apple's API when none is usable. The downloaded profile exists
only in the disposable signing workspace. Stored profile secrets remain optional
fallbacks for API incidents or migrations. The Team key applies across apps, so
keep the exact Environment branch allowlist in place.
Revoke and replace any certificate, API key, or GitHub App key that was ever
committed, pasted into a public log, or otherwise exposed; moving an exposed
credential into a secret does not make the old credential safe.

If API-managed provisioning is temporarily unavailable, create the optional
multi-application profile bundle on a trusted workstation. Filenames are only
labels: the protected runner selects a profile from its signed
`application-identifier` entitlement and requires an exact Bundle ID match.
Keep only App Store distribution profiles for `APPLE_TEAM_ID` in this ZIP.

To wait for processing and publish selected applications to existing TestFlight
groups, configure the `TESTFLIGHT_BETA_GROUPS` Environment secret in
`apple-production`. It must be a secret rather than an Actions variable because
GitHub renders step environment variables before in-step masking can run. Both
immutable group ID and public-link ID are required and verified before mutation:

```json
{
  "com.example.app": {
    "group_id": "00000000-0000-0000-0000-000000000000",
    "public_link_id": "abcd1234",
    "submit_beta_review": true
  }
}
```

Applications absent from the map retain upload-only behavior. Configured builds
are selected by exact app, marketing version, and build number; the runner waits
up to 40 minutes for Apple processing, updates What to Test, attaches the build
additively, preserves the public link, and submits external builds for Beta App
Review when prerequisites are complete.

```bash
umask 077
zip -j /tmp/apple-provisioning-profiles.zip /trusted/profiles/*.mobileprovision
base64 < /tmp/apple-provisioning-profiles.zip | gh secret set APPLE_PROVISIONING_PROFILES --env apple-production --repo YOUR_ACCOUNT/ios-cloud-builder
```

Securely remove the temporary ZIP after setting the secret. During migration,
both profile secrets may exist; candidates from both are considered. Once at
least one deployment per configured Bundle ID succeeds, the legacy
`APPLE_PROVISIONING_PROFILE` secret can be removed. The bundle is limited by
GitHub's Environment-secret size limit; split certificates/teams across
separate protected builders if the compressed profiles no longer fit.

Verify metadata without reading secret values:

```bash
builder central doctor --testflight
```

## Add a private application

From each authorized private application:

```bash
cd /path/to/private-app
builder central setup --builder YOUR_ACCOUNT/ios-cloud-builder
builder central register
builder central doctor
```

`central setup` detects the source GitHub repository, project type, and common iOS path; creates/reuses a local AGE identity; and writes a configuration like:

```json
{
  "project": "MyApp",
  "project_id": "p_0123456789abcdef0123456789abcdef",
  "snapshot_namespace": "11111111111111111111111111111111",
  "platform": "ios",
  "backend": "central",
  "github": { "owner": "SOURCE_OWNER", "repo": "PRIVATE_SOURCE_REPO" },
  "builder": {
    "owner": "YOUR_ACCOUNT",
    "repo": "ios-cloud-builder",
    "workflow": "ios-build.yml"
  },
  "security": { "recipient": "age1..." },
  "ios": { "path": "ios", "scheme": "", "configuration": "Debug" }
}
```

`central register` creates a random 128-bit opaque handle and a separate private
128-bit snapshot namespace, updates the protected
`PROJECT_REGISTRY` Actions secret, and keeps a mode-`0600` local registry backup
so additional private projects can be added without manually editing secret
JSON. Never commit that backup. GitHub cannot return secret plaintext, so retain
the backup when managing multiple projects; losing it requires rebuilding and
re-registering the complete mapping before replacing the secret.

Only the public AGE recipient, opaque handle, and private snapshot namespace are
stored in the private application's `builder.json`. The namespace never appears
in public dispatch inputs and prevents the public build UUID from revealing the
temporary Git ref.
The private identity is kept in the OS keyring when usable or under the user's
configuration directory with `0600` permissions. To initialize it explicitly:

```bash
builder security init
```

Existing upstream `builder.json` files migrate automatically to `backend: repository`. Adding valid `builder` and `security` fields without a backend migrates to central. `builder init --backend central --builder OWNER/REPO` is the interactive alternative and deliberately does not create `.github/workflows` in the private repository.

## Daily use

```bash
cd /path/to/any/authorized/private-ios-project
builder ios build
```

The command snapshots staged, unstaged, and untracked non-ignored files without modifying the branch, real index, or working tree. It pushes only the temporary private ref, dispatches the public builder, shows high-level progress, downloads ciphertext, decrypts locally, validates the IPA ZIP and app structure, and writes:

Regular files larger than 45 MiB are represented in that temporary ref as verified 40 MiB transport chunks. The trusted runner reconstructs them after revoking the private-repository token, so generated Unity/Xcode files can exceed GitHub's 100 MB per-blob limit without Git LFS. The transport namespace is removed before framework detection or project code runs.

```text
./dist/MyApp.ipa
```

The private ref and encrypted public artifact are deleted best-effort. A failure automatically downloads and decrypts diagnostics under `dist/`. If retrieval was interrupted, retry while the one-day artifact still exists:

```bash
builder ios logs <build-uuid>
```

Remove abandoned refs older than the default 24 hours:

```bash
builder cleanup
builder cleanup --older-than 48h
```

Snapshot creation respects `.gitignore`. An untracked secret that is not ignored will be included in the temporary private snapshot, so review ignore rules before building.

To create a Release build and upload it to TestFlight:

```bash
builder ios deploy
# equivalent:
builder ios build --testflight
```

The command's default three-hour timeout includes build, signing, upload, and
configured App Store Connect processing; it can be changed with `--timeout`.
No signed IPA is downloaded or retained as a GitHub artifact.
The protected job queries App Store Connect for the application's existing builds
under the current marketing version, increments the highest leading build-number
component, and replaces only `CFBundleVersion` before signing. This preserves one
monotonic sequence across legacy and central workflows instead of depending on a
workflow-local GitHub run counter. The application's
`CFBundleShortVersionString` is preserved. Automatic numbering requires
`ASC_ISSUER_ID`. The job validates the signed IPA with App Store Connect before
uploading it.

## Supported projects

- Native Swift/Objective-C iOS projects and workspaces
- Flutter
- React Native and ejected Expo
- Kotlin Multiplatform iOS applications
- Cordova/Ionic generated iOS projects
- XcodeGen manifests

Schemes and workspaces/projects are detected where possible. The source build always passes `CODE_SIGNING_ALLOWED=NO` and packages the device `.app` as an unsigned IPA. In TestFlight mode, the separate protected job manually signs that app with the matching App Store provisioning profile from the protected multi-application bundle. Apps containing extensions, Watch apps, XPC services, or other embedded applications require nested-bundle signing support and are rejected rather than partially signed.

## Repository backend and retained commands

Set `"backend": "repository"` to retain the original behavior where workflows live in the application repository. This mode continues to support repository-local signing and simulator sharing.

```bash
builder init
builder ios build
builder ios share
builder signing csr
builder signing p12
builder signing setup
builder dev flutter
builder dev rn
builder dev kmp
builder mobai ping
builder update
```

`builder ios share` and `builder signing setup` remain repository-backend features. Central TestFlight credentials are configured only in the public builder's protected Environment; the CLI never downloads them.

## Doctor checks

`builder central doctor` verifies, without printing credentials:

- Git and local GitHub authentication source
- config schema and source/builder separation
- public builder and private source API access
- central workflow availability
- `APP_CLIENT_ID` variable and `APP_PRIVATE_KEY` secret metadata
- `PROJECT_REGISTRY` secret metadata
- matching local AGE identity
- explicit GitHub source remote
- dry-run permission to push the temporary snapshot namespace

With `--testflight`, it also requires the Team-key `ASC_ISSUER_ID`; provisioning
profiles themselves are fetched or created by the protected signer and are not
required as GitHub secrets.

With `--testflight`, doctor additionally verifies metadata for
`PACKAGING_RECIPIENT`, `PACKAGING_AGE_IDENTITY`,
`APPLE_SIGNING_RECIPIENT`, the `apple-production` Environment, `APPLE_TEAM_ID`,
every required Environment secret, either `APPLE_PROVISIONING_PROFILES` or the
legacy `APPLE_PROVISIONING_PROFILE`, the absence of a manual approval rule, and
an exact custom branch allowlist containing only the builder's default branch.
It cannot verify secret values or profile coverage.

## Updating and contributing

See [docs/UPSTREAM.md](docs/UPSTREAM.md) for the preserved upstream baseline and merge procedure. Before submitting:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/builder
go build ./cmd/builder-runner
```

## Known limitations

- A one-time GitHub App browser setup and private-repository selection cannot be completed safely by the CLI alone.
- The opaque project ID, random build ID, operation, and caller AGE public recipient are public workflow inputs. Private repository identity, private snapshot namespace and ref, iOS path, scheme, workspace/project name, and Bundle ID are not dispatch inputs; trusted code masks resolved values before GitHub-native actions use them.
- When the private source and public builder share one GitHub account, that account name is unavoidably public as the builder repository owner. Use a separate public-builder account if the owner name itself must be hidden; the private repository name and full owner/repository pair remain protected.
- A malicious project or dependency runs as the runner user and is not strongly sandboxed; it can intentionally leak or alter its own pre-boundary output. The clean packaging job prevents post-boundary substitution but does not prove that source/dependencies were benign.
- The central hosted-runner design has the policy caveat described in [COMPLIANCE.md](COMPLIANCE.md).
- Central TestFlight supports multiple top-level applications by exact Bundle ID, but still rejects embedded app extensions, Watch apps, App Clips, and XPC services that require nested-bundle signing and additional profiles.
- Applications without `TESTFLIGHT_BETA_GROUPS` retain upload-only semantics: acceptance does not mean Apple's asynchronous processing or review has completed.
- Private GitHub SSH aliases are rejected in central mode because the CLI cannot prove an alias resolves to GitHub; use an explicit `git@github.com:OWNER/REPO.git` or `https://github.com/OWNER/REPO.git` remote.
- Failed artifact deletion is non-fatal; ciphertext expires after one day.

## License

MIT. The original license and Git history are retained. See [LICENSE](LICENSE) and [docs/UPSTREAM.md](docs/UPSTREAM.md).
