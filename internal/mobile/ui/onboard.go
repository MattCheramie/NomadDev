//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"gioui.org/layout"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// Onboard renders the credential entry form. The screen has two text inputs
// (server URL, JWT token) and a Connect button; surfaced errors render
// above the button so the user can see why a previous attempt failed.
type Onboard struct {
	pal Palette

	url     widget.Editor
	token   widget.Editor
	connect widget.Clickable

	// Submit is invoked when the user taps Connect with non-empty inputs.
	Submit func(serverURL, token string)
}

// NewOnboard returns an Onboard ready to be laid out.
func NewOnboard(pal Palette) *Onboard {
	o := &Onboard{pal: pal}
	o.url.SingleLine = true
	o.url.SetText("ws://127.0.0.1:8080/ws")
	o.token.SingleLine = true
	o.token.Mask = '•'
	return o
}

// SetCredentials pre-populates the form. Called when the app launches with
// a saved token so the user can re-confirm or edit.
func (o *Onboard) SetCredentials(serverURL, token string) {
	if serverURL != "" {
		o.url.SetText(serverURL)
	}
	if token != "" {
		o.token.SetText(token)
	}
}

// Layout draws the screen and dispatches Submit if Connect was clicked.
func (o *Onboard) Layout(gtx layout.Context, th *material.Theme, lastError string) layout.Dimensions {
	paint.Fill(gtx.Ops, o.pal.Bg)
	if o.connect.Clicked(gtx) {
		if o.Submit != nil {
			o.Submit(o.url.Text(), o.token.Text())
		}
	}
	inset := layout.Inset{Top: unit.Dp(48), Bottom: unit.Dp(24), Left: unit.Dp(24), Right: unit.Dp(24)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical, Spacing: layout.SpaceEnd}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				title := material.H4(th, "NomadDev")
				title.Color = o.pal.Fg
				return title.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				sub := material.Body2(th, "Connect to your orchestrator")
				sub.Color = o.pal.Muted
				return sub.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(28)}.Layout),
			layout.Rigid(o.field(th, &o.url, "Server URL")),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(o.field(th, &o.token, "JWT token")),
			layout.Rigid(layout.Spacer{Height: unit.Dp(20)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if lastError == "" {
					return layout.Dimensions{}
				}
				lbl := material.Body2(th, lastError)
				lbl.Color = o.pal.Danger
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(20)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &o.connect, "Connect")
				return btn.Layout(gtx)
			}),
		)
	})
}

func (o *Onboard) field(th *material.Theme, ed *widget.Editor, label string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, label)
				lbl.Color = o.pal.Muted
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(th, ed, "")
				ed.Color = o.pal.Fg
				ed.HintColor = o.pal.Muted
				return ed.Layout(gtx)
			}),
		)
	}
}
