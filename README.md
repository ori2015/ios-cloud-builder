# iOS Cloud Builder

Build unsigned iOS applications, or deploy signed releases to TestFlight, from Linux, WSL, or Windows using a narrowly scoped remote macOS build. This is a truthful open-source remote-build/orchestration project derived from [MobAI-App/ios-builder](https://github.com/MobAI-App/ios-builder), not a generic compute service or a disguised workload.

The central backend lets multiple private application repositories use one public builder repository without committing private source or publishing plaintext output:

```text
private app working tree
  -> temporary private refs/ios-builder/jobs/<uuid>
  -> public builder workflow on macOS
  -> repository-scoped GitHub App checkout
  -> unsigned iOS build with private console redirection
  -> AGE-encrypted IPA + log artifact
  -> local download, decryption, IPA validation, and cleanup
```

The optional TestFlight path adds a second, protected job. The build job encrypts
the unsigned IPA to a transport recipient. After approval of the
`apple-production` Environment, the signing job receives only that authenticated
ciphertext—not the private checkout—then signs and uploads directly to App Store
Connect. GitHub stores no signed IPA artifact.

The original repository backend remains available for existing MobAI users and repository-local builds. Simulator sharing, MobAI integration, Flutter/React Native/KMP development commands, framework detection, working-tree snapshots, signing tools, and public Go wrappers are retained. See [the upstream relationship](docs/UPSTREAM.md).

> [!IMPORTANT]
> GitHub's hosted-runner terms contain a repository-association limitation, and GitHub has not explicitly approved this exact cross-repository architecture. Read [COMPLIANCE.md](COMPLIANCE.md) before using the central hosted-runner backend. The backend boundary is intentionally replaceable by a self-hosted Mac, Codemagic, or another macOS CI implementation.

## Security properties

- Private source is pushed only to the private source repository, under a temporary non-branch ref.
- The GitHub App has Metadata read and Contents read only and is installed on explicitly selected repositories.
- Each job requests an installation token for exactly one source repository, checks out with `persist-credentials: false`, then revokes the token before project code runs.
- Unsigned build remains the default; Apple credentials are available only to the manually protected `apple-production` job.
- The signing job never checks out private source and never runs project scripts or dependencies.
- Detailed dependency/compiler output is redirected to a private log from process start.
- Build logs and locally downloaded IPAs are encrypted to the caller's local-only AGE identity. TestFlight intermediates use a distinct AGE identity held by the protected Environment.
- The public artifact contains only `App.ipa.age` and `build.log.age`, is retained for one day, and is deleted early when local retrieval succeeds.
- Central mode creates no Actions caches and uploads no DerivedData, dSYM, archive, source, or plaintext diagnostics.
- Inputs are validated before credential creation; build commands use fixed argv arrays, never `eval` or user-provided scripts.
- Full UUIDv4 correlation binds the workflow run and artifact to one build.

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
Require a reviewer, restrict deployment to the protected default branch, and do
not allow unreviewed workflow changes. For a single-operator repository, leave
**Prevent self-review** disabled so the operator who dispatched the build can
approve it. Environment approval is the point at which Apple credentials become
available to the signing job.

To receive a Telegram message as soon as a successful unsigned build is ready
for that approval, configure repository secrets `TELEGRAM_BOT_TOKEN` and
`TELEGRAM_CHAT_ID` in the public builder. The notification job checks out no
source and sends only the public build ID and Actions approval URL. Notification
failure is reported as a warning and never blocks or fails the protected
deployment.

Generate a dedicated AGE identity for transport between the two jobs. Put its
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
| Environment variable | `TESTFLIGHT_BETA_GROUPS` | optional JSON map of Bundle IDs to existing beta groups |
| Environment secret | `APPLE_SIGNING_AGE_IDENTITY` | dedicated AGE identity |
| Environment secret | `APPLE_DISTRIBUTION_P12` | base64-encoded `.p12` |
| Environment secret | `APPLE_DISTRIBUTION_P12_PASSWORD` | `.p12` password |
| Environment secret | `APPLE_PROVISIONING_PROFILES` | base64-encoded ZIP of App Store `.mobileprovision` files |
| Environment secret (legacy) | `APPLE_PROVISIONING_PROFILE` | base64-encoded single App Store `.mobileprovision`; used when the bundle secret is absent |
| Environment secret | `ASC_API_KEY_P8` | complete `.p8` PEM or its base64 encoding |
| Environment secret | `ASC_KEY_ID` | App Store Connect API key ID |
| Environment secret | `ASC_ISSUER_ID` | App Store Connect issuer UUID for a Team key; omit for an Individual key |

Prefer an Individual App Store Connect key belonging to a dedicated Developer
user whose app access is restricted to the applications this builder may upload.
Individual keys do not use an issuer ID and cannot call Apple's provisioning
endpoints; that is compatible with this manual-profile signing path. If a Team
key is used instead, it applies across all apps and `ASC_ISSUER_ID` is required.
Beta-group publishing also requires a Team key with `ASC_ISSUER_ID`; upload-only
applications may continue using an Individual key.
Revoke and replace any certificate, API key, or GitHub App key that was ever
committed, pasted into a public log, or otherwise exposed; moving an exposed
credential into a secret does not make the old credential safe.

Create the multi-application profile bundle on a trusted workstation. Filenames
are only labels: the protected runner selects a profile from its signed
`application-identifier` entitlement and requires an exact Bundle ID match.
Keep only App Store distribution profiles for `APPLE_TEAM_ID` in this ZIP.

To wait for processing and publish selected applications to existing TestFlight
groups, configure `TESTFLIGHT_BETA_GROUPS` in `apple-production`. Both immutable
group ID and public-link ID are required and verified before mutation:

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
builder central doctor
```

`central setup` detects the source GitHub repository, project type, and common iOS path; creates/reuses a local AGE identity; and writes a configuration like:

```json
{
  "project": "MyApp",
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

Only the public AGE recipient is stored in this file. The private identity is kept in the OS keyring when usable or under the user's configuration directory with `0600` permissions. To initialize it explicitly:

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

The command may wait for approval of `apple-production`; its default three-hour
timeout includes that approval window and can be changed with `--timeout`. On
success, App Store Connect has accepted the upload for processing; TestFlight
processing itself is asynchronous. No signed IPA is downloaded or retained as a
GitHub artifact.
The protected job replaces only `CFBundleVersion` with the unique GitHub Actions
`run_number.run_attempt` value before signing; the application's
`CFBundleShortVersionString` is preserved. It validates the signed IPA with App
Store Connect before uploading it.

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
- matching local AGE identity
- explicit GitHub source remote
- dry-run permission to push the temporary snapshot namespace

With `--testflight`, doctor additionally verifies metadata for
`APPLE_SIGNING_RECIPIENT`, the `apple-production` Environment, `APPLE_TEAM_ID`,
every required Environment secret, either `APPLE_PROVISIONING_PROFILES` or the
legacy `APPLE_PROVISIONING_PROFILE`, a non-empty required-reviewer rule with
self-review allowed, and an exact custom branch allowlist containing only the
builder's default branch. It cannot verify secret values or profile coverage.

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
- Repository/source names and workflow inputs are public metadata even though source contents and outputs are encrypted.
- A malicious project or dependency runs as the runner user and is not strongly sandboxed.
- The central hosted-runner design has the policy caveat described in [COMPLIANCE.md](COMPLIANCE.md).
- Central TestFlight supports multiple top-level applications by exact Bundle ID, but still rejects embedded app extensions, Watch apps, App Clips, and XPC services that require nested-bundle signing and additional profiles.
- Applications without `TESTFLIGHT_BETA_GROUPS` retain upload-only semantics: acceptance does not mean Apple's asynchronous processing or review has completed.
- Private GitHub SSH aliases are rejected in central mode because the CLI cannot prove an alias resolves to GitHub; use an explicit `git@github.com:OWNER/REPO.git` or `https://github.com/OWNER/REPO.git` remote.
- Failed artifact deletion is non-fatal; ciphertext expires after one day.

## License

MIT. The original license and Git history are retained. See [LICENSE](LICENSE) and [docs/UPSTREAM.md](docs/UPSTREAM.md).
