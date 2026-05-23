//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"fmt"
	"sort"
	"sync"

	"gioui.org/layout"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/mattcheramie/nomaddev/internal/mobile/state"
)

// Config is the read-only viewer for /admin/config. It groups settings by
// category, with each category collapsible. M5 ships display only; M6+
// will add field editors, dirty tracking, and the Apply+Restart flow.
//
// Snapshot, loading flag, and last error live on the widget rather than
// the global Store because they are intrinsic to this screen's session —
// no other surface reads them and they reset when the user navigates
// away from the screen.
type Config struct {
	pal Palette

	list    widget.List
	back    widget.Clickable
	refresh widget.Clickable
	// One widget.Clickable + open bool per category. Both maps are keyed
	// on the category name and live for the widget's lifetime so a
	// re-fetch doesn't collapse the operator's drilldown.
	toggles map[string]*widget.Clickable
	open    map[string]bool

	mu        sync.RWMutex
	snapshot  state.ConfigSnapshot
	loading   bool
	lastError string

	OnBack    func()
	OnRefresh func()
}

// NewConfig returns an empty config viewer.
func NewConfig(pal Palette) *Config {
	c := &Config{
		pal:     pal,
		toggles: map[string]*widget.Clickable{},
		open:    map[string]bool{},
	}
	c.list.Axis = layout.Vertical
	return c
}

// SetLoading flips the spinner / disables the refresh button while a
// fetch is in flight. Safe to call from the fetch goroutine.
func (c *Config) SetLoading(v bool) {
	c.mu.Lock()
	c.loading = v
	c.mu.Unlock()
}

// SetSnapshot stores the most recent successful fetch. Safe from any
// goroutine.
func (c *Config) SetSnapshot(snap state.ConfigSnapshot) {
	c.mu.Lock()
	c.snapshot = snap
	c.lastError = ""
	c.mu.Unlock()
}

// SetError surfaces a fetch failure to the operator. Pass "" to clear.
func (c *Config) SetError(msg string) {
	c.mu.Lock()
	c.lastError = msg
	c.mu.Unlock()
}

// Layout draws the config viewer.
func (c *Config) Layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	paint.Fill(gtx.Ops, c.pal.Bg)
	if c.back.Clicked(gtx) && c.OnBack != nil {
		c.OnBack()
	}
	if c.refresh.Clicked(gtx) && c.OnRefresh != nil {
		c.OnRefresh()
	}

	c.mu.RLock()
	snap := c.snapshot
	loading := c.loading
	lastErr := c.lastError
	c.mu.RUnlock()

	cats := groupByCategory(snap)
	for _, cat := range snap.Categories {
		if _, ok := c.toggles[cat]; !ok {
			c.toggles[cat] = &widget.Clickable{}
		}
		if c.toggles[cat].Clicked(gtx) {
			c.open[cat] = !c.open[cat]
		}
	}

	rows := c.buildRows(th, cats, snap.Categories, loading, lastErr)
	inset := layout.Inset{Top: unit.Dp(36), Bottom: unit.Dp(24), Left: unit.Dp(16), Right: unit.Dp(16)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		ml := material.List(th, &c.list)
		return ml.Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
			return rows[i](gtx)
		})
	})
}

func (c *Config) buildRows(th *material.Theme, byCat map[string][]state.ConfigSetting, ordered []string, loading bool, lastErr string) []layout.Widget {
	rows := []layout.Widget{c.headerRow(th, loading)}
	if lastErr != "" {
		rows = append(rows, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, lastErr)
			lbl.Color = c.pal.Danger
			return lbl.Layout(gtx)
		})
	}
	if len(ordered) == 0 && lastErr == "" && !loading {
		rows = append(rows, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "tap Refresh to load the orchestrator's effective configuration")
			lbl.Color = c.pal.Muted
			return lbl.Layout(gtx)
		})
	}
	for _, cat := range ordered {
		cat := cat
		toggle := c.toggles[cat]
		open := c.open[cat]
		rows = append(rows, c.categoryHeader(th, cat, len(byCat[cat]), toggle, open))
		if !open {
			continue
		}
		for _, s := range byCat[cat] {
			s := s
			rows = append(rows, c.settingRow(th, s))
		}
	}
	return rows
}

func (c *Config) headerRow(th *material.Theme, loading bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				t := material.H5(th, "Server config")
				t.Color = c.pal.Fg
				return t.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "Refresh"
						if loading {
							label = "Loading…"
						}
						btn := material.Button(th, &c.refresh, label)
						btn.Background = c.pal.Surface
						btn.Color = c.pal.Fg
						return btn.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &c.back, "Back")
						btn.Background = c.pal.Surface
						btn.Color = c.pal.Fg
						return btn.Layout(gtx)
					}),
				)
			}),
		)
	}
}

func (c *Config) categoryHeader(th *material.Theme, name string, count int, click *widget.Clickable, open bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return material.ButtonLayout(th, click).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					prefix := "▸"
					if open {
						prefix = "▾"
					}
					lbl := material.Body1(th, fmt.Sprintf("%s %s (%d)", prefix, name, count))
					lbl.Color = c.pal.Fg
					return lbl.Layout(gtx)
				})
			})
		})
	}
}

func (c *Config) settingRow(th *material.Theme, s state.ConfigSetting) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th, s.EnvVar)
					lbl.Color = c.pal.Fg
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					meta := s.Type
					if s.Dangerous {
						meta = "⚠ " + meta
					}
					if s.ReadOnly {
						meta += " · read-only"
					}
					lbl := material.Caption(th, meta)
					lbl.Color = c.pal.Muted
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					value := s.Value
					if value == "" {
						switch s.ValueState {
						case "set":
							value = "(secret set)"
						case "unset":
							value = "(unset)"
						default:
							value = "—"
						}
					}
					lbl := material.Body2(th, value)
					lbl.Color = c.pal.Fg
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if s.Help == "" {
						return layout.Dimensions{}
					}
					lbl := material.Caption(th, s.Help)
					lbl.Color = c.pal.Muted
					return lbl.Layout(gtx)
				}),
			)
		})
	}
}

// groupByCategory bucketises settings by their Category. Within each
// bucket the rows are sorted by env-var name so the order is stable
// across fetches.
func groupByCategory(snap state.ConfigSnapshot) map[string][]state.ConfigSetting {
	out := map[string][]state.ConfigSetting{}
	for _, s := range snap.Settings {
		out[s.Category] = append(out[s.Category], s)
	}
	for cat := range out {
		sort.Slice(out[cat], func(i, j int) bool { return out[cat][i].EnvVar < out[cat][j].EnvVar })
	}
	return out
}
