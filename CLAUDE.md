# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Builder** is a Go CLI tool for iOS development without a Mac. It has two main capabilities:
1. **Remote builds**: Build iOS apps via GitHub Actions from any platform
2. **Dev tools**: Hot reload on real iOS devices using MobAI (Flutter and React Native)

## Build Commands

```bash
# Build
go build -o builder ./cmd/builder

# Test
go test ./...

# Install
go install ./cmd/builder

# Run
./builder auth github       # Authenticate with GitHub (OAuth device flow)
./builder init              # Set up workflow in current repo
./builder ios build         # Trigger build and download IPA to ./dist/
./builder dev flutter       # Flutter hot reload with MobAI
./builder dev rn            # React Native hot reload with MobAI
./builder dev kmp           # Kotlin Multiplatform install + launch (no hot reload)
./builder dev flutter --skip-install --bundle-id <id>  # Use already installed app
./builder dev rn --skip-install --bundle-id <id>       # Use already installed app
```

## Architecture

```
builder auth github ─────► GitHub OAuth (device flow)
                                │
                                ▼
                          Token stored in OS keychain

builder init ────────────► Detects git remote (origin)
                                │
                                ▼
                          Creates .github/workflows/ios-build.yml
                          Creates builder.json
                          Optionally commits/pushes and runs build

builder ios build ───────► Snapshots working tree (git commit-tree)
                                │
                                ▼
                          Pushes to refs/ios-builder/jobs/<build-id>
                                │
                                ▼
                          Triggers workflow_dispatch with snapshot_ref
                                │
                                ▼
                          GitHub Actions (macos-14)
                            ├─ Checks out the snapshot ref
                            ├─ Detects Flutter/native
                            ├─ Caches DerivedData
                            ├─ Builds unsigned IPA
                            └─ Uploads artifact
                                │
                                ▼
                          Downloads IPA to ./dist/
                                │
                                ▼
                          Deletes the snapshot ref

builder dev flutter ─────► Connects to MobAI
                                │
                                ▼
                          Installs IPA on device (with optional re-sign)
                                │
                                ▼
                          Launches app with debugger
                                │
                                ▼
                          Captures VM Service URL
                                │
                                ▼
                          Runs flutter attach (hot reload)

builder dev rn ──────────► Starts Metro bundler (if not running)
                                │
                                ▼
                          Connects to MobAI
                                │
                                ▼
                          Installs IPA on device (with optional re-sign)
                                │
                                ▼
                          Launches app with Metro URL env vars

builder dev kmp ─────────► Connects to MobAI
                                │
                                ▼
                          Installs IPA on device (with optional re-sign)
                                │
                                ▼
                          Launches app and streams output (no hot reload)
```

### Module Layout

```
cmd/builder/         # CLI entrypoint (Cobra)
internal/
  auth/              # GitHub OAuth device flow + keyring storage
  github/            # GitHub REST API (workflow dispatch, artifacts)
  build/             # Build coordination (snapshot + trigger + poll + download)
  signing/           # CSR generation and .p12 assembly (signing without a Mac)
  snapshot/          # Working-tree snapshot as a throwaway commit on a remote ref
  workflow/          # Workflow template (embedded)
  config/            # builder.json management
  dev/               # Development session (Flutter/React Native hot reload)
  mobai/             # MobAI API client (device control, app install/launch)
```

## Key Patterns

- **OAuth Device Flow**: No gh CLI dependency, uses GitHub's device authorization
- **Keyring Storage**: Token stored via `go-keyring` (macOS Keychain, Windows Credential Manager, Linux SecretService)
- **Interactive Init**: Prompts to commit/push and run first build
- **Remote Selection**: `--remote` flag to use non-origin remotes
- **Working-Tree Snapshot**: `ios build` builds what is on disk, including uncommitted and
  untracked files. `git write-tree`/`commit-tree` against a temporary index produce a commit
  parented on HEAD; the branch, index and working tree are never modified. The commit is pushed
  to `refs/ios-builder/jobs/<build-id>`, which is outside `refs/heads` so it creates no branch
  and fires no `push` events, and is deleted when the build finishes.
- **Snapshot Exclusions**: `.gitignore` applies, so ignored files (`.env`, `GoogleService-Info.plist`,
  local `*.xcconfig`) are absent from the build. Submodules are recorded as gitlinks, so a
  submodule commit that only exists locally fails checkout on the runner.
- **Run Correlation**: `run-name` carries the build ID so concurrent builds cannot adopt each
  other's runs
- **Flutter Detection**: Auto-detects Flutter projects, runs `flutter pub get`, uses `Runner` scheme
- **DerivedData Caching**: Single static cache key for incremental builds
- **MobAI Integration**: HTTP/WebSocket API for device control, app install, debug launch
- **Flutter Custom Devices**: Auto-configures `~/.config/flutter/custom_devices.json` for `mobai-ios` device
- **Debug URL Capture**: WebSocket stream captures VM Service URL from app launch
- **React Native Metro**: Auto-starts Metro bundler, passes Metro URL to app via environment variables
- **FrameworkHandler Interface**: Common session with pluggable handlers for Flutter/React Native/KMP
- **KMP Detection**: The multiplatform Gradle plugin is matched by regex in root and module build
  files. `cmd/builder/root.go` (`kmpPluginRe`) and both workflow templates must agree: a project the
  CLI calls KMP but the runner does not gets no JDK, and vice versa.
- **KMP Has No Hot Reload**: shared Kotlin compiles to a native framework at build time, so
  `dev kmp` only installs, launches and streams output; code changes need `ios build`

## Supported frameworks (unsigned central builds)

`builder central detect` shows what setup/register read from a project; `scripts/verify-matrix.sh`
checks detection on generic fixtures. Each framework below is built by the **Real build** CI job,
which scaffolds a project with the framework's own generator, runs the production unsigned pipeline
and validates the IPA (`internal/ipacheck`) — see `internal/runner/e2e_frameworks_test.go`.

| Framework | Generate step | Notes |
|---|---|---|
| Native / XcodeGen | `xcodegen generate` when only `project.yml` exists | stub `.xcworkspace` files are ignored |
| Flutter | `flutter pub get`, `flutter build ios --no-codesign` | version from `.flutter-version`/`.fvmrc` |
| Expo (managed) | `expo prebuild --platform ios` | Expo with a committed `ios/` builds as React Native; the newest SDK may need a newer Xcode than the runner has |
| React Native | `pod install` | |
| Capacitor / Ionic | web build script (`build`, `build:web`), `cap sync ios` | `webDir` must contain `index.html` afterwards |
| Cordova | `cordova prepare ios` | |
| Tauri 2 | `tauri ios init` (if `gen/apple` is not committed), `tauri ios build --ci --no-sign` | produces the IPA itself |
| NativeScript | `nativescript prepare ios` | |
| Sparkling (Lynx) | `npm run build` (`sparkling-app-cli build --copy`) | committed `ios/` project |
| .NET MAUI | `dotnet workload restore`, `dotnet publish -p:EnableCodeSigning=false -p:BuildIpa=true` | .NET 8 verified; the newest iOS workload can need a newer Xcode than the runner has (explained in the log) |
| Godot 4 | headless iOS export to an Xcode project (`scripts/install-godot.sh` installs the editor) | needs an iOS export preset; ETC2/ASTC import is enabled in the checkout |
| KMP | Gradle via the Xcode build phase | detected and built, but **not** covered by a real-build test (no scaffolding CLI) |
| Unity | — | recognised only: needs the editor and a license on the runner; export the Xcode project and register it with `--app-path` |

Not covered by any real build: signing, TestFlight, a physical device.

## Configuration

`builder.json`:
```json
{
  "project": "MyApp",
  "platform": "ios",
  "github": { "owner": "username", "repo": "my-ios-app" },
  "ios": { "path": "ios", "scheme": "" }
}
```

## Workflow Features

The embedded workflow template (`internal/workflow/templates/ios-build.yml`):
- Triggered via `workflow_dispatch` with `build_id`, `snapshot_ref`, `ios_path`, `scheme`
- Dispatch runs the workflow from the **default branch**, so edits to the workflow file itself
  only take effect once pushed there — unlike app sources, which come from the snapshot ref
- Checks out `snapshot_ref` over the default-branch checkout when set
- Runs on `macos-14`
- Detects Flutter projects (checks for `pubspec.yaml`)
- Caches DerivedData with static key for fast incremental builds
- Auto-detects workspace/project and scheme
- Flutter: uses `Runner` scheme, runs `flutter pub get`
- Installs CocoaPods if Podfile exists
- Builds unsigned IPA with `CODE_SIGNING_ALLOWED=NO`
- Uploads IPA as GitHub artifact with 7-day retention

## Flutter Dev Requirements

- MobAI running with physical iOS device connected (no simulators)
- App must be closed on device before launching
- Re-signing requires iCloud account (recommend creating a new one)
- After re-sign, bundle ID has team suffix (e.g., `com.example.app.TEAMID`)
- Only rebuild (`ios build`) for native code changes; Dart changes use hot reload

## React Native Dev Requirements

- MobAI running with physical iOS device connected (no simulators)
- Device and computer must be on the same WiFi network (for Metro connection)
- Node.js and React Native CLI installed
- Metro bundler started automatically or manually (`npx react-native start`)
- App must be closed on device before launching
- Re-signing requires iCloud account (recommend creating a new one)
- After re-sign, bundle ID has team suffix (e.g., `com.example.app.TEAMID`)
- Only rebuild (`ios build`) for native code changes; JS changes use hot reload
