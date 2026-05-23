//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"gioui.org/layout"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/mattcheramie/nomaddev/internal/mobile/state"
)

// ConfigPhase is the editor's apply-flow state machine. The SPA tracks
// the same five phases at mobile/src/screens/ConfigScreen.tsx — keep the
// labels aligned so users moving between surfaces see the same vocabulary.
type ConfigPhase string

const (
	ConfigPhaseIdle       ConfigPhase = "idle"
	ConfigPhaseApplying   ConfigPhase = "applying"
	ConfigPhaseRestarting ConfigPhase = "restarting"
	ConfigPhaseApplied    ConfigPhase = "applied"
	ConfigPhaseReauth     ConfigPhase = "reauth"
	ConfigPhaseFailed     ConfigPhase = "failed"
)

// Config renders the schema-driven settings editor. The screen tracks
// dirty fields locally, batches them into a single PUT /admin/config on
// Apply, and follows up with POST /admin/config/restart. While the
// restart is in flight the App shell polls for a fresh hello; the
// reducer clears RestartPending on hello which the editor reads to
// transition to ConfigPhaseApplied.
type Config struct {
	pal Palette

	list    widget.List
	back    widget.Clickable
	refresh widget.Clickable
	apply   widget.Clickable
	confirm widget.Clickable
	revertAll widget.Clickable
	signOut widget.Clickable

	toggles map[string]*widget.Clickable
	open    map[string]bool

	// Per-row state — re-created on every fetch so memory does not
	// accrue indefinitely if the orchestrator's schema grows.
	editors    map[string]*widget.Editor
	cycleClicks map[string]*widget.Clickable
	revertClicks map[string]*widget.Clickable

	mu          sync.RWMutex
	snapshot    state.ConfigSnapshot
	loading     bool
	lastError   string
	pending     map[string]string // env_var → pending new value (empty string allowed for "set to empty")
	fieldErrors map[string]string // env_var → server-rejected error message
	banner      string
	bannerTone  string // "ok" | "err" | "info"
	phase       ConfigPhase
	dangerArmed bool // operator has acknowledged the dangerous-changes confirmation gate

	// Apply and Reauth are wired by the App shell. Apply runs the
	// PUT + POST + polling sequence on a background goroutine;
	// Reauth signs the user out so they re-enter their JWT.
	Apply     func(changes map[string]string)
	Reauth    func()
	OnBack    func()
	OnRefresh func()
}

// NewConfig returns an empty config editor.
func NewConfig(pal Palette) *Config {
	c := &Config{
		pal:          pal,
		toggles:      map[string]*widget.Clickable{},
		open:         map[string]bool{},
		editors:      map[string]*widget.Editor{},
		cycleClicks:  map[string]*widget.Clickable{},
		revertClicks: map[string]*widget.Clickable{},
		pending:      map[string]string{},
		fieldErrors:  map[string]string{},
		phase:        ConfigPhaseIdle,
	}
	c.list.Axis = layout.Vertical
	return c
}

// SetLoading flips the refresh button's label while a fetch is in flight.
func (c *Config) SetLoading(v bool) {
	c.mu.Lock()
	c.loading = v
	c.mu.Unlock()
}

// SetSnapshot stores the most recent successful fetch and resets the
// editor's per-field caches so widgets bind to the new shape.
func (c *Config) SetSnapshot(snap state.ConfigSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = snap
	c.lastError = ""
	// Clear per-row widgets so a re-fetch (post-restart) seeds editors
	// with the fresh values instead of the operator's last typed-text.
	c.editors = map[string]*widget.Editor{}
	c.cycleClicks = map[string]*widget.Clickable{}
	c.revertClicks = map[string]*widget.Clickable{}
	c.pending = map[string]string{}
	c.fieldErrors = map[string]string{}
	c.dangerArmed = false
}

// SetError surfaces a fetch failure to the operator. Pass "" to clear.
func (c *Config) SetError(msg string) {
	c.mu.Lock()
	c.lastError = msg
	c.mu.Unlock()
}

// SetPhase moves the editor through its apply-flow state machine. The
// App shell calls this from the apply goroutine.
func (c *Config) SetPhase(p ConfigPhase) {
	c.mu.Lock()
	c.phase = p
	if p == ConfigPhaseApplied {
		// Successful apply — clear the pending set and field errors so
		// the next render shows a clean editor.
		c.pending = map[string]string{}
		c.fieldErrors = map[string]string{}
		c.dangerArmed = false
	}
	c.mu.Unlock()
}

// Phase returns the current apply-flow phase. Used by the App shell to
// decide whether to re-fetch after the restart succeeds.
func (c *Config) Phase() ConfigPhase {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.phase
}

// SetBanner shows a status message above the categories. Tone selects
// the foreground colour; pass "" to clear.
func (c *Config) SetBanner(text, tone string) {
	c.mu.Lock()
	c.banner = text
	c.bannerTone = tone
	c.mu.Unlock()
}

// SetFieldError marks one env var as rejected. The category containing
// it auto-expands on the next render so the user sees the offending row.
func (c *Config) SetFieldError(envVar, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if msg == "" {
		delete(c.fieldErrors, envVar)
		return
	}
	c.fieldErrors[envVar] = msg
	for _, s := range c.snapshot.Settings {
		if s.EnvVar == envVar {
			c.open[s.Category] = true
			break
		}
	}
}

// Layout draws the config editor.
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
	pending := copyStringMap(c.pending)
	fieldErrs := copyStringMap(c.fieldErrors)
	banner := c.banner
	bannerTone := c.bannerTone
	phase := c.phase
	dangerArmed := c.dangerArmed
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

	c.processPendingClicks(gtx, snap, pending)

	if c.confirm.Clicked(gtx) {
		c.mu.Lock()
		c.dangerArmed = true
		c.mu.Unlock()
		dangerArmed = true
	}
	if c.revertAll.Clicked(gtx) {
		c.revertAllPending()
		pending = map[string]string{}
	}
	if c.signOut.Clicked(gtx) && c.Reauth != nil {
		c.Reauth()
	}
	busy := phase == ConfigPhaseApplying || phase == ConfigPhaseRestarting
	hasDangerous := dangerousPending(snap, pending)
	canApply := len(pending) > 0 && !busy && (!hasDangerous || dangerArmed)
	if c.apply.Clicked(gtx) && canApply && c.Apply != nil {
		c.Apply(copyStringMap(pending))
	}

	rows := c.buildRows(th, snap, cats, pending, fieldErrs, banner, bannerTone, phase, hasDangerous, dangerArmed, busy, loading, lastErr)
	inset := layout.Inset{Top: unit.Dp(36), Bottom: unit.Dp(24), Left: unit.Dp(16), Right: unit.Dp(16)}
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		ml := material.List(th, &c.list)
		return ml.Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
			return rows[i](gtx)
		})
	})
}

func (c *Config) processPendingClicks(gtx layout.Context, snap state.ConfigSnapshot, pending map[string]string) {
	for _, s := range snap.Settings {
		envVar := s.EnvVar
		// Cycle-tap for enum + bool fields.
		if click, ok := c.cycleClicks[envVar]; ok && click.Clicked(gtx) {
			next := nextCycleValue(s, c.effectiveValue(s, pending))
			c.recordPending(s, next)
		}
		// Revert tap for any field.
		if click, ok := c.revertClicks[envVar]; ok && click.Clicked(gtx) {
			c.mu.Lock()
			delete(c.pending, envVar)
			delete(c.fieldErrors, envVar)
			c.mu.Unlock()
		}
		// Free-text editors: persist the typed value into pending whenever
		// it changes. We avoid filing identical values so a quick edit-and-
		// undo doesn't keep the apply button armed.
		if ed, ok := c.editors[envVar]; ok {
			text := ed.Text()
			if text != s.Value {
				c.recordPending(s, text)
			} else {
				c.mu.Lock()
				delete(c.pending, envVar)
				c.mu.Unlock()
			}
		}
	}
}

func (c *Config) recordPending(s state.ConfigSetting, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value == s.Value {
		delete(c.pending, s.EnvVar)
		return
	}
	c.pending[s.EnvVar] = value
	delete(c.fieldErrors, s.EnvVar)
}

func (c *Config) revertAllPending() {
	c.mu.Lock()
	c.pending = map[string]string{}
	c.fieldErrors = map[string]string{}
	c.dangerArmed = false
	c.mu.Unlock()
	// Reset any editors that already buffered the user's typed value
	// back to the orchestrator's known-good value so the next frame
	// renders consistently.
	for _, s := range c.snapshot.Settings {
		if ed, ok := c.editors[s.EnvVar]; ok {
			ed.SetText(s.Value)
		}
	}
}

func (c *Config) effectiveValue(s state.ConfigSetting, pending map[string]string) string {
	if v, ok := pending[s.EnvVar]; ok {
		return v
	}
	return s.Value
}

func (c *Config) buildRows(
	th *material.Theme,
	snap state.ConfigSnapshot,
	byCat map[string][]state.ConfigSetting,
	pending map[string]string,
	fieldErrs map[string]string,
	banner, bannerTone string,
	phase ConfigPhase,
	hasDangerous, dangerArmed, busy, loading bool,
	lastErr string,
) []layout.Widget {
	rows := []layout.Widget{c.headerRow(th, loading)}
	if banner != "" {
		rows = append(rows, c.bannerRow(th, banner, bannerTone))
	}
	if lastErr != "" {
		rows = append(rows, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, lastErr)
			lbl.Color = c.pal.Danger
			return lbl.Layout(gtx)
		})
	}
	if phase == ConfigPhaseReauth {
		rows = append(rows, c.signOutRow(th))
	}
	if len(snap.Categories) == 0 && lastErr == "" && !loading {
		rows = append(rows, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "tap Refresh to load the orchestrator's effective configuration")
			lbl.Color = c.pal.Muted
			return lbl.Layout(gtx)
		})
	}
	for _, cat := range snap.Categories {
		cat := cat
		toggle := c.toggles[cat]
		open := c.open[cat]
		items := byCat[cat]
		dirtyCount := 0
		for _, s := range items {
			if _, ok := pending[s.EnvVar]; ok {
				dirtyCount++
			}
		}
		rows = append(rows, c.categoryHeader(th, cat, len(items), dirtyCount, toggle, open))
		if !open {
			continue
		}
		for _, s := range items {
			s := s
			rows = append(rows, c.settingRow(th, s, pending, fieldErrs))
		}
	}
	if len(pending) > 0 {
		rows = append(rows, c.applyFooter(th, snap, pending, hasDangerous, dangerArmed, busy))
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

func (c *Config) bannerRow(th *material.Theme, text, tone string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		col := c.pal.Muted
		switch tone {
		case "ok":
			col = c.pal.Accent
		case "err":
			col = c.pal.Danger
		}
		return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, text)
			lbl.Color = col
			return lbl.Layout(gtx)
		})
	}
}

func (c *Config) signOutRow(th *material.Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(th, &c.signOut, "Sign out and re-onboard")
			btn.Background = c.pal.Danger
			btn.Color = c.pal.Fg
			return btn.Layout(gtx)
		})
	}
}

func (c *Config) categoryHeader(th *material.Theme, name string, count, dirty int, click *widget.Clickable, open bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return material.ButtonLayout(th, click).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					prefix := "▸"
					if open {
						prefix = "▾"
					}
					label := fmt.Sprintf("%s %s (%d)", prefix, name, count)
					if dirty > 0 {
						label += fmt.Sprintf(" · %d changed", dirty)
					}
					lbl := material.Body1(th, label)
					lbl.Color = c.pal.Fg
					if dirty > 0 {
						lbl.Color = c.pal.Accent
					}
					return lbl.Layout(gtx)
				})
			})
		})
	}
}

func (c *Config) settingRow(th *material.Theme, s state.ConfigSetting, pending, fieldErrs map[string]string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return c.settingHeader(gtx, th, s, pending)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return c.settingEditor(gtx, th, s, pending)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if errMsg, ok := fieldErrs[s.EnvVar]; ok {
						lbl := material.Caption(th, "✗ "+errMsg)
						lbl.Color = c.pal.Danger
						return lbl.Layout(gtx)
					}
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

func (c *Config) settingHeader(gtx layout.Context, th *material.Theme, s state.ConfigSetting, pending map[string]string) layout.Dimensions {
	_, dirty := pending[s.EnvVar]
	meta := s.Type
	if s.Dangerous {
		meta = "⚠ " + meta
	}
	if s.ReadOnly {
		meta += " · read-only"
	}
	revertClick := c.revertClicks[s.EnvVar]
	if revertClick == nil {
		revertClick = &widget.Clickable{}
		c.revertClicks[s.EnvVar] = revertClick
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th, s.EnvVar)
					lbl.Color = c.pal.Fg
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Caption(th, meta)
					lbl.Color = c.pal.Muted
					if s.Dangerous {
						lbl.Color = c.pal.Danger
					}
					return lbl.Layout(gtx)
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !dirty {
				return layout.Dimensions{}
			}
			btn := material.Button(th, revertClick, "Revert")
			btn.Background = c.pal.Surface
			btn.Color = c.pal.Fg
			btn.Inset = layout.UniformInset(unit.Dp(4))
			return btn.Layout(gtx)
		}),
	)
}

func (c *Config) settingEditor(gtx layout.Context, th *material.Theme, s state.ConfigSetting, pending map[string]string) layout.Dimensions {
	current := c.effectiveValue(s, pending)
	switch {
	case s.ReadOnly:
		lbl := material.Body2(th, displayValue(s, current))
		lbl.Color = c.pal.Muted
		return lbl.Layout(gtx)
	case s.Type == "bool":
		return c.cycleField(gtx, th, s, current, []string{"true", "false"})
	case s.Type == "enum" && len(s.Enum) > 0:
		return c.cycleField(gtx, th, s, current, s.Enum)
	default:
		return c.textField(gtx, th, s, current)
	}
}

// cycleField renders a single tap-to-cycle button. The label is the current
// effective value; tapping advances to the next entry in the cycle list.
func (c *Config) cycleField(gtx layout.Context, th *material.Theme, s state.ConfigSetting, current string, cycle []string) layout.Dimensions {
	click := c.cycleClicks[s.EnvVar]
	if click == nil {
		click = &widget.Clickable{}
		c.cycleClicks[s.EnvVar] = click
	}
	btn := material.Button(th, click, displayValue(s, current))
	btn.Background = c.pal.Surface
	btn.Color = c.pal.Fg
	return btn.Layout(gtx)
}

// textField renders a single-line text editor. The orchestrator's
// schema covers int / float / duration / csv / int64 with free-text
// input — we hand all of them to the same widget and let the server's
// validator return a per-field error on apply.
func (c *Config) textField(gtx layout.Context, th *material.Theme, s state.ConfigSetting, current string) layout.Dimensions {
	ed := c.editors[s.EnvVar]
	if ed == nil {
		ed = &widget.Editor{SingleLine: true}
		ed.SetText(s.Value)
		c.editors[s.EnvVar] = ed
	}
	hint := ""
	if s.ValueState == "unset" {
		hint = "(unset)"
	} else if s.Default != "" {
		hint = "default: " + s.Default
	}
	mat := material.Editor(th, ed, hint)
	mat.Color = c.pal.Fg
	mat.HintColor = c.pal.Muted
	return mat.Layout(gtx)
}

func (c *Config) applyFooter(th *material.Theme, snap state.ConfigSnapshot, pending map[string]string, hasDangerous, dangerArmed, busy bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					summary := fmt.Sprintf("%d field(s) changed", len(pending))
					if hasDangerous {
						summary += " · ⚠ includes dangerous changes"
					}
					lbl := material.Body2(th, summary)
					lbl.Color = c.pal.Muted
					return lbl.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !hasDangerous || dangerArmed {
						return layout.Dimensions{}
					}
					btn := material.Button(th, &c.confirm, "Acknowledge dangerous changes")
					btn.Background = c.pal.Surface
					btn.Color = c.pal.Danger
					return btn.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceBetween}.Layout(gtx,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							btn := material.Button(th, &c.revertAll, "Revert all")
							btn.Background = c.pal.Surface
							btn.Color = c.pal.Fg
							return btn.Layout(gtx)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							label := "Apply & Restart"
							if busy {
								label = "Restarting…"
							}
							btn := material.Button(th, &c.apply, label)
							if !busy && (!hasDangerous || dangerArmed) {
								if hasDangerous {
									btn.Background = c.pal.Danger
								} else {
									btn.Background = c.pal.Accent
								}
							} else {
								btn.Background = c.pal.Surface
							}
							btn.Color = c.pal.Fg
							return btn.Layout(gtx)
						}),
					)
				}),
			)
		})
	}
}

// dangerousPending returns true when at least one staged change targets a
// setting flagged Dangerous in the schema. Used to gate the Apply button
// behind the acknowledgement step.
func dangerousPending(snap state.ConfigSnapshot, pending map[string]string) bool {
	for _, s := range snap.Settings {
		if _, ok := pending[s.EnvVar]; ok && s.Dangerous {
			return true
		}
	}
	return false
}

func nextCycleValue(s state.ConfigSetting, current string) string {
	cycle := s.Enum
	if s.Type == "bool" {
		cycle = []string{"true", "false"}
	}
	for i, v := range cycle {
		if strings.EqualFold(v, current) {
			return cycle[(i+1)%len(cycle)]
		}
	}
	if len(cycle) > 0 {
		return cycle[0]
	}
	return current
}

func displayValue(s state.ConfigSetting, current string) string {
	if current == "" {
		switch s.ValueState {
		case "set":
			return "(secret set)"
		case "unset":
			return "(unset)"
		default:
			return "—"
		}
	}
	return current
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

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
