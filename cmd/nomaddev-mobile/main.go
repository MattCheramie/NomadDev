// Command nomaddev-mobile is the Gio entrypoint for the native Android and
// iOS NomadDev apps. It builds into a real APK / IPA via gogio (see the
// android-* targets in the top-level Makefile), and into a desktop window on
// host platforms for fast iteration.
//
// M1 ships only the foundation: a placeholder window that confirms the Go +
// Gio toolchain builds and the binary launches. M2 lands real screens
// (Onboard, Chat) on top of internal/wireclient and internal/mobile/state.
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
	"image/color"
	"log"
	"os"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

func main() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title("NomadDev"))
		if err := loop(w); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	app.Main()
}

func loop(w *app.Window) error {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	th.Palette.Bg = color.NRGBA{R: 0x0d, G: 0x12, B: 0x1a, A: 0xff}
	th.Palette.Fg = color.NRGBA{R: 0xe5, G: 0xee, B: 0xfa, A: 0xff}

	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			paint.Fill(gtx.Ops, th.Palette.Bg)
			layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						title := material.H4(th, "NomadDev")
						title.Color = th.Palette.Fg
						return title.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						sub := material.Body1(th, "native shell — M1 foundation")
						sub.Color = color.NRGBA{R: 0x8a, G: 0xa0, B: 0xc6, A: 0xff}
						return sub.Layout(gtx)
					}),
				)
			})
			e.Frame(gtx.Ops)
		}
	}
}
