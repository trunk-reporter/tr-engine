package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/snarg/tr-engine/internal/audio"
	"github.com/snarg/tr-engine/internal/auth"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // CORS handled by middleware
}

const (
	// wsMaxMessageBytes caps one client control message. Subscribe messages
	// are small (a few KB even with hundreds of talkgroups); a larger one
	// closes the connection (1009) instead of being buffered, since the
	// hijacked connection is no longer covered by the request body limit.
	wsMaxMessageBytes = 64 << 10
	// wsReadTimeout is how long the connection may go without any frame from
	// the client (a message, or the pong to the ping sent with every
	// keepalive) before it is treated as gone.
	wsReadTimeout = 60 * time.Second
	// defaultKeepaliveInterval is how often the server sends a ping and a
	// keepalive message.
	defaultKeepaliveInterval = 15 * time.Second
)

// AudioStreamHandler serves live audio over WebSocket.
type AudioStreamHandler struct {
	streamer   AudioStreamer
	maxClients int
	clients    atomic.Int32
	log        zerolog.Logger
	// keepaliveInterval is defaultKeepaliveInterval; tests shorten it.
	keepaliveInterval time.Duration
}

// NewAudioStreamHandler creates a new handler for live audio WebSocket connections.
func NewAudioStreamHandler(streamer AudioStreamer, maxClients int) *AudioStreamHandler {
	return &AudioStreamHandler{
		streamer:          streamer,
		maxClients:        maxClients,
		log:               log.With().Str("component", "audio_stream").Logger(),
		keepaliveInterval: defaultKeepaliveInterval,
	}
}

// Routes registers audio stream routes on the given router.
func (h *AudioStreamHandler) Routes(r chi.Router) {
	r.Get("/audio/live", h.HandleStream)
	r.Get("/audio/jitter", h.GetJitterStats)
}

// subscribeMsg is a client control message sent over the WebSocket.
type subscribeMsg struct {
	Type    string `json:"type"`    // "subscribe" or "unsubscribe"
	TGIDs   []int  `json:"tgids"`
	Systems []int  `json:"systems"`
}

// HandleStream upgrades to WebSocket and streams live audio frames to the client.
//
// Frames reach the client only if the connection's principal may hear them
// (audio.PrincipalAllows, §7.4), whatever its subscribe messages ask for. The
// principal is re-checked every 60 s and when the auth generation moves
// (§7.5): a change that keeps listen is applied in place; otherwise the
// socket is closed with 4401 or 4403 and the signal as the reason. A ticket
// connection is closed when its ticket expires.
func (h *AudioStreamHandler) HandleStream(w http.ResponseWriter, r *http.Request) {
	if h.streamer == nil || !h.streamer.AudioStreamEnabled() {
		WriteError(w, http.StatusNotFound, "live audio streaming is not enabled")
		return
	}

	guard := newStreamGuard(r, h.log)
	if guard == nil {
		// Only reachable without the auth pipeline: fail closed.
		WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "no credential was resolved for this stream")
		return
	}

	if int(h.clients.Load()) >= h.maxClients {
		WriteError(w, http.StatusServiceUnavailable, "maximum audio stream clients reached")
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Error().Err(err).Msg("websocket upgrade failed")
		return
	}
	defer conn.Close()

	h.clients.Add(1)
	defer h.clients.Add(-1)

	connStart := time.Now()

	// Subscribe with empty filter (receives nothing until client sends
	// subscribe). The principal is fixed here, apart from the filter the
	// client controls.
	frameCh, cancel := h.streamer.SubscribeAudio(audio.AudioFilter{TGIDs: []int{-1}}, &guard.principal)
	defer cancel()

	ctx, stop := context.WithCancel(r.Context())
	defer stop()
	authLost := guard.watch(ctx)

	// Control channel for messages from the reader goroutine
	controlCh := make(chan subscribeMsg, 4)
	doneCh := make(chan struct{})

	// Bound what the client can make the server buffer, and drop a peer
	// that stops answering pings.
	conn.SetReadLimit(wsMaxMessageBytes)
	conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	})

	// Reader goroutine: reads JSON control messages from client
	go func() {
		defer close(doneCh)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				if errors.Is(err, websocket.ErrReadLimit) {
					h.log.Info().Str("remote", r.RemoteAddr).Int("limit", wsMaxMessageBytes).
						Msg("audio stream closed: client message too large")
				}
				return
			}
			conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
			var ctrl subscribeMsg
			if err := json.Unmarshal(msg, &ctrl); err != nil {
				continue
			}
			select {
			case controlCh <- ctrl:
			default:
			}
		}
	}()

	keepalive := time.NewTicker(h.keepaliveInterval)
	defer keepalive.Stop()

	tgSeq := make(map[uint64]uint16) // per-system:TG sequence counter
	// Pre-allocate frame buffer: 14-byte header + max audio data
	frameBuf := make([]byte, 14+8192)

	h.log.Info().Str("remote", r.RemoteAddr).Msg("audio stream client connected")

	for {
		select {
		case <-doneCh:
			h.log.Info().Str("remote", r.RemoteAddr).Msg("audio stream client disconnected")
			return

		case signal := <-authLost:
			msg := websocket.FormatCloseMessage(wsCloseCode(signal), signal)
			if err := conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second)); err != nil {
				h.log.Debug().Err(err).Msg("write close frame failed")
			}
			h.log.Info().Str("remote", r.RemoteAddr).Str("code", signal).
				Msg("audio stream closed: its credential no longer allows it")
			return

		case ctrl := <-controlCh:
			switch ctrl.Type {
			case "subscribe":
				filter := audio.AudioFilter{
					SystemIDs: ctrl.Systems,
					TGIDs:     ctrl.TGIDs,
				}
				h.streamer.UpdateAudioFilter(frameCh, filter)
				h.log.Debug().
					Ints("tgids", ctrl.TGIDs).
					Ints("systems", ctrl.Systems).
					Msg("client subscribed")
			case "unsubscribe":
				// Set filter to match nothing
				h.streamer.UpdateAudioFilter(frameCh, audio.AudioFilter{TGIDs: []int{-1}})
				h.log.Debug().Msg("client unsubscribed")
			}

		case frame, ok := <-frameCh:
			if !ok {
				return
			}
			// The principal may have been narrowed since the frame was
			// queued.
			if !audio.PrincipalAllows(guard.principal.Load(), frame) {
				continue
			}

			// Binary frame format (14-byte header + audio data):
			// system_id    (uint16 BE) bytes 0-1
			// tgid         (uint32 BE) bytes 2-5
			// timestamp    (uint32 BE) bytes 6-9  (ms since connection start)
			// seq          (uint16 BE) bytes 10-11
			// sample_rate  (uint16 BE) bytes 12-13 (Hz, e.g. 8000)
			// audio data              bytes 14+

			tsMs := uint32(time.Since(connStart).Milliseconds())
			tgKey := uint64(frame.SystemID)<<32 | uint64(uint32(frame.TGID))
			tgSeq[tgKey]++
			seq := tgSeq[tgKey]

			sampleRate := frame.SampleRate
			if sampleRate == 0 {
				sampleRate = 8000
			}

			dataLen := len(frame.Data)
			totalLen := 14 + dataLen
			var buf []byte
			if totalLen <= len(frameBuf) {
				buf = frameBuf[:totalLen]
			} else {
				buf = make([]byte, totalLen)
			}

			binary.BigEndian.PutUint16(buf[0:2], uint16(frame.SystemID))
			binary.BigEndian.PutUint32(buf[2:6], uint32(frame.TGID))
			binary.BigEndian.PutUint32(buf[6:10], tsMs)
			binary.BigEndian.PutUint16(buf[10:12], seq)
			binary.BigEndian.PutUint16(buf[12:14], uint16(sampleRate))
			copy(buf[14:], frame.Data)

			if err := conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
				h.log.Debug().Err(err).Msg("write frame failed")
				return
			}

		case <-keepalive.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
			msg, _ := json.Marshal(map[string]any{
				"type":           "keepalive",
				"active_streams": h.activeStreamsFor(guard.principal.Load()),
			})
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}
}

// activeStreamsFor returns the keepalive's active_streams for p: every live
// talkgroup stream for an unrestricted principal, and only the streams on
// talkgroups its restrictions allow for a restricted one, which never learns
// how busy the rest of the engine is (§14.5).
func (h *AudioStreamHandler) activeStreamsFor(p *auth.Principal) int {
	if p.Restricted() {
		n := 0
		for _, s := range h.streamer.AudioJitterStats() {
			if audio.PrincipalAllows(p, audio.AudioFrame{SystemID: s.SystemID, TGID: s.TGID}) {
				n++
			}
		}
		return n
	}
	if status := h.streamer.AudioStreamStatus(); status != nil {
		return status.ActiveEncoders
	}
	return 0
}

// GetJitterStats returns per-stream audio jitter statistics.
func (h *AudioStreamHandler) GetJitterStats(w http.ResponseWriter, r *http.Request) {
	if h.streamer == nil || !h.streamer.AudioStreamEnabled() {
		WriteError(w, http.StatusNotFound, "live audio streaming is not enabled")
		return
	}
	stats := h.streamer.AudioJitterStats()
	if stats == nil {
		stats = make(map[string]audio.StreamJitterSnapshot)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"streams": stats})
}
