// Package ui owns the Gio widgets that render the native NomadDev app.
// The package is build-constrained to the platforms gioui.org actually
// supports without extra system headers (mobile + macOS + Windows); on
// other platforms `go list ./...` sees an empty package, which is fine.
//
// All screens are passive: they read from a state.Store snapshot and emit
// callbacks for user actions. The cmd/nomaddev-mobile main wires those
// callbacks into a wireclient.Session.

//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"image/color"

	"gioui.org/font/gofont"
	"gioui.org/text"
	"gioui.org/widget/material"
)

// Palette holds the colors the app paints with. The values mirror the
// React Native SPA's dark theme so users moving between the two surfaces
// don't perceive a brand change.
type Palette struct {
	Bg         color.NRGBA
	Surface    color.NRGBA
	Fg         color.NRGBA
	Muted      color.NRGBA
	Accent     color.NRGBA
	UserBubble color.NRGBA
	AsstBubble color.NRGBA
	Danger     color.NRGBA
	Border     color.NRGBA
}

// DefaultPalette returns the dark palette used in production.
func DefaultPalette() Palette {
	return Palette{
		Bg:         color.NRGBA{R: 0x0d, G: 0x12, B: 0x1a, A: 0xff},
		Surface:    color.NRGBA{R: 0x14, G: 0x1c, B: 0x28, A: 0xff},
		Fg:         color.NRGBA{R: 0xe5, G: 0xee, B: 0xfa, A: 0xff},
		Muted:      color.NRGBA{R: 0x8a, G: 0xa0, B: 0xc6, A: 0xff},
		Accent:     color.NRGBA{R: 0x4c, G: 0x9a, B: 0xff, A: 0xff},
		UserBubble: color.NRGBA{R: 0x2a, G: 0x42, B: 0x6b, A: 0xff},
		AsstBubble: color.NRGBA{R: 0x1c, G: 0x26, B: 0x36, A: 0xff},
		Danger:     color.NRGBA{R: 0xff, G: 0x6b, B: 0x6b, A: 0xff},
		Border:     color.NRGBA{R: 0x23, G: 0x2e, B: 0x42, A: 0xff},
	}
}

// NewTheme returns a Gio material theme wired to the default Go font
// collection and the app palette.
func NewTheme() *material.Theme {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	p := DefaultPalette()
	th.Palette.Bg = p.Bg
	th.Palette.Fg = p.Fg
	th.Palette.ContrastBg = p.Accent
	th.Palette.ContrastFg = p.Bg
	return th
}
