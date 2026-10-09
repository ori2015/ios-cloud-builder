#!/usr/bin/env bash
# Builds a minimal generic project for each supported layout in a temp dir and
# checks that `builder central detect` reports the expected app folder, iOS
# folder and framework. Needs no network, no GitHub token and no Xcode.
#
#   scripts/verify-matrix.sh            # builds ./cmd/builder first
#   BUILDER=/path/to/builder scripts/verify-matrix.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

builder="${BUILDER:-}"
if [ -z "$builder" ]; then
  builder="$work/builder"
  (cd "$root" && go build -o "$builder" ./cmd/builder)
fi

pass=0
fail=0

mk() { mkdir -p "$work/$1/$2"; }
put() { # put <fixture> <relative file> <contents>
  mkdir -p "$(dirname "$work/$1/$2")"
  printf '%s\n' "$3" > "$work/$1/$2"
}
pbx() { # a project.pbxproj with one bundle identifier
  put "$1" "$2" "{ objects = { A1 = { isa = XCBuildConfiguration; buildSettings = {
PRODUCT_BUNDLE_IDENTIFIER = $3;
};
name = Debug; }; }; }"
}

# expect <fixture> <expected key=value lines, space separated> [-- detect flags]
expect() {
  local name="$1" wanted="$2"; shift 2
  local out
  if ! out="$("$builder" central detect --dir "$work/$name" "$@" 2>/dev/null)"; then
    echo "FAIL  $name: detect failed unexpectedly"; fail=$((fail + 1)); return
  fi
  local ok=1 pair
  for pair in $wanted; do
    if ! grep -qxF "$pair" <<<"$out"; then ok=0; echo "FAIL  $name: expected $pair, got:"; sed 's/^/        /' <<<"$out"; fi
  done
  if [ "$ok" = 1 ]; then echo "ok    $name"; pass=$((pass + 1)); else fail=$((fail + 1)); fi
}

# expect_error <fixture> <text the error must contain> [-- detect flags]
expect_error() {
  local name="$1" text="$2"; shift 2
  local out
  if out="$("$builder" central detect --dir "$work/$name" "$@" 2>&1)"; then
    echo "FAIL  $name: expected an error, detect succeeded"; fail=$((fail + 1)); return
  fi
  if grep -qF -- "$text" <<<"$out"; then echo "ok    $name (error names: $text)"; pass=$((pass + 1));
  else echo "FAIL  $name: error lacks '$text': $out"; fail=$((fail + 1)); fi
}

# --- Flutter
put flutter-root pubspec.yaml $'name: generic\nflutter:\n  uses-material-design: true'
pbx flutter-root ios/Runner.xcodeproj/project.pbxproj example.generic.flutter
put flutter-root .flutter-version "3.24.5"
expect flutter-root "app_path= ios_path=ios framework=flutter bundle_id=example.generic.flutter flutter_version=3.24.5"

put flutter-sub mobile/pubspec.yaml $'name: generic\nflutter:\n  uses-material-design: true'
pbx flutter-sub mobile/ios/Runner.xcodeproj/project.pbxproj example.generic.sub
expect flutter-sub "app_path=mobile ios_path=mobile/ios framework=flutter bundle_id=example.generic.sub"

# --- Expo / React Native
put expo-managed package.json '{"dependencies":{"expo":"~50.0.0","react-native":"0.73.0"}}'
put expo-managed app.json '{"expo":{"ios":{"bundleIdentifier":"example.generic.expo"}}}'
put expo-managed .nvmrc "v20.11.1"
expect expo-managed "app_path= ios_path=ios framework=expo bundle_id=example.generic.expo node_version=20.11.1"

put expo-bare package.json '{"dependencies":{"expo":"~50.0.0","react-native":"0.73.0"}}'
pbx expo-bare ios/Generic.xcodeproj/project.pbxproj example.generic.bare
expect expo-bare "ios_path=ios framework=react-native bundle_id=example.generic.bare"

put rn package.json '{"dependencies":{"react-native":"0.73.0"}}'
pbx rn ios/Generic.xcodeproj/project.pbxproj 'org.example.$(PRODUCT_NAME:rfc1034identifier)'
expect rn "ios_path=ios framework=react-native bundle_id="

put expo-monorepo apps/client/package.json '{"dependencies":{"expo":"~50.0.0"}}'
put expo-monorepo package.json '{"private":true,"workspaces":["apps/*"]}'
expect expo-monorepo "app_path=apps/client ios_path=apps/client/ios framework=expo"

# --- Capacitor / Ionic / Cordova
put capacitor package.json '{"dependencies":{"@capacitor/core":"5.0.0","@capacitor/ios":"5.0.0"}}'
put capacitor capacitor.config.json '{"appId":"example.generic.cap","webDir":"dist"}'
mk capacitor ios/App/App.xcodeproj
expect capacitor "ios_path=ios/App framework=ionic"

put ionic package.json '{"dependencies":{"@ionic/angular":"7.0.0","@capacitor/ios":"5.0.0"}}'
mk ionic ios/App/App.xcodeproj
put ionic capacitor.config.ts $'const config = {\n  appId: \'example.generic.ionic\',\n  webDir: \'www\',\n};\nexport default config;'
expect ionic "ios_path=ios/App framework=ionic bundle_id=example.generic.ionic"

# Capacitor without a committed ios folder cannot build: the error must say how to add it
put capacitor-no-ios package.json '{"dependencies":{"@capacitor/core":"5.0.0","@capacitor/ios":"5.0.0"}}'
put capacitor-no-ios capacitor.config.json '{"appId":"example.generic.cap","webDir":"dist"}'
expect_error capacitor-no-ios "cap add ios"

put cordova config.xml '<widget id="example.generic.cordova" xmlns="http://www.w3.org/ns/widgets"></widget>'
expect cordova "ios_path=platforms/ios framework=cordova"

# --- Tauri 2
put tauri package.json '{"devDependencies":{"@tauri-apps/cli":"^2.0.0"}}'
put tauri src-tauri/tauri.conf.json '{"identifier":"example.generic.tauri"}'
expect tauri "app_path= ios_path=src-tauri/gen/apple framework=tauri bundle_id=example.generic.tauri"

# --- NativeScript
put nativescript package.json '{"dependencies":{"@nativescript/core":"~8.8.0"}}'
put nativescript nativescript.config.ts $'export default {\n  id: \'example.generic.ns\',\n  appPath: \'src\',\n};'
expect nativescript "ios_path=platforms/ios framework=nativescript bundle_id=example.generic.ns"

# --- Sparkling (Lynx)
put sparkling package.json '{"dependencies":{"@lynx-js/react":"^0.116.2"},"devDependencies":{"sparkling-app-cli":"~2.0.1"}}'
pbx sparkling ios/App.xcodeproj/project.pbxproj example.generic.spk
expect sparkling "ios_path=ios framework=sparkling bundle_id=example.generic.spk"

# --- Kotlin Multiplatform
put kmp settings.gradle.kts 'rootProject.name = "generic"'
put kmp shared/build.gradle.kts 'plugins { kotlin("multiplatform") }'
pbx kmp iosApp/iosApp.xcodeproj/project.pbxproj example.generic.kmp
expect kmp "ios_path=iosApp framework=kmp bundle_id=example.generic.kmp"

# --- Native / XcodeGen
pbx native-root Generic.xcodeproj/project.pbxproj example.generic.native
expect native-root "app_path= ios_path= framework=native bundle_id=example.generic.native"

pbx native-sub App/Generic.xcodeproj/project.pbxproj example.generic.nsub
expect native-sub "app_path=App ios_path=App framework=native"

put xcodegen project.yml $'name: Generic\ntargets:\n  Generic:\n    type: application\n    settings:\n      base:\n        PRODUCT_BUNDLE_IDENTIFIER: example.generic.gen'
expect xcodegen "ios_path= framework=native bundle_id=example.generic.gen"

# an empty .xcworkspace stub beside project.yml must not hide the XcodeGen manifest
put xcodegen-stub project.yml $'name: Generic\ntargets:\n  Generic:\n    type: application'
mk xcodegen-stub Generic.xcworkspace
expect xcodegen-stub "ios_path= framework=native"

# --- Monorepo choice and the actionable errors
put two-apps a/pubspec.yaml $'flutter:\n  uses-material-design: true'
pbx two-apps a/ios/Runner.xcodeproj/project.pbxproj example.generic.a
put two-apps b/pubspec.yaml $'flutter:\n  uses-material-design: true'
pbx two-apps b/ios/Runner.xcodeproj/project.pbxproj example.generic.b
expect_error two-apps "--app-path"
expect two-apps "app_path=b ios_path=b/ios framework=flutter bundle_id=example.generic.b" --app-path b

# Engines that are recognised but not built must say so and how to proceed
put unity ProjectSettings/ProjectVersion.txt "m_EditorVersion: 2022.3.0f1"
expect_error unity "Unity is recognised"
put godot project.godot "config_version=5"
put godot export_presets.cfg $'[preset.0]\nplatform="iOS"'
expect_error godot "Godot is recognised"
put maui App.csproj '<Project><PropertyGroup><TargetFrameworks>net8.0-ios</TargetFrameworks><UseMaui>true</UseMaui></PropertyGroup></Project>'
expect_error maui "MAUI is recognised"

put empty README.md "nothing here"
expect_error empty "pubspec.yaml"

put swift-package Package.swift "// swift-tools-version:5.9"
expect_error swift-package "Package.swift"

put dart-only pubspec.yaml $'name: tool\ndependencies:\n  path: ^1.0.0'
expect_error dart-only "--app-path"

echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
