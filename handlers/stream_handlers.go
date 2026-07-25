package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/starfederation/datastar-go/datastar"

	"music-to-rtmp-playout/services"
	"music-to-rtmp-playout/services/playout"
)

var (
	errClipNotFound = errors.New("clip not found")
	errNoPlaylist   = errors.New("playlist not found or empty")
)

// StartStream begins streaming the given playlist (?id=), optionally from the
// flow item at ?at= (clamped by the engine).
func (app *App) StartStream(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	at, _ := strconv.Atoi(r.URL.Query().Get("at"))
	// Before NewSSE — the session cookie must go out with the response headers.
	app.rememberShow(w, r, id)
	sse := datastar.NewSSE(w, r)

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
	if err := app.Engine.Start(items, st, id, at); err != nil {
		sse.ConsoleError(err)
		return
	}
	app.patchStatus(sse)
	app.patchRundown(sse, 0)
}

// control runs an engine action, then re-patches the status panel and rundown
// over a fresh SSE. Every simple transport button funnels through here — they
// differ only in which Engine method fires.
func (app *App) control(w http.ResponseWriter, r *http.Request, action func()) {
	sse := datastar.NewSSE(w, r)
	action()
	app.patchStatus(sse)
	app.patchRundown(sse, 0)
}

// StopStream ends the live show.
func (app *App) StopStream(w http.ResponseWriter, r *http.Request) {
	app.control(w, r, app.Engine.Stop)
}

// SkipItem advances to the next flow item.
func (app *App) SkipItem(w http.ResponseWriter, r *http.Request) {
	app.control(w, r, app.Engine.Skip)
}

// PlayResume releases a manual hold/gate or resumes a paused show.
func (app *App) PlayResume(w http.ResponseWriter, r *http.Request) {
	app.control(w, r, app.Engine.Play)
}

// PauseStream freezes playback mid-item (the stream stays live with silence).
func (app *App) PauseStream(w http.ResponseWriter, r *http.Request) {
	app.control(w, r, app.Engine.Pause)
}

// PrevItem restarts the previous flow item from its beginning.
func (app *App) PrevItem(w http.ResponseWriter, r *http.Request) {
	app.control(w, r, app.Engine.Prev)
}

// RestartItem restarts the current flow item from its beginning.
func (app *App) RestartItem(w http.ResponseWriter, r *http.Request) {
	app.control(w, r, app.Engine.RestartItem)
}

// JumpToItem jumps the live show to the flow item at ?i=.
func (app *App) JumpToItem(w http.ResponseWriter, r *http.Request) {
	i, _ := strconv.Atoi(r.URL.Query().Get("i"))
	app.control(w, r, func() { app.Engine.JumpTo(i) })
}

// TogglePauseAfter arms/disarms the one-shot pause-after-current-item flag.
func (app *App) TogglePauseAfter(w http.ResponseWriter, r *http.Request) {
	app.control(w, r, app.Engine.TogglePauseAfter)
}

// StreamSetAutoNext toggles the auto-next flag of one flow item from the
// stream rundown (?id= item, ?i= snapshot index, ?on=1|0, ?pl= playlist for
// the stopped-state re-render). Persists to the DB and, when a show is live,
// also updates the engine's snapshot so the running flow honors it.
func (app *App) StreamSetAutoNext(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	q := r.URL.Query()
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	i, _ := strconv.Atoi(q.Get("i"))
	pl, _ := strconv.ParseInt(q.Get("pl"), 10, 64)
	on := q.Get("on") == "1"

	if err := app.Flow.SetAutoNext(id, on); err != nil {
		sse.ConsoleError(err)
		return
	}
	app.Engine.SetItemAutoNext(i, on)
	app.patchRundown(sse, pl)
}

// StreamRundown re-renders the stopped-state rundown for the playlist picked
// in the start dropdown (?id=), resetting the start-at selection.
func (app *App) StreamRundown(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	// Before NewSSE — the session cookie must go out with the response headers.
	app.rememberShow(w, r, id)
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"startAt": 0})
	app.patchRundown(sse, id)
}

// rundownKey captures the status fields the rundown rendering depends on.
// The engine broadcasts every 5ms tick; the rundown is only re-patched when
// this key changes so the full list isn't re-sent hundreds of times a second.
type rundownKey struct {
	running, paused, holding, finished, armed bool

	idx         int
	queued      int
	playedCount int
	playlistID  int64
}

func statusRundownKey(s playout.Status) rundownKey {
	return rundownKey{
		running:     s.Running,
		paused:      s.Paused,
		holding:     s.Holding,
		finished:    s.Finished,
		armed:       s.PauseAfterArmed,
		idx:         s.ItemIndex,
		queued:      s.QueuedIndex,
		playedCount: len(s.Played),
		playlistID:  s.PlaylistID,
	}
}

// StreamStatus is a long-lived SSE connection that pushes status updates to the
// operator console as the show progresses.
func (app *App) StreamStatus(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	ch, unsub := app.Engine.Subscribe()
	defer unsub()

	// Initial paint. The rundown uses the remembered show so this patch doesn't
	// clobber the page's server-rendered selection with the engine's last show
	// (while live, patchRundown uses the engine snapshot and ignores the id).
	app.patchStatus(sse)
	last := statusRundownKey(app.Engine.Status())
	app.patchRundown(sse, app.lastShowID(r))

	for {
		select {
		case <-r.Context().Done():
			return
		case s, ok := <-ch:
			if !ok {
				return
			}
			app.patchStatus(sse)
			if key := statusRundownKey(s); key != last {
				last = key
				app.patchRundown(sse, 0)
			}
		}
	}
}

// patchStatus renders the status panel from the engine snapshot and patches it.
func (app *App) patchStatus(sse *datastar.ServerSentEventGenerator) {
	// Running comes from the snapshot (not the engine's flag) so the final
	// stopped-state broadcast renders consistently.
	status := app.Engine.Status()
	out, err := app.Tmpl.Render("stream-status", map[string]any{
		"Status":  status,
		"Running": status.Running,
	})
	if err != nil {
		sse.ConsoleError(err)
		return
	}
	sse.PatchElements(out)
	// Keep the page's `running` signal in sync so the Start/Stop toggle and the
	// locked show selector in the "Start a show" card react live over SSE.
	// showElapsed* drive the rundown footer's live clock + overall progress bar:
	// the rundown list itself is only re-patched on state changes, so the ticking
	// value has to arrive as a signal on every broadcast rather than in the HTML.
	sse.MarshalAndPatchSignals(map[string]any{
		"running":        status.Running,
		"showElapsedSec": status.ElapsedSec,
		"showElapsedFmt": fmtDuration(status.ElapsedSec),
	})
}

// rundownData builds the template data for the stream-rundown partial. While
// live it uses the engine's item snapshot; while stopped it loads the items of
// explicitID (or the most recent show's playlist when explicitID is 0).
func (app *App) rundownData(explicitID int64) map[string]any {
	s := app.Engine.Status()
	if s.Running {
		items, plID := app.Engine.Show()
		return map[string]any{
			"Running":    true,
			"Items":      items,
			"ElapsedSec": s.ElapsedSec,
			"ItemIndex":  s.ItemIndex,
			// The "up next" marker follows the operator-queued item, or defaults
			// to the line right after the current one.
			"NextIndex":       s.NextIndex(),
			"Played":          s.Played,
			"Paused":          s.Paused,
			"Holding":         s.Holding,
			"Finished":        s.Finished,
			"PauseAfterArmed": s.PauseAfterArmed,
			"PlaylistID":      plID,
		}
	}
	id := explicitID
	if id == 0 {
		_, id = app.Engine.Show()
	}
	var items []services.FlowItem
	if id > 0 {
		items, _ = app.Flow.GetItems(id)
	}
	return map[string]any{
		"Running":    false,
		"Items":      items,
		"PlaylistID": id,
	}
}

// patchRundown renders the stream rundown and patches it over SSE.
func (app *App) patchRundown(sse *datastar.ServerSentEventGenerator, explicitID int64) {
	out, err := app.Tmpl.Render("stream-rundown", app.rundownData(explicitID))
	if err != nil {
		sse.ConsoleError(err)
		return
	}
	sse.PatchElements(out)
}
