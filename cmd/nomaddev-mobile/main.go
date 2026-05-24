// Command nomaddev-mobile is the Gio entrypoint for the native Android and
// iOS NomadDev apps. It builds into a real APK / IPA via gogio (see the
// android-* targets in the top-level Makefile), and into a desktop window on
// host platforms with the right system libs for fast iteration.
//
// M2 lands the Onboard + minimal Chat screens on top of the M1 foundation:
// a state.Store reducer, a wireclient.Session for reconnect/outbox, and a
// file-backed token cache (Keystore-backed AES-GCM arrives in M6).
//
// Build constraint: Linux desktop iteration needs X11/Wayland dev headers
// (libwayland-dev, libxkbcommon-dev, libgles2-mesa-dev, libegl1-mesa-dev).
// To avoid breaking CI runners that don't ship those headers, Linux builds
// require an explicit `-tags nomaddev_mobile_desktop` opt-in. The Android
// build path (gogio -target android) is unaffected because it targets the
// android GOOS and uses the NDK cross-compiler.

//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package main

import (
	"log"
	"os"
	"path/filepath"

	"gioui.org/app"
	"gioui.org/io/event"

	"github.com/mattcheramie/nomaddev/internal/mobile/state"
	"github.com/mattcheramie/nomaddev/internal/mobile/ui"
)

func main() {
	store := state.New()
	tokens := state.NewEncryptedFileTokenStore(tokenPath(), state.NewAESGCMCodec(tokenKeyPath()))
	a := ui.NewApp(store, tokens)
	go func() {
		w := new(app.Window)
		w.Option(app.Title("NomadDev"))
		if err := a.Run(w); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	// app.Events replaces app.Main when we want non-window events such
	// as URLEvent (deep links via the `nomaddev` scheme registered in
	// the Makefile's android-debug / android-release targets). It
	// never returns; the OS routes URL intents to the yield func
	// whether or not a window is currently open.
	app.Events(func(e event.Event) bool {
		if u, ok := e.(app.URLEvent); ok {
			a.HandleURL(u.URL)
		}
		return true
	})
}

// tokenPath returns the on-disk location for the saved JWT envelope. On
// Android and iOS this lives under the app's private data directory;
// gogio sets up the process so os.UserConfigDir resolves there. On
// desktop hosts it falls back to the OS-conventional config dir.
//
// File extension intentionally absent — the contents are an AES-GCM
// ciphertext blob (binary), not JSON. The encryptedFileStore transparently
// migrates the legacy `token.json` plaintext file on first Load (see
// looksLikePlainJSON in internal/mobile/state/tokens.go); we look for
// the legacy file first so an existing M5/M6.2 install keeps working.
func tokenPath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = "."
	}
	// Prefer the legacy path when present so we drive the migration
	// codepath; once Save rewrites it the contents become ciphertext.
	legacy := filepath.Join(base, "nomaddev", "token.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return filepath.Join(base, "nomaddev", "token")
}

// tokenKeyPath returns the on-disk location for the per-install AES-256
// key the encrypted token store uses. Lives next to the token file at
// 0o600 perms — see internal/mobile/state/codec.go.
func tokenKeyPath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = "."
	}
	return filepath.Join(base, "nomaddev", "token.key")
}
