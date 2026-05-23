//go:build android || ios || darwin || windows || (linux && nomaddev_mobile_desktop)

package ui

import (
	"context"
	"image/color"
	"log"
	"sync"

	"gioui.org/app"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"

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

	onboard *Onboard
	chat    *Chat

	sessionMu sync.Mutex
	session   *wireclient.Session
	cancel    context.CancelFunc
}

// NewApp wires the shell. The TokenStore is used to load credentials on
// launch and to persist whatever the user types on the Onboard screen.
func NewApp(store *state.Store, tokens state.TokenStore) *App {
	pal := DefaultPalette()
	a := &App{
		store:   store,
		tokens:  tokens,
		pal:     pal,
		onboard: NewOnboard(pal),
		chat:    NewChat(pal),
	}
	a.onboard.Submit = a.connect
	a.chat.Submit = a.sendIntent
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

	// Rebuild on every store notification so the WS goroutine can update
	// turns and statuses without polling.
	ch, unsub := a.store.Subscribe()
	defer unsub()
	go func() {
		for range ch {
			w.Invalidate()
		}
	}()

	var ops op.Ops
	for {
		switch e := w.Event().(type) {
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
				a.chat.Layout(gtx, th, snap)
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

func (a *App) sendIntent(text string) {
	a.sessionMu.Lock()
	sess := a.session
	a.sessionMu.Unlock()
	env, err := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: text})
	if err != nil {
		a.store.SetLastError(err.Error())
		return
	}
	a.store.RecordSentIntent(env.ID, text, nil)
	if sess == nil {
		a.store.SetLastError("not connected")
		return
	}
	if err := sess.Send(env); err != nil {
		a.store.SetLastError(err.Error())
	}
}
