//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"context"
	"errors"
	"image/color"
	"log"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"
	"gioui.org/x/explorer"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/mobile/state"
	"github.com/mattcheramie/nomaddev/internal/wireclient"
)

// App is the top-level shell. It selects which screen to render based on
// the saved-token state, drives a wireclient.Session in a background
// goroutine, and rebuilds the frame whenever the store notifies it.
type App struct {
	store  *state.Store
	tokens state.TokenStore
	pal    Palette

	onboard  *Onboard
	chat     *Chat
	settings *Settings
	config   *Config
	approval *ApprovalSheet

	sessionMu sync.Mutex
	session   *wireclient.Session
	cancel    context.CancelFunc

	// Explorer is set the first time Run binds to a window; subsequent
	// pick requests reuse it. Nil during onboarding and across
	// reconnects — the picker goroutine guards on this.
	explorerMu   sync.Mutex
	explorer     *explorer.Explorer
	pickInFlight bool
}

// NewApp wires the shell. The TokenStore is used to load credentials on
// launch and to persist whatever the user types on the Onboard screen.
func NewApp(store *state.Store, tokens state.TokenStore) *App {
	pal := DefaultPalette()
	a := &App{
		store:    store,
		tokens:   tokens,
		pal:      pal,
		onboard:  NewOnboard(pal),
		chat:     NewChat(pal),
		settings: NewSettings(pal),
		config:   NewConfig(pal),
		approval: NewApprovalSheet(pal),
	}
	a.onboard.Submit = a.connect
	a.chat.Submit = a.sendIntent
	a.chat.AttachImage = a.openImagePicker
	a.chat.RemoveImage = a.store.RemovePendingImage
	a.chat.OpenSettings = func() { a.store.SetScreen(state.ScreenSettings) }
	a.settings.OnBack = func() { a.store.SetScreen(state.ScreenChat) }
	a.settings.OnSelectModel = a.setModel
	a.settings.OnResetHistory = a.resetHistory
	a.settings.OnForceReconnect = a.forceReconnect
	a.settings.OnOpenConfig = func() {
		a.store.SetScreen(state.ScreenConfig)
		a.refreshConfig() // kick a fetch when the user opens the screen
	}
	a.settings.OnSignOut = a.signOut
	a.config.OnBack = func() { a.store.SetScreen(state.ScreenSettings) }
	a.config.OnRefresh = a.refreshConfig
	a.config.Apply = a.applyConfig
	a.config.Reauth = a.signOut
	a.approval.Approve = a.approveTopApproval
	a.approval.Deny = a.denyTopApproval
	if url, token, err := tokens.Load(); err == nil {
		a.onboard.SetCredentials(url, token)
		a.store.SetCredentials(url, token)
	}
	return a
}

// Run drives the Gio event loop until the window closes. It blocks; call
// it from a dedicated goroutine.
func (a *App) Run(w *app.Window) error {
	th := NewTheme()
	pal := a.pal

	// Register the cross-platform file picker against this window. The
	// returned *Explorer is goroutine-safe; the picker callback runs
	// off the UI thread.
	exp := explorer.NewExplorer(w)
	a.explorerMu.Lock()
	a.explorer = exp
	a.explorerMu.Unlock()

	// Rebuild on every store notification so the WS goroutine can update
	// turns and statuses without polling.
	ch, unsub := a.store.Subscribe()
	defer unsub()
	go func() {
		for range ch {
			w.Invalidate()
		}
	}()

	// Wake the window every second so the approval countdown stays smooth
	// even when nothing else changes. Cheap — Invalidate just nudges Gio
	// to re-run the frame loop. We exit the goroutine when the window
	// closes by piggy-backing on the store subscription's lifetime: when
	// Run returns, unsub fires and the ticker leaks for at most one beat
	// before the process exits (Gio app.Main owns the lifetime).
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			w.Invalidate()
		}
	}()

	var ops op.Ops
	for {
		evt := w.Event()
		// The image picker needs every Gio event to track the Android
		// activity / iOS view controller lifecycle. Call it before the
		// type switch so even events we don't otherwise handle reach
		// the explorer.
		exp.ListenEvents(evt)
		switch e := evt.(type) {
		case app.DestroyEvent:
			a.stopSession()
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			a.handleKeys(gtx)
			paint.Fill(gtx.Ops, pal.Bg)
			snap := a.store.Snapshot()
			if snap.Token == "" {
				a.onboard.Layout(gtx, th, snap.LastError)
			} else {
				switch snap.Screen {
				case state.ScreenSettings:
					a.settings.Layout(gtx, th, snap, a.outboxLen())
				case state.ScreenConfig:
					a.config.Layout(gtx, th)
				default:
					a.chat.Layout(gtx, th, snap)
				}
				if len(snap.PendingApprovals) > 0 && snap.Screen == state.ScreenChat {
					a.approval.Layout(gtx, th, snap.PendingApprovals[0], time.Now())
				}
			}
			a.statusOverlay(gtx, th, snap)
			e.Frame(gtx.Ops)
		}
	}
}

func (a *App) statusOverlay(gtx layout.Context, th *material.Theme, snap state.State) {
	if snap.LastError == "" || snap.Token == "" {
		return
	}
	layout.SE.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, snap.LastError)
			lbl.Color = color.NRGBA{R: 0xff, G: 0x6b, B: 0x6b, A: 0xff}
			return lbl.Layout(gtx)
		})
	})
}

// handleKeys lets the Back key on Android return to the Onboard screen by
// signing out. It is a no-op when no token is saved.
func (a *App) handleKeys(gtx layout.Context) {
	for {
		ev, ok := gtx.Event(key.Filter{Name: key.NameBack})
		if !ok {
			break
		}
		if k, ok := ev.(key.Event); ok && k.State == key.Press {
			a.signOut()
		}
	}
}

func (a *App) connect(serverURL, token string) {
	if serverURL == "" || token == "" {
		a.store.SetLastError("server URL and token are both required")
		return
	}
	if err := a.tokens.Save(serverURL, token); err != nil {
		log.Printf("nomaddev: save token: %v", err)
	}
	a.store.SetLastError("")
	a.store.SetCredentials(serverURL, token)
	a.startSession(serverURL, token)
}

func (a *App) signOut() {
	a.stopSession()
	if err := a.tokens.Clear(); err != nil {
		log.Printf("nomaddev: clear token: %v", err)
	}
	a.store.SetCredentials("", "")
}

func (a *App) startSession(serverURL, token string) {
	a.stopSession()
	ctx, cancel := context.WithCancel(context.Background())
	sess := wireclient.NewSession(wireclient.SessionConfig{
		Dial:        wireclient.DialOptions{URL: serverURL, Token: token},
		LastEventID: a.store.Snapshot().LastEventID,
		OnStatus: func(s wireclient.Status) {
			a.store.SetStatus(s)
		},
		OnEnvelope: func(env event.Envelope) {
			state.Ingest(a.store, env)
		},
	})
	a.sessionMu.Lock()
	a.session = sess
	a.cancel = cancel
	a.sessionMu.Unlock()
	go func() {
		if err := sess.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("nomaddev: session: %v", err)
		}
	}()
}

func (a *App) stopSession() {
	a.sessionMu.Lock()
	sess := a.session
	cancel := a.cancel
	a.session = nil
	a.cancel = nil
	a.sessionMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if sess != nil {
		sess.Close()
	}
}

// approveTopApproval sends a tool.approval.granted reply for the
// front-of-queue pending approval and removes it from the store. The
// envelope's correlation_id is the original tool.approval.request.id so
// the orchestrator matches the reply back to its pending command.
func (a *App) approveTopApproval() {
	snap := a.store.Snapshot()
	if len(snap.PendingApprovals) == 0 {
		return
	}
	req, ok := a.store.PopApproval(snap.PendingApprovals[0].EnvelopeID)
	if !ok {
		return
	}
	env, err := event.NewReply(event.EventToolApprovalGranted, req.EnvelopeID, event.ToolApprovalGrantedPayload{})
	if err != nil {
		a.store.SetLastError(err.Error())
		return
	}
	a.sendOrError(env)
}

// denyTopApproval is the deny-button equivalent of approveTopApproval. The
// optional reason text is forwarded to the orchestrator and surfaces in
// the eventual command.result error_message.
func (a *App) denyTopApproval(reason string) {
	snap := a.store.Snapshot()
	if len(snap.PendingApprovals) == 0 {
		return
	}
	req, ok := a.store.PopApproval(snap.PendingApprovals[0].EnvelopeID)
	if !ok {
		return
	}
	env, err := event.NewReply(event.EventToolApprovalDenied, req.EnvelopeID, event.ToolApprovalDeniedPayload{Reason: reason})
	if err != nil {
		a.store.SetLastError(err.Error())
		return
	}
	a.sendOrError(env)
}

func (a *App) sendOrError(env event.Envelope) {
	a.sessionMu.Lock()
	sess := a.session
	a.sessionMu.Unlock()
	if sess == nil {
		a.store.SetLastError("not connected")
		return
	}
	if err := sess.Send(env); err != nil {
		a.store.SetLastError(err.Error())
	}
}

func (a *App) sendIntent(text string) {
	a.sessionMu.Lock()
	sess := a.session
	a.sessionMu.Unlock()
	// Atomically drain pending attachments so a stray frame can't
	// double-send them, and so the composer clears even on send error.
	images := a.store.TakePendingImages()
	env, err := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: text, Images: images})
	if err != nil {
		a.store.SetLastError(err.Error())
		return
	}
	a.store.RecordSentIntent(env.ID, text, images)
	if sess == nil {
		a.store.SetLastError("not connected")
		return
	}
	if err := sess.Send(env); err != nil {
		a.store.SetLastError(err.Error())
	}
}

// outboxLen returns the current outbox depth so the Settings screen can
// render the "N queued" indicator. Returns 0 when no session is alive.
func (a *App) outboxLen() int {
	a.sessionMu.Lock()
	sess := a.session
	a.sessionMu.Unlock()
	if sess == nil {
		return 0
	}
	return sess.OutboxLen()
}

// resetHistory clears the local chat surface immediately for snappy UX
// and sends a user.command{reset_history} envelope so the orchestrator
// wipes its server-side history too. The session token tickers reset as
// a side-effect of ResetTurns since they belong to the same session.
func (a *App) resetHistory() {
	a.store.ResetTurns()
	env, err := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{Action: event.UserCommandResetHistory})
	if err != nil {
		a.store.SetLastError(err.Error())
		return
	}
	a.sendOrError(env)
}

// setModel sends user.command{set_model, model}. The ack reducer in
// state.Ingest sets the active model when the server accepts; on
// rejection the orchestrator returns an ack with Error set and the
// reducer leaves Model alone.
func (a *App) setModel(model string) {
	env, err := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandSetModel,
		Model:  model,
	})
	if err != nil {
		a.store.SetLastError(err.Error())
		return
	}
	a.sendOrError(env)
}

// forceReconnect tears the current session down and rebuilds it against
// the credentials currently on file. The next FrameEvent picks up the
// new session via the same store-subscription path.
func (a *App) forceReconnect() {
	snap := a.store.Snapshot()
	if snap.ServerURL == "" || snap.Token == "" {
		return
	}
	a.startSession(snap.ServerURL, snap.Token)
}

// refreshConfig kicks a GET /admin/config in the background. The viewer
// widget owns the snapshot + loading + error state internally so the App
// only needs to hand it values via the setters.
func (a *App) refreshConfig() {
	snap := a.store.Snapshot()
	if snap.ServerURL == "" || snap.Token == "" {
		a.config.SetError("not connected")
		return
	}
	client, err := state.NewAdminClient(snap.ServerURL, snap.Token)
	if err != nil {
		a.config.SetError(err.Error())
		return
	}
	a.config.SetLoading(true)
	go func() {
		defer a.config.SetLoading(false)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cs, err := client.FetchConfig(ctx)
		if err != nil {
			a.config.SetError(err.Error())
			return
		}
		a.config.SetSnapshot(cs)
	}()
}

// restartBudget bounds how long the apply flow waits for the orchestrator
// to come back after POST /admin/config/restart. Matches RESTART_BUDGET_MS
// in mobile/src/screens/ConfigScreen.tsx so users see the same timing on
// both clients.
const (
	restartBudget   = 35 * time.Second
	restartPollEvery = 2500 * time.Millisecond
)

// applyConfig drives the full PUT → POST /admin/config/restart →
// polling-reconnect sequence on a background goroutine. The Config widget
// reflects every phase via SetPhase / SetBanner / SetFieldError so the
// UI thread only renders state — never blocks on the network.
func (a *App) applyConfig(changes map[string]string) {
	snap := a.store.Snapshot()
	if snap.ServerURL == "" || snap.Token == "" {
		a.config.SetBanner("not connected", "err")
		return
	}
	client, err := state.NewAdminClient(snap.ServerURL, snap.Token)
	if err != nil {
		a.config.SetBanner(err.Error(), "err")
		return
	}
	a.config.SetPhase(ConfigPhaseApplying)
	a.config.SetBanner("Applying configuration…", "info")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := client.ApplyConfig(ctx, changes, nil); err != nil {
			a.handleApplyError(err)
			return
		}
		// PUT succeeded — fire the restart.
		if err := client.RestartOrchestrator(ctx); err != nil {
			a.config.SetPhase(ConfigPhaseFailed)
			a.config.SetBanner("Settings saved but restart failed: "+err.Error(), "err")
			return
		}
		// Tear our current session down so the polling loop dials
		// fresh against the restarted orchestrator.
		a.store.SetRestartPending(true)
		a.config.SetPhase(ConfigPhaseRestarting)
		a.config.SetBanner("Restarting orchestrator…", "info")
		a.stopSession()
		a.driveRestartPolling(snap.ServerURL, snap.Token)
	}()
}

func (a *App) handleApplyError(err error) {
	var ae *state.ApplyConfigError
	if errors.As(err, &ae) {
		a.config.SetPhase(ConfigPhaseIdle)
		if ae.Status == 401 {
			a.config.SetPhase(ConfigPhaseReauth)
			a.config.SetBanner("Token is no longer accepted — sign back in.", "err")
			return
		}
		if ae.EnvVar != "" {
			a.config.SetFieldError(ae.EnvVar, ae.Message)
			a.config.SetBanner("A setting was rejected — see the highlighted field.", "err")
			return
		}
		a.config.SetBanner(ae.Message, "err")
		return
	}
	a.config.SetPhase(ConfigPhaseIdle)
	a.config.SetBanner(err.Error(), "err")
}

// driveRestartPolling re-dials the orchestrator every restartPollEvery
// until either a fresh hello clears RestartPending (success), the
// session status flips to unauthorized (the JWT secret rotated → reauth),
// or restartBudget elapses (failure). Runs on the goroutine the apply
// path spawned; the UI thread only reads phase / banner.
func (a *App) driveRestartPolling(serverURL, token string) {
	deadline := time.Now().Add(restartBudget)
	// Start a fresh session immediately; wireclient.Session handles its
	// own dial/backoff/reconnect so we don't hammer the orchestrator
	// while it's coming back up.
	a.startSession(serverURL, token)
	for {
		time.Sleep(restartPollEvery)
		snap := a.store.Snapshot()
		if !snap.RestartPending && snap.Status == wireclient.StatusOpen {
			a.config.SetPhase(ConfigPhaseApplied)
			a.config.SetBanner("Configuration applied and the orchestrator is back online.", "ok")
			a.refreshConfig()
			return
		}
		if snap.Status == wireclient.StatusUnauthorized {
			a.config.SetPhase(ConfigPhaseReauth)
			a.config.SetBanner("JWT secret rotated — sign back in.", "err")
			return
		}
		if time.Now().After(deadline) {
			a.config.SetPhase(ConfigPhaseFailed)
			a.config.SetBanner("Orchestrator did not come back in time. Check the server logs.", "err")
			return
		}
	}
}

// openImagePicker spawns the platform image chooser on a background
// goroutine, decodes the result via state.DecodeImageAttachment, and
// pushes the wire-ready ImageInput onto the store's pending queue.
// Concurrent picker calls are silently ignored — gioui.org/x/explorer
// only supports one open dialog per window.
func (a *App) openImagePicker() {
	a.explorerMu.Lock()
	exp := a.explorer
	if exp == nil || a.pickInFlight {
		a.explorerMu.Unlock()
		return
	}
	a.pickInFlight = true
	a.explorerMu.Unlock()

	go func() {
		defer func() {
			a.explorerMu.Lock()
			a.pickInFlight = false
			a.explorerMu.Unlock()
		}()
		f, err := exp.ChooseFile(".jpg", ".jpeg", ".png", ".gif", ".webp")
		if err != nil {
			if errors.Is(err, explorer.ErrUserDecline) {
				return // user cancelled — nothing to surface
			}
			a.store.SetLastError("pick image: " + err.Error())
			return
		}
		defer f.Close()
		hint := ""
		if named, ok := f.(interface{ Name() string }); ok {
			hint = named.Name()
		}
		img, decoded, err := state.DecodeImageAttachment(f, hint)
		if err != nil {
			a.store.SetLastError(err.Error())
			return
		}
		if err := a.store.AddPendingImage(img, decoded); err != nil {
			a.store.SetLastError(err.Error())
			return
		}
		a.store.SetLastError("")
	}()
}
