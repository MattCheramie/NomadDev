# Android — signing, distribution, deep links

This doc covers the M6.2 ship: how the native Go app is packaged into a
signed APK, how the GitHub release pipeline produces it on every tag,
and how the `nomaddev://` deep-link scheme is registered so a single
QR can drop the operator straight into a connected session.

For the high-level architecture and screen-by-screen walkthrough see
[`docs/mobile-native.md`](./mobile-native.md). The roadmap status lives
in the main [`README.md`](../README.md#phase-16-native-go-mobile-apps--in-progress).

## Building the APK

Two build modes, both driven by [`gogio`](https://pkg.go.dev/gioui.org/cmd/gogio):

```sh
make android-tools        # one-time — installs gogio under $GOPATH/bin

make android-debug        # unsigned debug APK at build/android/nomaddev.apk
make android-install      # adb install -r build/android/nomaddev.apk

make android-release      # signed release APK — required env vars below
```

Both targets register the `nomaddev` URL scheme via `gogio -schemes`,
so a tap on a `nomaddev://onboard?...` link opens the app on a device
that has it installed. The build itself needs:

- **JDK 17+** on `PATH`
- **Android SDK** with platforms 34+ and build-tools 34+ at
  `$ANDROID_SDK_ROOT` or `$ANDROID_HOME`
- **Android NDK r25+** under that SDK (`ndk/25.2.9519653`)

The CI job `mobile-native-android` in
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) provisions
all of these and produces an unsigned APK as a PR artifact. The
release job documented below produces the signed equivalent.

## Signing the release APK

`make android-release` requires two environment variables:

| Variable                  | What it's for                                         |
|---------------------------|-------------------------------------------------------|
| `ANDROID_KEYSTORE`        | path to a JKS or PKCS12 keystore file                 |
| `ANDROID_KEYSTORE_PASS`   | the keystore password (forwarded as `GOGIO_SIGNPASS`) |
| `ANDROID_VERSION`         | semver+versioncode for `gogio -version` (optional)    |

A throwaway keystore for local smoke testing:

```sh
make android-debug-keystore       # writes build/android/debug.keystore
ANDROID_KEYSTORE=build/android/debug.keystore \
ANDROID_KEYSTORE_PASS=debug \
    make android-release
```

**Never use the debug keystore for a real release.** Its password is
published in the Makefile and an APK signed with it will refuse to
install over an APK signed with the real release key (Android enforces
signing-key continuity per-package).

For the real release keystore, generate one with `keytool` directly:

```sh
keytool -genkey -v \
    -keystore /secure/path/nomaddev.keystore \
    -storetype PKCS12 \
    -storepass "$STRONG_PASSWORD" \
    -alias nomaddev \
    -keyalg RSA -keysize 2048 -validity 36500 \
    -dname "CN=NomadDev, OU=mobile, O=NomadDev, L=City, S=State, C=US"
```

Keep the keystore file (and the password) out of the repository. The
CI release pipeline reads both from GitHub repository secrets — see
the next section.

## Release pipeline

The `Release` workflow at
[`.github/workflows/release.yml`](../.github/workflows/release.yml)
runs on every `v*` tag push (and on `workflow_dispatch`). It includes
a new `android-apk` job that:

1. Provisions the Android SDK + NDK on the runner.
2. Installs gogio.
3. Decodes the release keystore from the
   `ANDROID_KEYSTORE_BASE64` repository secret if present.
4. Runs `make android-release` with the keystore password from the
   `ANDROID_KEYSTORE_PASS` secret.
5. Falls back to `make android-debug` (unsigned APK) when either
   secret is absent so the build doesn't bounce — useful for the
   initial release cuts before the keystore lands in CI.
6. Uploads `nomaddev.apk` as an artifact the `release` job attaches
   to the GitHub Release.

To provision the secrets:

```sh
# One-time, on the maintainer's machine:
base64 -w0 < /secure/path/nomaddev.keystore | gh secret set ANDROID_KEYSTORE_BASE64
printf '%s' "$STRONG_PASSWORD" | gh secret set ANDROID_KEYSTORE_PASS
```

The `gogio` version number on each release is derived from the
release tag — `v0.2.0` becomes `0.2.0.<commit-count>` so the
Android versionCode integer monotonically increases across releases.

## The `nomaddev://` deep-link scheme

When `gogio -schemes nomaddev` runs, the generated `AndroidManifest.xml`
declares an `<intent-filter>` for `android.intent.action.VIEW` with the
`nomaddev` scheme. On iOS the equivalent `CFBundleURLTypes` entry is
generated.

The app accepts two URL shapes:

1. **Native shape (recommended for new QRs):**

   ```
   nomaddev://onboard?server=<ws-url>&token=<jwt>&sid=<session-id>
   ```

2. **SPA-compatible shape (for re-using existing SPA QRs):**

   ```
   https://orch.example.com/#token=<jwt>&sid=<session-id>
   ```

   The fragment-encoded form matches what `scripts/qr-jwt` generates
   for the React Native SPA. The native app intercepts both shapes:
   in the SPA shape the WS URL is derived as
   `ws://<host>/ws` (or `wss://` when the outer URL is HTTPS) so a
   single QR works on both clients.

`HandleURL` in [`internal/mobile/ui/app.go`](../internal/mobile/ui/app.go)
parses the URL via the `ExtractOnboardParams` helper in
[`internal/mobile/state/deeplink.go`](../internal/mobile/state/deeplink.go),
saves the credentials via the `TokenStore`, and starts a session — the
same code path the Onboard screen's Connect button drives.

## What's not in this milestone

- **Android-Keystore-backed AES-GCM token storage.** Tokens still
  live in `os.UserConfigDir()/nomaddev/token.json` (private to the
  app's data directory). Upgrading the storage to Keystore-backed
  encryption requires a JNI bridge and needs on-device validation;
  it's the next milestone.
- **Google Play Store distribution.** The release pipeline produces
  a sideloadable APK; the Play Store requires an AAB (Android App
  Bundle) and a developer-account upload step. Sideload via GitHub
  Releases is the v0.x path.
