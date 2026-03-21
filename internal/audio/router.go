package audio

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// IdentityLookup resolves a trunk-recorder short name to system and site IDs.
// instanceID scopes the lookup to a specific TR instance (direct cache key lookup);
// when empty, falls back to scanning all entries (non-deterministic for conventional
// systems with duplicate short names across instances).
type IdentityLookup interface {
	LookupByShortName(instanceID, shortName string) (systemID, siteID int, ok bool)
	// LookupByShortNameAny scans all identity cache entries for a matching short name,
	// skipping entries whose instanceID is in the exclude set. Returns the first
	// non-excluded match along with its instanceID. Used for auto-learning source IP
	// to instance mappings in multi-instance simplestream setups.
	LookupByShortNameAny(shortName string, exclude map[string]bool) (systemID, siteID int, instanceID string, ok bool)
}

// activeStream tracks a live audio stream for a specific talkgroup.
type activeStream struct {
	systemID  int
	siteID    int
	shortName string
	lastChunk time.Time
	seq       uint16
	jitter    JitterStats
	startedAt time.Time
}

// StreamJitterSnapshot is a point-in-time snapshot of jitter stats for one stream.
type StreamJitterSnapshot struct {
	SystemID  int       `json:"system_id"`
	TGID      int       `json:"tgid"`
	Count     int       `json:"count"`
	Min       float64   `json:"min_ms"`
	Max       float64   `json:"max_ms"`
	Mean      float64   `json:"mean_ms"`
	Stddev    float64   `json:"stddev_ms"`
	Last      float64   `json:"last_delta_ms"`
	StartedAt time.Time `json:"started_at"`
}

// AudioRouter receives AudioChunks, resolves identity (shortName to system/site),
// deduplicates multi-site streams, encodes audio, and publishes AudioFrames to the AudioBus.
type AudioRouter struct {
	bus          *AudioBus
	identity     IdentityLookup
	instanceID   string        // TR instance ID fallback for scoped identity lookups
	idleTimeout  time.Duration
	opusBitrate  int // 0 = PCM passthrough, >0 = Opus requested (falls back to PCM if unavailable)
	log          zerolog.Logger

	input chan AudioChunk

	mu            sync.RWMutex
	activeStreams map[string]*activeStream // key: "systemID:tgid"
	encoders      map[string]AudioEncoder  // key: "systemID:tgid"

	sourceMu  sync.RWMutex
	sourceMap map[string]string // sender IP → instanceID (auto-learned)
}

// NewAudioRouter creates an AudioRouter that resolves identity, deduplicates
// multi-site streams, encodes audio, and publishes frames to the given AudioBus.
// opusBitrate controls encoding: 0 = PCM passthrough, >0 = Opus encoding (falls
// back to PCM passthrough if Opus is not available in this build).
// SetLogger assigns a real logger (replaces the default no-op logger).
func (r *AudioRouter) SetLogger(l zerolog.Logger) {
	r.log = l.With().Str("component", "audio_router").Logger()
}

func NewAudioRouter(bus *AudioBus, identity IdentityLookup, instanceID string, idleTimeout time.Duration, opusBitrate int) *AudioRouter {
	return &AudioRouter{
		bus:           bus,
		identity:      identity,
		instanceID:    instanceID,
		idleTimeout:   idleTimeout,
		opusBitrate:   opusBitrate,
		log:           zerolog.Nop(),
		input:         make(chan AudioChunk, 256),
		activeStreams: make(map[string]*activeStream),
		encoders:      make(map[string]AudioEncoder),
		sourceMap:     make(map[string]string),
	}
}

// Input returns the channel for sending AudioChunks into the router.
func (r *AudioRouter) Input() chan<- AudioChunk {
	return r.input
}

// ActiveStreamCount returns the number of currently active streams.
func (r *AudioRouter) ActiveStreamCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.activeStreams)
}

// Run starts the router's main loop. It processes incoming chunks, publishes
// frames to the bus, and periodically cleans up idle streams. It blocks until
// ctx is cancelled.
func (r *AudioRouter) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case chunk := <-r.input:
			r.processChunk(chunk)
		case <-ticker.C:
			r.cleanupIdle()
		}
	}
}

// resolveInstanceID returns the instance ID to use for this chunk's identity lookup.
// For chunks with a source address and short name, it checks the auto-learned
// sourceMap first, then tries LookupByShortNameAny to learn new mappings.
// Falls back to the configured STREAM_INSTANCE_ID.
func (r *AudioRouter) resolveInstanceID(chunk AudioChunk) string {
	// No source address or no short name — can't auto-learn, use fallback
	if chunk.SourceAddr == "" || chunk.ShortName == "" {
		return r.instanceID
	}

	// Fast path: check if we've already learned this IP's instance
	r.sourceMu.RLock()
	if instID, ok := r.sourceMap[chunk.SourceAddr]; ok {
		r.sourceMu.RUnlock()
		return instID
	}
	r.sourceMu.RUnlock()

	// Slow path: try to learn by scanning all identity entries.
	// Build the exclude set from already-claimed instanceIDs.
	r.sourceMu.RLock()
	exclude := make(map[string]bool, len(r.sourceMap))
	for _, instID := range r.sourceMap {
		exclude[instID] = true
	}
	r.sourceMu.RUnlock()

	_, _, instID, ok := r.identity.LookupByShortNameAny(chunk.ShortName, exclude)
	if ok {
		r.sourceMu.Lock()
		r.sourceMap[chunk.SourceAddr] = instID
		r.sourceMu.Unlock()
		r.log.Info().
			Str("source_ip", chunk.SourceAddr).
			Str("instance_id", instID).
			Str("short_name", chunk.ShortName).
			Msg("auto-learned source IP → instance mapping")
		return instID
	}

	// Learning failed — fall back to configured default
	return r.instanceID
}

// processChunk resolves identity, applies dedup logic, and publishes a frame.
func (r *AudioRouter) processChunk(chunk AudioChunk) {
	systemID := chunk.SystemID
	siteID := chunk.SiteID

	// Resolve identity from short name if system ID is not already set.
	if systemID == 0 && chunk.ShortName != "" {
		instanceID := r.resolveInstanceID(chunk)
		var ok bool
		systemID, siteID, ok = r.identity.LookupByShortName(instanceID, chunk.ShortName)
		if !ok {
			r.log.Debug().
				Str("short_name", chunk.ShortName).
				Str("instance_id", instanceID).
				Msg("dropping chunk: unknown short name")
			return
		}
	}

	// Drop chunks without a valid system or talkgroup.
	if systemID == 0 || chunk.TGID == 0 {
		r.log.Debug().
			Int("system_id", systemID).
			Int("tgid", chunk.TGID).
			Msg("dropping chunk: missing system or talkgroup")
		return
	}

	key := fmt.Sprintf("%d:%d", systemID, chunk.TGID)
	now := time.Now()

	r.mu.Lock()
	stream, exists := r.activeStreams[key]

	if exists {
		// If the existing stream's site differs, check if it's gone idle.
		if stream.siteID != siteID {
			if now.Sub(stream.lastChunk) > r.idleTimeout {
				// Stream is stale; allow takeover by the new site.
				stream.siteID = siteID
				stream.shortName = chunk.ShortName
				stream.lastChunk = chunk.Timestamp
				stream.seq = 0
				stream.jitter.Reset()
				stream.startedAt = chunk.Timestamp
			} else {
				// Another site owns this stream; drop (dedup).
				r.mu.Unlock()
				r.log.Debug().
					Str("key", key).
					Int("existing_site", stream.siteID).
					Int("new_site", siteID).
					Msg("dropping chunk: dedup multi-site")
				return
			}
		} else {
			// Same site — compute jitter from receive timestamps, then update.
			delta := chunk.Timestamp.Sub(stream.lastChunk)
			stream.lastChunk = chunk.Timestamp
			stream.seq++
			if delta > 0 && delta < 10*time.Second { // sanity bound
				stream.jitter.Add(float64(delta.Microseconds()) / 1000.0)
			}
		}
	} else {
		// New stream.
		stream = &activeStream{
			systemID:  systemID,
			siteID:    siteID,
			shortName: chunk.ShortName,
			lastChunk: chunk.Timestamp,
			seq:       0,
			startedAt: chunk.Timestamp,
		}
		r.activeStreams[key] = stream
	}

	seq := stream.seq

	// Get or create encoder for this stream.
	enc, ok := r.encoders[key]
	if !ok {
		enc = NewEncoder(chunk.SampleRate, r.opusBitrate, r.log)
		r.encoders[key] = enc
	}
	r.mu.Unlock()

	// Encode audio data.
	data, format, err := enc.Encode(chunk.Data)
	if err != nil {
		r.log.Error().Err(err).Str("key", key).Msg("encoding failed, using raw PCM")
		data = chunk.Data
		format = chunk.Format
	}

	// Build and publish frame.
	frame := AudioFrame{
		SystemID:   systemID,
		TGID:       chunk.TGID,
		UnitID:     chunk.UnitID,
		SampleRate: enc.SampleRate(),
		Seq:        seq,
		Format:     format,
		Data:       data,
	}

	r.bus.Publish(frame)
}

// GetJitterStats returns a snapshot of jitter stats for all active streams.
func (r *AudioRouter) GetJitterStats() map[string]StreamJitterSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]StreamJitterSnapshot, len(r.activeStreams))
	for key, stream := range r.activeStreams {
		snap := stream.jitter.Snapshot()
		result[key] = StreamJitterSnapshot{
			SystemID:  stream.systemID,
			TGID:      extractTGID(key),
			Count:     snap.Count,
			Min:       snap.Min,
			Max:       snap.Max,
			Mean:      snap.Mean(),
			Stddev:    snap.Stddev(),
			Last:      snap.Last,
			StartedAt: stream.startedAt,
		}
	}
	return result
}

// extractTGID parses the talkgroup ID from a "systemID:tgid" key.
func extractTGID(key string) int {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == ':' {
			v := 0
			for _, c := range key[i+1:] {
				v = v*10 + int(c-'0')
			}
			return v
		}
	}
	return 0
}

// cleanupIdle removes streams and their encoders that have been idle longer than idleTimeout.
func (r *AudioRouter) cleanupIdle() {
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	for key, stream := range r.activeStreams {
		if now.Sub(stream.lastChunk) > r.idleTimeout {
			if enc, ok := r.encoders[key]; ok {
				enc.Close()
				delete(r.encoders, key)
			}
			delete(r.activeStreams, key)
		}
	}
}
