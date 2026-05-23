# Native Go mobile apps (Phase 16)

The native client is a real Android (and, in a later milestone, iOS) app
written entirely in Go. It is **additive** to the React Native SPA
documented in [`docs/mobile.md`](./mobile.md) — that surface stays
shipped and embedded in the orchestrator binary; the native app is a
separate artifact you sideload onto a phone.

## Why a second client?

The React Native SPA is what the orchestrator hands a browser today.
It works, but it is a web bundle running inside a phone browser, with
the usual sharp edges (no real background life, no secure platform
keystore, deep-linking through fragment URLs, etc.). The native client
gives the orchestrator a first-class mobile surface that owns:

- Real `Activity` lifecycle (Android) and `UIApplication` lifecycle (iOS)
- Platform secure storage (Android Keystore / iOS Keychain) for JWTs
- Platform image pickers and intent integration
- Single-binary distribution — no Tailscale-served web bundle to fetch

The orchestrator's wire protocol (`internal/event`) is unchanged; the
native client speaks the same v1 envelope vocabulary as wsclient and
the SPA.

## Architecture

```
cmd/
  nomaddev-mobile/
    main.go            Gio app.Main entrypoint (one binary per platform)
    main_stub.go       no-op stub for unsupported GOOS so go list works

internal/
  wireclient/          envelope-level WS client (shared with cmd/wsclient)
    wireclient.go      Dial / Conn / ReadEnvelope / WriteEnvelope
    session.go         long-lived auto-reconnecting Session w/ outbox

  mobile/
    state/             in-memory store + reducer
      state.go         Store, Subscribe/Update/Snapshot, Turn, SessionTokens
      ingest.go        envelope → state reducer (mirrors mobile/src/state)
      tokens.go        TokenStore (file-backed in M2, Keystore in M6)

    ui/                Gio widgets (build-tagged: android|ios|darwin|windows)
      theme.go         Palette + material.Theme
      app.go           shell: subscribe to store, drive screens + session
      onboard.go       server URL + JWT entry
      chat.go          turn list + composer
```

The `internal/mobile/ui` package is build-constrained to platforms Gio
supports without extra system headers. Linux desktop iteration needs
the X11 / Wayland dev libs (`libwayland-dev`, `libxkbcommon-dev`,
`libgles2-mesa-dev`, `libegl1-mesa-dev`, `libvulkan-dev`,
`libxkbcommon-x11-dev`, `libx11-xcb-dev`) and the
`-tags nomaddev_mobile_desktop` opt-in. On platforms without those tags
the package compiles to an empty translation unit and a stub `main`
panics on launch, so `go list ./...` still succeeds and CI runs aren't
broken by Gio's C dependencies.

## Data flow

1. The Gio thread renders frames from `state.Store.Snapshot()`.
2. The user types in the composer and taps Send. The `Chat.Submit`
   callback constructs a `user.intent` envelope, calls
   `Store.RecordSentIntent` so the user's bubble shows immediately,
   and hands the envelope to `wireclient.Session.Send`.
3. `Session` writes the envelope on the open socket — or, if offline,
   queues it in a 64-deep outbox (capacity matches the SPA so the
   orchestrator's idempotency assumptions hold). On reconnect the
   outbox drains in order.
4. The session's read goroutine receives inbound envelopes, calls
   `state.Ingest`, which updates `Store` under the same mutex used by
   `Snapshot`. The store's subscribers are notified; the UI goroutine
   sees the change on its next frame.

The Status enum (`idle | connecting | open | closed | unauthorized`)
matches `mobile/src/state/store.ts` so users moving between the SPA
and the native app see the same vocabulary.

## Building

### Android (debug APK)

```sh
make android-tools     # one-time: installs gogio under $GOPATH/bin
make android-debug     # cross-compiles into build/android/nomaddev.apk
```

Requires the Android SDK (platform 34+, build-tools 34+) and NDK
(r25+) reachable via `$ANDROID_SDK_ROOT` / `$ANDROID_HOME`, plus
JDK 17+. The CI job `mobile-native-android` provisions all of these
on every PR and uploads the APK as a 14-day artifact.

### Android (install on a connected device)

```sh
make android-install   # adb install -r build/android/nomaddev.apk
```

### Desktop iteration (Linux)

```sh
sudo apt-get install -y libwayland-dev libxkbcommon-dev libgles2-mesa-dev \
    libegl1-mesa-dev libvulkan-dev libxkbcommon-x11-dev libx11-xcb-dev
go build -tags nomaddev_mobile_desktop ./cmd/nomaddev-mobile
```

The resulting binary is the same Go code as the Android build,
rendering into an X11 / Wayland window.

### iOS

Deferred to Milestone M7 (see roadmap in
[`README.md`](../README.md#phase-16-native-go-mobile-apps--in-progress)).
The Gio code is platform-agnostic; only the token storage and image
picker need iOS-specific shims.

## Onboarding flow

1. User launches the app. `os.UserConfigDir() + "/nomaddev/token.json"`
   is checked; if a token is present, it pre-populates the Onboard
   form.
2. User enters / confirms server URL and JWT, taps Connect.
3. `App.connect` saves the credentials via `state.TokenStore.Save`,
   updates the store, and starts a `wireclient.Session` in a
   background goroutine.
4. `Session.Run` dials, reads the orchestrator's hello, replays via
   `client.hello{last_event_id}` if a prior session ID is on file,
   then enters the read loop. Status callbacks update the store; the
   UI flips from Onboard to Chat automatically.
5. Android's Back key invokes `App.signOut`, which closes the
   session, clears the saved token, and returns to Onboard.

Phase M6 will replace the plain-file token storage with an
Android-Keystore-backed AES-GCM implementation behind the same
`TokenStore` interface; nothing else changes.

## Wire-protocol parity

The native app and the React Native SPA both depend on
`internal/event/types.go` (directly for the Go binary; via a hand-mirrored
TypeScript port for the SPA). When a new envelope type lands, the
Phase-5 [`docs/mobile.md`](./mobile.md) reminder still applies: update
both translations.

For the Go side specifically, the touch points are
`internal/mobile/state/ingest.go` (reducer switch) and, when the new
event surfaces a new state slice, `internal/mobile/state/state.go`.

## Testing

```sh
go test ./internal/wireclient/...    # Session reconnect + outbox + dispatch
go test ./internal/mobile/state/...  # Store mutations, ingest reducer
```

The Gio UI itself is rendered, not unit-tested in this milestone —
the screens are intentionally thin wrappers around the store and the
reducer is where the behavior lives. End-to-end on-device validation
ships with Milestone M6.

## Roadmap

The milestones from the original plan
(`/root/.claude/plans/make-a-plan-to-quirky-narwhal.md` at the time of
M1) are tracked in [`README.md`](../README.md#phase-16-native-go-mobile-apps--in-progress).
M1 (foundation) and M2 (Onboard + minimal Chat) are landed; M3 picks
up the ApprovalSheet and LiveTerminal next.
