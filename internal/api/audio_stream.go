package api

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/snarg/tr-engine/internal/audio"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // CORS handled by middleware
}

// AudioStreamHandler serves live audio over WebSocket.
type AudioStreamHandler struct {
	streamer   AudioStreamer
	maxClients int
	clients    atomic.Int32
	log        zerolog.Logger
}

// NewAudioStreamHandler creates a new handler for live audio WebSocket connections.
func NewAudioStreamHandler(streamer AudioStreamer, maxClients int) *AudioStreamHandler {
	return &AudioStreamHandler{
		streamer:   streamer,
		maxClients: maxClients,
		log:        log.With().Str("component", "audio_stream").Logger(),
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
func (h *AudioStreamHandler) HandleStream(w http.ResponseWriter, r *http.Request) {
	if h.streamer == nil || !h.streamer.AudioStreamEnabled() {
		WriteError(w, http.StatusNotFound, "live audio streaming is not enabled")
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

	// Subscribe with empty filter (receives nothing until client sends subscribe)
	frameCh, cancel := h.streamer.SubscribeAudio(audio.AudioFilter{TGIDs: []int{-1}})
	defer cancel()

	// Control channel for messages from the reader goroutine
	controlCh := make(chan subscribeMsg, 4)
	doneCh := make(chan struct{})

	// Reader goroutine: reads JSON control messages from client
	go func() {
		defer close(doneCh)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
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

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	var seq uint16
	// Pre-allocate frame buffer: 14-byte header + max audio data
	frameBuf := make([]byte, 14+8192)

	h.log.Info().Str("remote", r.RemoteAddr).Msg("audio stream client connected")

	for {
		select {
		case <-doneCh:
			h.log.Info().Str("remote", r.RemoteAddr).Msg("audio stream client disconnected")
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

			// Binary frame format (14-byte header + audio data):
			// system_id    (uint16 BE) bytes 0-1
			// tgid         (uint32 BE) bytes 2-5
			// timestamp    (uint32 BE) bytes 6-9  (ms since connection start)
			// seq          (uint16 BE) bytes 10-11
			// sample_rate  (uint16 BE) bytes 12-13 (Hz, e.g. 8000)
			// audio data              bytes 14+

			tsMs := uint32(time.Since(connStart).Milliseconds())
			seq++

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
			status := h.streamer.AudioStreamStatus()
			activeStreams := 0
			if status != nil {
				activeStreams = status.ActiveEncoders
			}
			msg, _ := json.Marshal(map[string]any{
				"type":           "keepalive",
				"active_streams": activeStreams,
			})
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}
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
