// No-op stub so `go list ./...` and `go vet ./...` succeed on Linux CI
// runners that don't ship the X11/Wayland dev headers Gio needs.
// On those builds the binary is intentionally a panic-on-launch placeholder
// — the real targets are Android (via gogio), iOS (via gogio), macOS, and
// Windows; see the //go:build line in main.go.

//go:build !android && !ios && !darwin && !windows && !(linux && nomaddev_mobile_desktop)

package main

func main() {
	panic("nomaddev-mobile: this binary is built for android/ios/darwin/windows; " +
		"use `make android-debug` for Android via gogio, or build with " +
		"`-tags nomaddev_mobile_desktop` on Linux with Wayland/X11 dev headers installed")
}
