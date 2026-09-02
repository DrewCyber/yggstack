# AGENTS.md

Instructions for AI coding agents working in this repository. Keep it short — the Go
toolchain requirement comes from `go.mod` (CI installs it via `go-version-file: go.mod` in
`.github/workflows/trunk.yml`), and the gomobile/gobind pins live in
`mobile/build-android.sh`.

## What this is

Fork of [yggstack](https://github.com/yggdrasil-network/yggstack) (Go, `develop` branch) with
the `mobile/` directory added: gomobile bindings (`yggstack.go`, `stats.go`, `quic_check.go`),
the AAR build script (`build-android.sh`), and usage docs (`API_USAGE.md`, `BUILD_MODES.md`).

## Rules

- Never run `gomobile init` and never install gomobile/gobind with `@latest` —
  `mobile/build-android.sh` installs the pinned tools if they are missing and creates
  `$GOPATH/pkg/gomobile` directly, which is all `gomobile bind` needs.

## Build

```bash
./mobile/build-android.sh          # → android-build/yggstack.aar
```

The output is a fat AAR covering all four ABIs (arm64-v8a, armeabi-v7a, x86, x86_64). With
`CI`/`GITHUB_ACTIONS` set the script builds the release flavor (stripped symbols); locally it
keeps symbols for debugging.
