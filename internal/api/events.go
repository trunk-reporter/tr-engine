package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
)

type EventsHandler struct {
	live LiveDataSource
}

func NewEventsHandler(live LiveDataSource) *EventsHandler {
	return &EventsHandler{live: live}
}

// StreamEvents opens an SSE connection and pushes filtered events.
//
// Events reach the client only if the stream's principal may see them
// (SSEEventAllowed, §7.3) and they match the client's filters. The
// principal is re-checked every 60 s and when the auth generation moves
// (§7.5): a change that keeps listen is applied in place; otherwise the
// stream ends with an `event: auth` signal. A ticket stream ends when its
// ticket expires.
func (h *EventsHandler) StreamEvents(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "event streaming not available")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	log := hlog.FromRequest(r)
	guard := newStreamGuard(r, *log)
	if guard == nil {
		// Only reachable without the auth pipeline: fail closed.
		WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "no credential was resolved for this stream")
		return
	}

	// Parse filter parameters
	filter := EventFilter{
		Systems: QueryIntList(r, "systems"),
		Sites:   QueryIntList(r, "sites"),
		Tgids:   QueryIntList(r, "tgids"),
		Units:   QueryIntList(r, "units"),
	}
	if v, ok := QueryString(r, "types"); ok {
		filter.Types = strings.Split(v, ",")
	}
	if v, ok := QueryBool(r, "emergency_only"); ok {
		filter.EmergencyOnly = v
	}

	// Resume after Last-Event-ID, or after ?last_event_id= for clients that
	// re-create an EventSource with a fresh ticket and so can't set the
	// header (§5). The header wins.
	lastEventID := r.Header.Get("Last-Event-ID")
	if lastEventID == "" {
		lastEventID = r.URL.Query().Get("last_event_id")
	}

	// Subscribe and take the replay in one step, so no event published in
	// between is lost or sent twice.
	replay, ch, cancel := h.live.SubscribeSince(lastEventID, filter, &guard.principal)
	defer cancel()

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	for _, e := range replay {
		writeSSEEvent(w, e)
	}
	// Flush now even without a replay, so the client sees the stream open
	// before the first event or keepalive.
	flusher.Flush()

	log.Info().Str("credential", string(PrincipalFrom(r).Kind)).Msg("SSE client connected")

	ctx, stop := context.WithCancel(r.Context())
	defer stop()
	authLost := guard.watch(ctx)

	// Keepalive ticker
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			log.Info().Msg("SSE client disconnected")
			return
		case signal := <-authLost:
			// Sent whatever the client's types filter says; no id, so the
			// client's last event ID stays that of the last real event.
			data, _ := json.Marshal(map[string]string{"code": signal})
			fmt.Fprintf(w, "event: auth\ndata: %s\n\n", data)
			flusher.Flush()
			log.Info().Str("code", signal).Msg("SSE stream closed: its credential no longer allows it")
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			// The principal may have been narrowed since the event was
			// queued.
			if !SSEEventAllowed(guard.principal.Load(), event.Type, event.SystemID, event.Tgid) {
				continue
			}
			writeSSEEvent(w, event)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes one event in text/event-stream format.
func writeSSEEvent(w http.ResponseWriter, e SSEEvent) {
	fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.ID, e.Type, e.Data)
}

// Routes registers event routes on the given router.
func (h *EventsHandler) Routes(r chi.Router) {
	r.Get("/events/stream", h.StreamEvents)
}
