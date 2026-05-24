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
      state.go         Store, Subscribe/Update/Snapshot, Turn, ToolCall,
                        TerminalLine, ApprovalRequest, SessionTokens,
                        PendingImages + Add/Remove/Take (M4),
                        Screen + SetScreen + ResetTurns (M5),
                        RestartPending + SetRestartPending (M6.1)
      ingest.go        envelope → state reducer (mirrors mobile/src/state)
      images.go        DecodeImageAttachment (MIME sniff + base64) (M4)
      admin.go         AdminClient — /admin/config GET + PUT +
                        /admin/config/restart with typed errors (M5/M6.1)
      deeplink.go      ExtractOnboardParams — nomaddev:// and SPA-fragment
                        URL parser shared by HandleURL (M6.2)
      tokens.go        TokenStore + encryptedFileStore with auto-migration
                        from the legacy plain-JSON layout (M6.3)
      codec.go         TokenCodec + Passthrough + AES-256-GCM with
                        per-install key file at 0o600 (M6.3)

    ui/                Gio widgets (build-tagged: android|ios|darwin|windows)
      theme.go         Palette + material.Theme
      app.go           shell: subscribe to store, drive screens + session,
                        own the *explorer.Explorer for image pick (M4),
                        route between Chat/Settings/Config (M5)
      onboard.go       server URL + JWT entry
      chat.go          turn list (user/asst bubbles + inline LiveTerminals
                        + attachments strip + composer + ⚙ header button)
      approval.go      ApprovalSheet modal (M3)
      terminal.go      LiveTerminal widget (M3)
      settings.go      Settings screen — connection metadata, session
                        tokens, model picker, reset/reconnect/sign-out (M5)
      config.go        schema-driven /admin/config editor with dirty
                        tracking, type-driven fields, dangerous-flag
                        acknowledgement gate, and the Apply + Restart +
                        polling-reconnect flow (M5 read-only → M6 editor)
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

When the orchestrator's middleware dispatches a tool, the wire flow
becomes:

1. `command.request` (correlation_id = the user.intent id) — `Ingest`
   attaches a new `ToolCall` to the matching `Turn`.
2. Optional `tool.approval.request` — `Ingest` pushes an
   `ApprovalRequest` onto `PendingApprovals`. The App shell renders
   `ApprovalSheet` over the chat surface; the operator types the tool
   name and taps Approve, which sends `tool.approval.granted` with the
   request envelope ID as `correlation_id`.
3. `command.chunk` (correlation_id = the command.request id) —
   `MergeChunkIntoToolCall` folds the chunk into the call's `Lines`
   ring, holding any unterminated trailing fragment in
   `StdoutPartial` / `StderrPartial` until the next chunk closes it.
   Lines beyond `TerminalLineCap` (2000) roll off the front.
4. `sandbox.heartbeat` — updates the call's `ElapsedMs`. The
   `LiveTerminal` widget extrapolates forward from this anchor with
   the local clock so the timer never freezes between heartbeats.
5. `command.result` — closes the call (`AwaitingApproval` cleared,
   `Result` set). The `LiveTerminal` swaps "live" for "done" and
   stops the elapsed-time tick.

## Image attachments

The composer's `+image` button opens the platform image picker via
[`gioui.org/x/explorer`](https://pkg.go.dev/gioui.org/x/explorer):

1. `App.openImagePicker` runs `Explorer.ChooseFile(".jpg", ".jpeg",
   ".png", ".gif", ".webp")` on a background goroutine. The Android
   path goes through `ACTION_GET_CONTENT` via explorer's bundled JAR
   (gogio embeds it automatically); iOS uses `UIDocumentPickerViewController`;
   desktop falls back to the OS-native file dialog.
2. The picked `io.ReadCloser` is fed to `state.DecodeImageAttachment`,
   which reads up to `MaxImageBytes+1` bytes, detects the MIME type
   via `http.DetectContentType` with a filename-extension fallback for
   SAF content URIs that strip the extension, and base64-encodes the
   bytes into a `event.ImageInput{MediaType, Data}`.
3. `Store.AddPendingImage` enforces `MaxImageCount` (4) and
   `MaxImageBytes` (5 MiB) — the same caps the orchestrator's
   user.intent validator applies — and pushes the attachment onto
   `State.PendingImages`. Composer renders one chip per attachment
   with a tap-to-remove × badge.
4. On Send, `App.sendIntent` calls `Store.TakePendingImages` which
   atomically returns the queue and clears it, then ships them as
   `UserIntentPayload.Images` on the outbound envelope.

## Navigation and the admin surface

The Chat header has a ⚙ button that flips `State.Screen` to
`ScreenSettings`. The Settings screen surfaces the connection
metadata (server URL, status, session ID, last event ID, outbox
depth), the cumulative session token + cost ticker, the model picker
(populated from the `hello`'s `available_models`), the last error,
and four action buttons:

- **Reset history** — clears `State.Turns` and `State.SessionTokens`
  locally for snappy UX, then sends a `user.command{reset_history}`
  envelope so the orchestrator wipes its server-side history.
- **Force reconnect** — tears down the current `wireclient.Session`
  and rebuilds it against the saved credentials.
- **Open server config** — navigates to the read-only `Config`
  viewer and kicks an asynchronous `GET /admin/config` fetch.
- **Sign out** — closes the session, clears `TokenStore`, and
  returns to Onboard.

The Config editor (M6) groups settings by category. Each category is
collapsible with a dirty-count badge. Type-driven field widgets:

- **bool** and **enum** fields render as tap-to-cycle buttons (each
  tap advances to the next value in the cycle list).
- **string / int / duration / csv / float / int64** all hand off to a
  text editor; the orchestrator's validator returns a per-field error
  on apply when the input doesn't parse, and the editor surfaces that
  error inline next to the row.
- **Read-only** rows render the current value as muted text — no
  editor, since the server rejects writes to them.
- **Secrets** display `(secret set)` / `(unset)` when empty; the
  text editor accepts new values, an empty submit is treated as "leave
  unchanged" by the server.

Dirty fields show a Revert button per row plus an aggregate Revert-all
in the footer. When at least one staged change targets a row flagged
`dangerous: true`, the Apply button is gated behind an explicit
"Acknowledge dangerous changes" confirmation; the apply button itself
turns red so the colour reinforces the typed acknowledgement.

The Apply + Restart flow is a state machine (`idle` → `applying` →
`restarting` → `applied` / `reauth` / `failed`) running on a background
goroutine in the App shell:

1. `App.applyConfig` PUTs the changes via `AdminClient.ApplyConfig`.
   A per-field error (`env_var` populated) marks the offending row
   and opens its category; a banner-only error renders above the
   editor. 401 transitions to `reauth` so the operator re-onboards.
2. On success, `POST /admin/config/restart` fires and the App tears
   down its current `wireclient.Session` so the orchestrator can
   exit cleanly.
3. The polling loop (`driveRestartPolling`) starts a new session and
   waits up to 35 s, checking every 2.5 s for a fresh hello to clear
   `RestartPending` (success) or for the dial to come back with
   `unauthorized` (JWT secret rotated → `reauth`).
4. On `applied`, the editor re-fetches `GET /admin/config` so the
   operator sees the new effective values.

The same client (`AdminClient`) drives every endpoint; it derives the
HTTP base from the WebSocket URL the App already has on file
(`ws://host/ws` → `http://host`) so the operator never has to type a
second URL.

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

### Signed release APK + deep links

`make android-release` produces a signed APK ready to attach to a
GitHub Release; the gogio `-signkey` / `-signpass` flags handle the
signing and `-schemes nomaddev` registers the `nomaddev://` URL
scheme in the generated manifest. The CI release workflow runs the
same target on every `v*` tag. See
[`docs/mobile-android.md`](./mobile-android.md) for the keystore
provisioning, environment variables, and deep-link URL formats.

### Token encryption at rest (M6.3)

The saved JWT lives at `os.UserConfigDir()/nomaddev/token`. M6.3 wraps
that file in AES-256-GCM via the new `TokenCodec` interface:

- `state.NewAESGCMCodec(path)` lazily generates a 32-byte random key
  on first use, persists it at `os.UserConfigDir()/nomaddev/token.key`
  with mode `0o600`, and uses it to seal every Save.
- `state.NewEncryptedFileTokenStore(path, codec)` wraps the codec
  around the same on-disk layout the M2 store used. AES-GCM provides
  authenticated encryption — a tampered file surfaces as a decrypt
  error rather than silently corrupted credentials.
- The store auto-migrates the legacy plain-JSON file from M2: if Load
  sees an opening `{` it parses the file as plaintext, returns the
  values, and the next Save rewrites the file as ciphertext. No
  manual upgrade step.
- `Clear` (sign-out) deletes both the token and the key file so the
  next session starts with a fresh key — any previously-leaked key
  file is self-healing on the next onboard.

The threat model the codec defends against is "an attacker reads the
token file alone" (cloud backup, casual filesystem-share, photo of
the operator's screen during recovery). An attacker who has read
access to the whole app private dir — root on the device,
debug-bridge access to an unlocked phone — reads both files and can
decrypt. Closing that gap requires binding the key to the
hardware-backed Android Keystore, which needs a JNI bridge and
on-device validation; that work is deferred to M6.4 and the
`TokenCodec` interface is the seam it'll plug into.

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
