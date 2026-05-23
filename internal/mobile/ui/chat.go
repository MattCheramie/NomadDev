//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"image"
	"image/color"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/mattcheramie/nomaddev/internal/mobile/state"
	"github.com/mattcheramie/nomaddev/internal/wireclient"
)

// Chat renders the turn-by-turn conversation plus a text composer. M3 adds
// the per-tool-call LiveTerminal directly under each assistant bubble; the
// ApprovalSheet is rendered by the App shell above the chat surface.
type Chat struct {
	pal      Palette
	list     widget.List
	composer widget.Editor
	send     widget.Clickable

	// Per-tool-call LiveTerminal widgets, keyed by CommandID so scroll
	// position survives across frames.
	terminals    map[string]*LiveTerminal
	terminalSeen map[string]time.Time

	// Submit is invoked when the user taps Send with non-empty text.
	Submit func(text string)
}

// NewChat returns an empty Chat screen.
func NewChat(pal Palette) *Chat {
	c := &Chat{
		pal:          pal,
		terminals:    map[string]*LiveTerminal{},
		terminalSeen: map[string]time.Time{},
	}
	c.list.Axis = layout.Vertical
	c.list.ScrollToEnd = true
	c.composer.SingleLine = false
	c.composer.Submit = false
	return c
}

// Layout draws the chat screen. It does not own the state — callers pass in
// the current snapshot, which keeps the widget testable and stateless.
func (c *Chat) Layout(gtx layout.Context, th *material.Theme, snap state.State) layout.Dimensions {
	paint.Fill(gtx.Ops, c.pal.Bg)
	if c.send.Clicked(gtx) {
		text := c.composer.Text()
		c.composer.SetText("")
		if c.Submit != nil && text != "" {
			c.Submit(text)
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.header(gtx, th, snap)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return c.turns(gtx, th, snap.Turns)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.composerRow(gtx, th)
		}),
	)
}

// terminalFor returns the LiveTerminal widget for the given command,
// constructing it on first sight and recording the wall-clock anchor for
// elapsed-time extrapolation between heartbeats.
func (c *Chat) terminalFor(commandID string) (*LiveTerminal, time.Time) {
	if t, ok := c.terminals[commandID]; ok {
		return t, c.terminalSeen[commandID]
	}
	t := NewLiveTerminal(c.pal)
	c.terminals[commandID] = t
	c.terminalSeen[commandID] = time.Now()
	return t, c.terminalSeen[commandID]
}

func (c *Chat) header(gtx layout.Context, th *material.Theme, snap state.State) layout.Dimensions {
	inset := layout.Inset{Top: unit.Dp(36), Bottom: unit.Dp(8), Left: unit.Dp(16), Right: unit.Dp(16)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				t := material.H6(th, "NomadDev")
				t.Color = c.pal.Fg
				return t.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, statusText(snap.Status))
				lbl.Color = statusColor(c.pal, snap.Status)
				return lbl.Layout(gtx)
			}),
		)
	})
}

func (c *Chat) turns(gtx layout.Context, th *material.Theme, turns []state.Turn) layout.Dimensions {
	// Flatten turns into a row list: [user, asst, tool*, ...] so each
	// turn's tool calls render inline under the assistant bubble.
	type row struct {
		kind string // "user" | "asst" | "tool" | "err"
		turn state.Turn
		call state.ToolCall
	}
	rows := make([]row, 0, len(turns)*3)
	for _, t := range turns {
		rows = append(rows, row{kind: "user", turn: t})
		if t.Error != "" {
			rows = append(rows, row{kind: "err", turn: t})
		} else {
			rows = append(rows, row{kind: "asst", turn: t})
		}
		for _, call := range t.ToolCalls {
			rows = append(rows, row{kind: "tool", turn: t, call: call})
		}
	}
	now := time.Now()
	inset := layout.Inset{Left: unit.Dp(12), Right: unit.Dp(12)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		ml := material.List(th, &c.list)
		return ml.Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
			r := rows[i]
			switch r.kind {
			case "user":
				return c.bubble(gtx, th, r.turn.UserText, true)
			case "err":
				return c.errorBubble(gtx, th, r.turn.Error)
			case "tool":
				return c.toolRow(gtx, th, r.call, now)
			default:
				text := r.turn.AssistantText
				if !r.turn.Finished && text == "" && len(r.turn.ToolCalls) == 0 {
					text = "…"
				}
				if text == "" {
					return layout.Dimensions{}
				}
				return c.bubble(gtx, th, text, false)
			}
		})
	})
}

func (c *Chat) toolRow(gtx layout.Context, th *material.Theme, call state.ToolCall, now time.Time) layout.Dimensions {
	term, anchor := c.terminalFor(call.CommandID)
	pad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(4), Right: unit.Dp(4)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, call.Tool)
				lbl.Color = c.pal.Muted
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = gtx.Dp(180)
				gtx.Constraints.Max.Y = gtx.Dp(280)
				return term.Layout(gtx, th, call, anchor, now)
			}),
		)
	})
}

func (c *Chat) bubble(gtx layout.Context, th *material.Theme, text string, isUser bool) layout.Dimensions {
	if text == "" {
		return layout.Dimensions{}
	}
	bg := c.pal.AsstBubble
	if isUser {
		bg = c.pal.UserBubble
	}
	align := layout.W
	if isUser {
		align = layout.E
	}
	pad := layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(4), Right: unit.Dp(4)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return align.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			maxW := gtx.Constraints.Max.X * 4 / 5
			gtx.Constraints.Max.X = maxW
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					rect := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(12))
					paint.FillShape(gtx.Ops, bg, rect.Op(gtx.Ops))
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}),
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, text)
						lbl.Color = c.pal.Fg
						return lbl.Layout(gtx)
					})
				}),
			)
		})
	})
}

func (c *Chat) errorBubble(gtx layout.Context, th *material.Theme, msg string) layout.Dimensions {
	pad := layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(4), Right: unit.Dp(4)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.W.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "error: "+msg)
			lbl.Color = c.pal.Danger
			return lbl.Layout(gtx)
		})
	})
}

func (c *Chat) composerRow(gtx layout.Context, th *material.Theme) layout.Dimensions {
	inset := layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(16), Left: unit.Dp(12), Right: unit.Dp(12)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(th, &c.composer, "Ask the orchestrator…")
				ed.Color = c.pal.Fg
				ed.HintColor = c.pal.Muted
				return ed.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &c.send, "Send")
				return btn.Layout(gtx)
			}),
		)
	})
}

func statusText(s wireclient.Status) string {
	switch s {
	case wireclient.StatusConnecting:
		return "connecting…"
	case wireclient.StatusOpen:
		return "online"
	case wireclient.StatusClosed:
		return "offline"
	case wireclient.StatusUnauthorized:
		return "unauthorized"
	default:
		return "idle"
	}
}

func statusColor(pal Palette, s wireclient.Status) color.NRGBA {
	switch s {
	case wireclient.StatusOpen:
		return color.NRGBA{R: 0x6b, G: 0xd1, B: 0x82, A: 0xff}
	case wireclient.StatusUnauthorized:
		return pal.Danger
	case wireclient.StatusClosed:
		return pal.Danger
	default:
		return pal.Muted
	}
}
