//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"strings"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/mattcheramie/nomaddev/internal/mobile/state"
)

// ApprovalSheet is the modal overlay shown while a tool.approval.request is
// pending. The orchestrator gates destructive tools on a typed-confirmation
// step (matching the SPA): the operator must type the exact tool name into
// the confirmation field before the Approve button enables. A countdown
// derived from the request deadline ticks once per second.
type ApprovalSheet struct {
	pal Palette

	confirm widget.Editor
	reason  widget.Editor
	approve widget.Clickable
	deny    widget.Clickable

	currentTool string

	// Approve / Deny are invoked when the user taps the corresponding
	// button. Approve only fires once the typed confirmation matches the
	// tool name; Deny passes the optional reason text the user supplied.
	Approve func()
	Deny    func(reason string)
}

// NewApprovalSheet returns an empty sheet. Layout is given fresh data on
// every frame from the store snapshot.
func NewApprovalSheet(pal Palette) *ApprovalSheet {
	s := &ApprovalSheet{pal: pal}
	s.confirm.SingleLine = true
	s.reason.SingleLine = true
	return s
}

// Layout draws the sheet for the given approval request. The caller passes
// the current time so a test can pin the countdown without sleeping.
func (s *ApprovalSheet) Layout(gtx layout.Context, th *material.Theme, req state.ApprovalRequest, now time.Time) layout.Dimensions {
	if req.Tool != s.currentTool {
		// Reset the confirm field whenever the sheet swaps to a new
		// approval — otherwise an operator who typed the previous
		// tool name could one-tap through the next one.
		s.confirm.SetText("")
		s.reason.SetText("")
		s.currentTool = req.Tool
	}

	// Scrim darkens the chat surface behind the sheet. Gio's Z-ordering
	// ensures the widgets we paint later (Approve / Deny / confirm field)
	// own the pointer hit-test within their own bounds; taps in the
	// scrim itself fall through to whatever the chat surface had under
	// the cursor — a minor polish item, not a correctness one.
	paint.Fill(gtx.Ops, color.NRGBA{R: 0, G: 0, B: 0, A: 0xa0})

	confirmed := strings.EqualFold(strings.TrimSpace(s.confirm.Text()), req.Tool)
	if s.deny.Clicked(gtx) && s.Deny != nil {
		s.Deny(s.reason.Text())
	}
	if confirmed && s.approve.Clicked(gtx) && s.Approve != nil {
		s.Approve()
	}

	return layout.S.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Max.Y = gtx.Constraints.Max.Y * 4 / 5
		return s.card(gtx, th, req, now, confirmed)
	})
}

func (s *ApprovalSheet) card(gtx layout.Context, th *material.Theme, req state.ApprovalRequest, now time.Time, confirmed bool) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rect := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(12))
			paint.FillShape(gtx.Ops, s.pal.Surface, rect.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(s.title(th)),
					layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
					layout.Rigid(s.toolRow(th, req)),
					layout.Rigid(s.maybeReason(th, req)),
					layout.Rigid(s.maybePreview(th, req)),
					layout.Rigid(s.argsBlock(th, req)),
					layout.Rigid(s.countdown(th, req, now)),
					layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
					layout.Rigid(s.confirmField(th, req)),
					layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
					layout.Rigid(s.reasonField(th)),
					layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
					layout.Rigid(s.actions(th, confirmed)),
				)
			})
		}),
	)
}

func (s *ApprovalSheet) title(th *material.Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		t := material.H6(th, "Approval required")
		t.Color = s.pal.Fg
		return t.Layout(gtx)
	}
}

func (s *ApprovalSheet) toolRow(th *material.Theme, req state.ApprovalRequest) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body1(th, req.Tool)
				lbl.Color = s.pal.Fg
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !strings.HasPrefix(req.Tool, "github_") {
					return layout.Dimensions{}
				}
				return s.badge(gtx, th, "GITHUB", color.NRGBA{R: 0x6f, G: 0x42, B: 0xc1, A: 0xff})
			}),
		)
	}
}

func (s *ApprovalSheet) badge(gtx layout.Context, th *material.Theme, text string, bg color.NRGBA) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rect := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(4))
			paint.FillShape(gtx.Ops, bg, rect.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2), Left: unit.Dp(6), Right: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, text)
				lbl.Color = s.pal.Fg
				return lbl.Layout(gtx)
			})
		}),
	)
}

func (s *ApprovalSheet) maybeReason(th *material.Theme, req state.ApprovalRequest) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if req.Reason == "" {
			return layout.Dimensions{}
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(s.label(th, "Reason")),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, req.Reason)
				lbl.Color = s.pal.Fg
				return lbl.Layout(gtx)
			}),
		)
	}
}

func (s *ApprovalSheet) maybePreview(th *material.Theme, req state.ApprovalRequest) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if req.Preview == nil {
			return layout.Dimensions{}
		}
		loc := fmt.Sprintf("Diff preview — %s:%d", req.Preview.Path, req.Preview.LineNumber)
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(s.label(th, loc)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.diff(gtx, th, req.Preview.UnifiedDiff)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if req.Preview.VerifyCommand == "" {
					return layout.Dimensions{}
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(s.label(th, "Verify after apply (rollback on non-zero exit)")),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(th, req.Preview.VerifyCommand)
						lbl.Color = color.NRGBA{R: 0xfb, G: 0xbf, B: 0x24, A: 0xff}
						return lbl.Layout(gtx)
					}),
				)
			}),
		)
	}
}

func (s *ApprovalSheet) diff(gtx layout.Context, th *material.Theme, diffText string) layout.Dimensions {
	lines := strings.Split(strings.TrimRight(diffText, "\n"), "\n")
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, diffLineWidgets(th, s.pal, lines)...)
}

// diffLineWidgets returns one rigid widget per diff line, colourised per the
// SPA convention: header/hunk muted, added green, removed red, context fg.
func diffLineWidgets(th *material.Theme, pal Palette, lines []string) []layout.FlexChild {
	out := make([]layout.FlexChild, 0, len(lines))
	for _, ln := range lines {
		ln := ln
		out = append(out, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, ln)
			lbl.Color = diffColor(pal, ln)
			return lbl.Layout(gtx)
		}))
	}
	return out
}

func diffColor(pal Palette, line string) color.NRGBA {
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "@@"):
		return pal.Muted
	case strings.HasPrefix(line, "+"):
		return color.NRGBA{R: 0x7e, G: 0xe7, B: 0x87, A: 0xff}
	case strings.HasPrefix(line, "-"):
		return color.NRGBA{R: 0xff, G: 0x7b, B: 0x72, A: 0xff}
	default:
		return pal.Fg
	}
}

func (s *ApprovalSheet) argsBlock(th *material.Theme, req state.ApprovalRequest) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		b, err := json.MarshalIndent(req.Args, "", "  ")
		if err != nil {
			b = []byte("<args unserialisable>")
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(s.label(th, "Args")),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, string(b))
				lbl.Color = s.pal.Fg
				return lbl.Layout(gtx)
			}),
		)
	}
}

func (s *ApprovalSheet) countdown(th *material.Theme, req state.ApprovalRequest, now time.Time) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		remaining := req.DeadlineUnixMs - now.UnixMilli()
		if remaining < 0 {
			remaining = 0
		}
		seconds := (remaining + 999) / 1000
		lbl := material.Body2(th, fmt.Sprintf("Time left: %ds", seconds))
		lbl.Color = color.NRGBA{R: 0xfb, G: 0xbf, B: 0x24, A: 0xff}
		return lbl.Layout(gtx)
	}
}

func (s *ApprovalSheet) confirmField(th *material.Theme, req state.ApprovalRequest) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(s.label(th, "Type "+req.Tool+" to enable Approve")),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(th, &s.confirm, req.Tool)
				ed.Color = s.pal.Fg
				ed.HintColor = s.pal.Muted
				return ed.Layout(gtx)
			}),
		)
	}
}

func (s *ApprovalSheet) reasonField(th *material.Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(s.label(th, "Deny reason (optional)")),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(th, &s.reason, "")
				ed.Color = s.pal.Fg
				ed.HintColor = s.pal.Muted
				return ed.Layout(gtx)
			}),
		)
	}
}

func (s *ApprovalSheet) actions(th *material.Theme, confirmed bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceBetween}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &s.deny, "Deny")
				btn.Background = s.pal.Danger
				return btn.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &s.approve, "Approve")
				if confirmed {
					btn.Background = color.NRGBA{R: 0x16, G: 0xa3, B: 0x4a, A: 0xff}
				} else {
					btn.Background = color.NRGBA{R: 0x37, G: 0x41, B: 0x51, A: 0xff}
				}
				return btn.Layout(gtx)
			}),
		)
	}
}

func (s *ApprovalSheet) label(th *material.Theme, text string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(th, text)
		lbl.Color = s.pal.Muted
		return lbl.Layout(gtx)
	}
}
