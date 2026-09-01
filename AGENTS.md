# AGENTS.md

Instructions for AI coding agents working in this repository. Keep it short — toolchain
versions are pinned in the consuming Android app repository,
[DrewCyber/yggstack-android](https://github.com/DrewCyber/yggstack-android)
(`.github/workflows/build-release.yml` there is the source of truth); they are not
duplicated here.

## What this is

Fork of [yggstack](https://github.com/yggdrasil-network/yggstack) (Go, `develop` branch) with
the `mobile/` directory added: gomobile bindings (`yggstack.go`, `stats.go`, `quic_check.go`),
the AAR build script (`build-android.sh`), and usage docs (`API_USAGE.md`, `BUILD_MODES.md`).
The yggstack-android app consumes this repository as a git submodule (`lib/yggstack`);
changes here have no effect on the app until the AAR is rebuilt and copied into that app's
`app/libs/` (see the yggstack-android repository for that step).

## Rules

- Never run `gomobile init` and never install gomobile/gobind with `@latest` — the pinned
  versions live in the yggstack-android CI workflow. `mobile/build-android.sh` installs the
  pinned tools if they are missing and creates `$GOPATH/pkg/gomobile` directly, which is all
  `gomobile bind` needs.
- This is a separate git repository with its own remote and history. When consumed as a
  submodule, commit and push changes here **before** updating the submodule pointer in the
  parent repository, or CI will try to clone a commit that does not exist yet.

## Build

```bash
./mobile/build-android.sh          # → android-build/yggstack.aar
```

The output is a fat AAR covering all four ABIs (arm64-v8a, armeabi-v7a, x86, x86_64). With
`CI`/`GITHUB_ACTIONS` set the script builds the release flavor (stripped symbols); locally it
keeps symbols for debugging.
