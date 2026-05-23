//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"fmt"
	"image"
	"image/color"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/mobile/state"
)

// LiveTerminal renders the streamed output of one ToolCall. It auto-tails
// to the latest line while the operator hasn't scrolled away from the
// bottom; a "Jump to bottom" pill appears when they have. The elapsed-time
// header is anchored on the most recent sandbox.heartbeat and ticks
// locally between heartbeats so it never freezes mid-run.
type LiveTerminal struct {
	pal  Palette
	list widget.List
	jump widget.Clickable
}

// NewLiveTerminal returns a fresh terminal widget.
func NewLiveTerminal(pal Palette) *LiveTerminal {
	t := &LiveTerminal{pal: pal}
	t.list.Axis = layout.Vertical
	t.list.ScrollToEnd = true
	return t
}

// Layout draws the terminal for the given ToolCall. `now` is passed in so
// the elapsed-time extrapolation can be pinned in tests; in production
// callers pass time.Now().
func (t *LiveTerminal) Layout(gtx layout.Context, th *material.Theme, call state.ToolCall, anchor time.Time, now time.Time) layout.Dimensions {
	if t.jump.Clicked(gtx) {
		t.list.Position.BeforeEnd = false
		t.list.ScrollToEnd = true
	}
	atBottom := !t.list.Position.BeforeEnd
	// Re-arm auto-tail when the user scrolls back to the bottom themselves.
	if atBottom {
		t.list.ScrollToEnd = true
	}

	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return t.container(gtx, th, call, anchor, now)
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			if atBottom {
				return layout.Dimensions{}
			}
			return layout.S.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return t.jumpPill(gtx, th)
				})
			})
		}),
	)
}

func (t *LiveTerminal) container(gtx layout.Context, th *material.Theme, call state.ToolCall, anchor, now time.Time) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rect := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(6))
			paint.FillShape(gtx.Ops, t.pal.Surface, rect.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return t.header(gtx, th, call, anchor, now)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return t.lines(gtx, th, call.Lines)
				}),
			)
		}),
	)
}

func (t *LiveTerminal) header(gtx layout.Context, th *material.Theme, call state.ToolCall, anchor, now time.Time) layout.Dimensions {
	running := call.Result == nil
	elapsed := elapsedSince(call.ElapsedMs, anchor, now, running)

	dotColor := t.pal.Muted
	state := "done"
	if running {
		dotColor = color.NRGBA{R: 0x7e, G: 0xe7, B: 0x87, A: 0xff}
		state = "live"
	}

	rolledOff := call.LineCount - len(call.Lines)
	var right string
	if rolledOff > 0 {
		right = fmt.Sprintf("showing %d of %d", len(call.Lines), call.LineCount)
	} else {
		right = fmt.Sprintf("%d lines", call.LineCount)
	}

	inset := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(10), Right: unit.Dp(10)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return t.dot(gtx, dotColor)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Caption(th, state+" · "+formatElapsed(elapsed))
						lbl.Font.Typeface = "monospace"
						lbl.Color = t.pal.Muted
						return lbl.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, right)
				lbl.Color = t.pal.Muted
				return lbl.Layout(gtx)
			}),
		)
	})
}

func (t *LiveTerminal) dot(gtx layout.Context, c color.NRGBA) layout.Dimensions {
	sz := gtx.Dp(8)
	rect := clip.UniformRRect(image.Rectangle{Max: image.Point{X: sz, Y: sz}}, sz/2)
	paint.FillShape(gtx.Ops, c, rect.Op(gtx.Ops))
	return layout.Dimensions{Size: image.Point{X: sz, Y: sz}}
}

func (t *LiveTerminal) lines(gtx layout.Context, th *material.Theme, lines []state.TerminalLine) layout.Dimensions {
	inset := layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(8), Left: unit.Dp(10), Right: unit.Dp(10)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		ml := material.List(th, &t.list)
		return ml.Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
			ln := lines[i]
			c := color.NRGBA{R: 0xc9, G: 0xd1, B: 0xd9, A: 0xff}
			if ln.Stream == event.StreamStderr {
				c = color.NRGBA{R: 0xf8, G: 0x71, B: 0x71, A: 0xff}
			}
			text := ln.Text
			if text == "" {
				text = " "
			}
			lbl := material.Body2(th, text)
			lbl.Color = c
			lbl.Font.Typeface = "monospace"
			lbl.Font.Style = font.Regular
			return lbl.Layout(gtx)
		})
	})
}

func (t *LiveTerminal) jumpPill(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return material.ButtonLayout(th, &t.jump).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "↓ Jump to bottom")
			lbl.Color = t.pal.Fg
			return lbl.Layout(gtx)
		})
	})
}

// elapsedSince extrapolates the elapsed-time the LiveTerminal renders. The
// orchestrator's sandbox.heartbeat anchors ElapsedMs at a known wall-clock
// instant (`anchor`); between heartbeats we drift forward with the local
// clock so the display doesn't freeze. Once the command finishes, we
// return the server-reported value verbatim.
func elapsedSince(serverElapsedMs int64, anchor, now time.Time, running bool) int64 {
	if !running {
		return serverElapsedMs
	}
	delta := now.Sub(anchor).Milliseconds()
	if delta < 0 {
		delta = 0
	}
	return serverElapsedMs + delta
}

// formatElapsed mirrors mobile/src/components/LiveTerminal.tsx#formatElapsed:
// sub-second → "Nms", minutes → MM:SS, hours → H:MM:SS.
func formatElapsed(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	totalSec := ms / 1000
	sec := totalSec % 60
	min := (totalSec / 60) % 60
	hr := totalSec / 3600
	if hr > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hr, min, sec)
	}
	return fmt.Sprintf("%02d:%02d", min, sec)
}
