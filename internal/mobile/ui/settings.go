//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"fmt"
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/mattcheramie/nomaddev/internal/mobile/state"
	"github.com/mattcheramie/nomaddev/internal/wireclient"
)

// Settings renders the post-auth settings drawer: connection metadata,
// the cumulative session-token ticker, the model picker (when the
// orchestrator's middleware advertised a model list at hello), and four
// action buttons (Reset history, Force reconnect, Open config, Sign out).
type Settings struct {
	pal Palette

	list   widget.List
	back   widget.Clickable
	models []widget.Clickable

	reset     widget.Clickable
	reconnect widget.Clickable
	config    widget.Clickable
	signOut   widget.Clickable

	// Callbacks. App wires these to its session/store methods. All are
	// nillable; a missing callback is a silent no-op so the widget can
	// be exercised in tests without the rest of the App scaffold.
	OnBack          func()
	OnSelectModel   func(model string)
	OnResetHistory  func()
	OnForceReconnect func()
	OnOpenConfig    func()
	OnSignOut       func()
}

// NewSettings returns an empty settings screen ready to Layout.
func NewSettings(pal Palette) *Settings {
	s := &Settings{pal: pal}
	s.list.Axis = layout.Vertical
	return s
}

// Layout draws the settings screen. The store snapshot is passed in fresh
// each frame so the widget reflects live status / outbox / token totals
// without owning any of that state itself.
func (s *Settings) Layout(gtx layout.Context, th *material.Theme, snap state.State, outboxLen int) layout.Dimensions {
	paint.Fill(gtx.Ops, s.pal.Bg)
	if s.back.Clicked(gtx) && s.OnBack != nil {
		s.OnBack()
	}
	if s.reset.Clicked(gtx) && s.OnResetHistory != nil {
		s.OnResetHistory()
	}
	if s.reconnect.Clicked(gtx) && s.OnForceReconnect != nil {
		s.OnForceReconnect()
	}
	if s.config.Clicked(gtx) && s.OnOpenConfig != nil {
		s.OnOpenConfig()
	}
	if s.signOut.Clicked(gtx) && s.OnSignOut != nil {
		s.OnSignOut()
	}
	// Keep one clickable per model row across frames so a tap that
	// straddles a re-layout still registers on the correct row.
	if cap(s.models) < len(snap.AvailableModels) {
		s.models = make([]widget.Clickable, len(snap.AvailableModels))
	} else {
		s.models = s.models[:len(snap.AvailableModels)]
	}
	for i := range s.models {
		if s.models[i].Clicked(gtx) && s.OnSelectModel != nil {
			s.OnSelectModel(snap.AvailableModels[i])
		}
	}

	rows := s.buildRows(th, snap, outboxLen)
	inset := layout.Inset{Top: unit.Dp(36), Bottom: unit.Dp(24), Left: unit.Dp(16), Right: unit.Dp(16)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		ml := material.List(th, &s.list)
		return ml.Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
			return rows[i](gtx)
		})
	})
}

// buildRows returns one widget per visible row. Splitting this off keeps
// Layout's flow readable and makes adding new rows (e.g. WebAuthn in M6)
// a single insertion.
func (s *Settings) buildRows(th *material.Theme, snap state.State, outboxLen int) []layout.Widget {
	rows := []layout.Widget{
		s.headerRow(th),
		layout.Spacer{Height: unit.Dp(16)}.Layout,
		s.sectionHeader(th, "Connection"),
		s.keyValue(th, "Server", snap.ServerURL),
		s.keyValue(th, "Status", statusText(snap.Status)),
		s.keyValue(th, "Session ID", emptyAsDash(snap.SessionID)),
		s.keyValue(th, "Last event", emptyAsDash(snap.LastEventID)),
		s.keyValue(th, "Outbox", fmt.Sprintf("%d queued", outboxLen)),

		layout.Spacer{Height: unit.Dp(16)}.Layout,
		s.sectionHeader(th, "Usage this session"),
		s.keyValue(th, "Prompt tokens", fmt.Sprintf("%d", snap.SessionTokens.Prompt)),
		s.keyValue(th, "Candidate tokens", fmt.Sprintf("%d", snap.SessionTokens.Candidates)),
		s.keyValue(th, "Total tokens", fmt.Sprintf("%d", snap.SessionTokens.Total)),
		s.keyValue(th, "Cost (USD)", fmt.Sprintf("$%.4f", snap.SessionTokens.CostUSD)),
	}

	if len(snap.AvailableModels) > 0 {
		rows = append(rows,
			layout.Spacer{Height: unit.Dp(16)}.Layout,
			s.sectionHeader(th, "Model"),
		)
		for i, m := range snap.AvailableModels {
			rows = append(rows, s.modelRow(th, i, m, snap.Model))
		}
	}

	if snap.LastError != "" {
		rows = append(rows,
			layout.Spacer{Height: unit.Dp(16)}.Layout,
			s.sectionHeader(th, "Last error"),
			func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, snap.LastError)
				lbl.Color = s.pal.Danger
				return lbl.Layout(gtx)
			},
		)
	}

	rows = append(rows,
		layout.Spacer{Height: unit.Dp(24)}.Layout,
		s.actionButton(th, &s.reset, "Reset history", s.pal.Surface),
		layout.Spacer{Height: unit.Dp(8)}.Layout,
		s.actionButton(th, &s.reconnect, "Force reconnect", s.pal.Surface),
		layout.Spacer{Height: unit.Dp(8)}.Layout,
		s.actionButton(th, &s.config, "Open server config", s.pal.Surface),
		layout.Spacer{Height: unit.Dp(8)}.Layout,
		s.actionButton(th, &s.signOut, "Sign out", s.pal.Danger),
	)
	return rows
}

func (s *Settings) headerRow(th *material.Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				t := material.H5(th, "Settings")
				t.Color = s.pal.Fg
				return t.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &s.back, "Back")
				btn.Background = s.pal.Surface
				btn.Color = s.pal.Fg
				return btn.Layout(gtx)
			}),
		)
	}
}

func (s *Settings) sectionHeader(th *material.Theme, label string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(th, label)
		lbl.Color = s.pal.Muted
		return lbl.Layout(gtx)
	}
}

func (s *Settings) keyValue(th *material.Theme, key, value string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceBetween}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th, key)
					lbl.Color = s.pal.Muted
					return lbl.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.E.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(th, value)
						lbl.Color = s.pal.Fg
						return lbl.Layout(gtx)
					})
				}),
			)
		})
	}
}

func (s *Settings) modelRow(th *material.Theme, idx int, name, current string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		selected := name == current
		bg := s.pal.Surface
		if selected {
			bg = color.NRGBA{R: 0x2a, G: 0x42, B: 0x6b, A: 0xff}
		}
		return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return material.ButtonLayout(th, &s.models[idx]).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Stack{}.Layout(gtx,
					layout.Expanded(func(gtx layout.Context) layout.Dimensions {
						rect := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(6))
						paint.FillShape(gtx.Ops, bg, rect.Op(gtx.Ops))
						return layout.Dimensions{Size: gtx.Constraints.Min}
					}),
					layout.Stacked(func(gtx layout.Context) layout.Dimensions {
						return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Body1(th, name)
							lbl.Color = s.pal.Fg
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		})
	}
}

func (s *Settings) actionButton(th *material.Theme, click *widget.Clickable, label string, bg color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		btn := material.Button(th, click, label)
		btn.Background = bg
		btn.Color = s.pal.Fg
		return btn.Layout(gtx)
	}
}

func emptyAsDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// statusText is re-exported privately to avoid a circular dependency on
// chat.go's helper; we keep it identical so the labels match the chat
// header indicator.
func init() {
	// Compile-time check that wireclient.Status surface hasn't drifted —
	// the const set is small and stable, this is just a poke to surface
	// a build error if someone removes one of these constants without
	// updating the UI.
	_ = wireclient.StatusOpen
	_ = wireclient.StatusClosed
}
