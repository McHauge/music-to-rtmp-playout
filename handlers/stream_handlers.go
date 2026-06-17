package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/starfederation/datastar-go/datastar"
)

var (
	errClipNotFound = errors.New("clip not found")
	errNoPlaylist   = errors.New("playlist not found or empty")
)

// StartStream begins streaming the given playlist (?id=).
func (app *App) StartStream(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)

	pl, _ := app.Flow.GetPlaylist(id)
	if pl == nil {
		sse.ConsoleError(errNoPlaylist)
		return
	}
	items, err := app.Flow.GetItems(id)
	if err != nil || len(items) == 0 {
		sse.ConsoleError(errNoPlaylist)
		return
	}
	st, err := app.Settings.Get()
	if err != nil {
		sse.ConsoleError(err)
		return
	}
	if err := app.Engine.Start(items, st); err != nil {
		sse.ConsoleError(err)
		return
	}
	app.patchStatus(sse)
}

// StopStream ends the live show.
func (app *App) StopStream(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	app.Engine.Stop()
	app.patchStatus(sse)
}

// SkipItem advances to the next flow item.
func (app *App) SkipItem(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	app.Engine.Skip()
	app.patchStatus(sse)
}

// PlayResume releases a manual hold/gate.
func (app *App) PlayResume(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	app.Engine.Play()
	app.patchStatus(sse)
}

// StreamStatus is a long-lived SSE connection that pushes status updates to the
// operator console as the show progresses.
func (app *App) StreamStatus(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	ch, unsub := app.Engine.Subscribe()
	defer unsub()

	// Initial paint.
	app.patchStatus(sse)

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			app.patchStatus(sse)
		}
	}
}

// patchStatus renders the status panel from the engine snapshot and patches it.
func (app *App) patchStatus(sse *datastar.ServerSentEventGenerator) {
	status := app.Engine.Status()
	out, err := app.Tmpl.Render("stream-status", map[string]any{
		"Status":  status,
		"Running": app.Engine.Running(),
	})
	if err != nil {
		sse.ConsoleError(err)
		return
	}
	sse.PatchElements(out)
}
