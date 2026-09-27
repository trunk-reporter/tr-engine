package audio

import (
	"context"
	"fmt"
	"net"
	"strings"
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
	// InstancesByShortName returns the IDs of every TR instance that has a
	// system with this short name, sorted. Used for auto-learning source IP
	// to instance mappings in multi-instance simplestream setups.
	InstancesByShortName(shortName string) []string
}

// ambiguousWarnInterval is how often the router warns about one sender whose
// short name matches several instances.
const ambiguousWarnInterval = 5 * time.Minute

// maxTrackedSenders caps the per-sender maps the router keeps (learned
// attributions, warning times). Simplestream is unauthenticated UDP, so
// source addresses can be spoofed; past the cap the maps are cleared.
const maxTrackedSenders = 4096

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
	bus         *AudioBus
	identity    IdentityLookup
	instanceID  string // TR instance ID fallback for scoped identity lookups
	idleTimeout time.Duration
	opusBitrate int // 0 = PCM passthrough, >0 = Opus requested (falls back to PCM if unavailable)
	log         zerolog.Logger

	input chan AudioChunk

	mu            sync.RWMutex
	activeStreams map[string]*activeStream // key: "systemID:tgid"
	encoders      map[string]AudioEncoder  // key: "systemID:tgid"

	sourceMu  sync.RWMutex
	sourceMap map[string]string // sender IP → instanceID (STREAM_SOURCE_MAP)

	// watchInstanceID and uploadInstanceID are the instances that file
	// watch / TR_DIR imports and HTTP uploads resolve identities under
	// (SetIngestOnlyInstances). They never send simplestream audio.
	watchInstanceID  string
	uploadInstanceID string

	// Only the Run goroutine uses these. learned is the attribution last
	// logged per "ip|short name" (it is re-derived for every chunk);
	// ambiguousWarned is when the router last warned about a sender and
	// short name it could not attribute.
	learned         map[string]string
	ambiguousWarned map[string]time.Time
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
		bus:             bus,
		identity:        identity,
		instanceID:      instanceID,
		idleTimeout:     idleTimeout,
		opusBitrate:     opusBitrate,
		log:             zerolog.Nop(),
		input:           make(chan AudioChunk, 256),
		activeStreams:   make(map[string]*activeStream),
		encoders:        make(map[string]AudioEncoder),
		sourceMap:       make(map[string]string),
		learned:         make(map[string]string),
		ambiguousWarned: make(map[string]time.Time),
	}
}

// SetIngestOnlyInstances names the instance IDs under which file watch and
// TR_DIR imports (watchID, WATCH_INSTANCE_ID) and HTTP uploads (uploadID,
// UPLOAD_INSTANCE_ID) resolve identities. They create systems keyed by
// (instance, short name) like a trunk-recorder instance does, but never send
// simplestream audio, so a sender is attributed to them only when no real
// instance has its short name, and to the upload instance (whose systems any
// upload key can create) only when nothing else has it. Call before Run.
func (r *AudioRouter) SetIngestOnlyInstances(watchID, uploadID string) {
	r.watchInstanceID = watchID
	r.uploadInstanceID = uploadID
}

// SetSourceMap configures sender IP → TR instance ID mappings
// (STREAM_SOURCE_MAP). They take priority over automatic attribution, and
// the instances they name are not attributed to other senders while any
// other instance has the short name. Needed when instances share a short
// name on different systems: the router can't tell their audio apart.
func (r *AudioRouter) SetSourceMap(m map[string]string) {
	r.sourceMu.Lock()
	defer r.sourceMu.Unlock()
	for ip, inst := range m {
		r.sourceMap[ip] = inst
	}
}

// ParseSourceMap parses STREAM_SOURCE_MAP: comma-separated "ip=instance_id"
// pairs. IPs are normalized to the form the simplestream listener reports.
func ParseSourceMap(s string) (map[string]string, error) {
	out := make(map[string]string)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ipStr, inst, ok := strings.Cut(part, "=")
		ip := net.ParseIP(strings.TrimSpace(ipStr))
		inst = strings.TrimSpace(inst)
		if !ok || ip == nil || inst == "" {
			return nil, fmt.Errorf("invalid entry %q: want ip=instance_id", part)
		}
		out[ip.String()] = inst
	}
	return out, nil
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

// resolveInstanceID returns the instance ID to use for this chunk's identity
// lookup. A sender mapped by STREAM_SOURCE_MAP uses its configured instance.
// Otherwise the attribution is derived again for every chunk, from the
// instances that have the chunk's short name now (attributionCandidates):
// the sender is attributed when they all resolve the short name to one
// system. When they resolve it to different systems the router can't tell
// which one sent it, and guessing would label one system's audio with
// another's ID (which restrictions on live audio rely on), so it returns
// ok = false and the chunk is dropped, with a WARN, until STREAM_SOURCE_MAP
// maps the sender. Nothing learned earlier is kept as final: an instance
// that appears later (a new TR instance, or a watched or uploaded call that
// creates a file-watch or upload identity) is taken into account at once.
// Chunks without a source address or short name, and short names no
// instance has, use the configured STREAM_INSTANCE_ID.
func (r *AudioRouter) resolveInstanceID(chunk AudioChunk) (string, bool) {
	// No source address or no short name — can't attribute, use fallback
	if chunk.SourceAddr == "" || chunk.ShortName == "" {
		return r.instanceID, true
	}

	r.sourceMu.RLock()
	instID, configured := r.sourceMap[chunk.SourceAddr]
	// Instances the source map gives to other senders.
	claimed := make(map[string]bool, len(r.sourceMap))
	for ip, id := range r.sourceMap {
		if ip != chunk.SourceAddr {
			claimed[id] = true
		}
	}
	r.sourceMu.RUnlock()
	if configured {
		return instID, true
	}

	candidates := r.attributionCandidates(r.identity.InstancesByShortName(chunk.ShortName), claimed)
	key := chunk.SourceAddr + "|" + chunk.ShortName
	switch {
	case len(candidates) == 0:
		// No instance has this short name — fall back to configured default
		return r.instanceID, true
	case len(candidates) == 1 || r.sameSystem(candidates, chunk.ShortName):
		// One instance, or several on one system (merged sites): any of
		// them labels the audio correctly.
		instID := candidates[0]
		if prev, ok := r.learned[key]; !ok || prev != instID {
			if len(r.learned) >= maxTrackedSenders {
				clear(r.learned)
			}
			r.learned[key] = instID
			ev := r.log.Info()
			if ok {
				ev = r.log.Warn().Str("previous_instance_id", prev)
			}
			ev.Str("source_ip", chunk.SourceAddr).
				Str("instance_id", instID).
				Str("short_name", chunk.ShortName).
				Msg("auto-learned source IP → instance mapping")
		}
		return instID, true
	default:
		delete(r.learned, key)
		if now := time.Now(); now.Sub(r.ambiguousWarned[key]) >= ambiguousWarnInterval {
			if len(r.ambiguousWarned) >= maxTrackedSenders {
				clear(r.ambiguousWarned)
			}
			r.ambiguousWarned[key] = now
			r.log.Warn().
				Str("source_ip", chunk.SourceAddr).
				Str("short_name", chunk.ShortName).
				Strs("instances", candidates).
				Msg("dropping live audio: several trunk-recorder instances use this short name for different systems, so its sender can't be attributed; map the sender with STREAM_SOURCE_MAP=ip=instance_id or give the systems unique short names")
		}
		return "", false
	}
}

// attributionCandidates narrows the instances that have a chunk's short name
// to the ones that may have sent it: instances the source map gives to other
// senders are left out (unless that leaves none), then real trunk-recorder
// instances win over the file-watch instance, which wins over the upload
// instance (SetIngestOnlyInstances): neither sends simplestream audio, and
// upload identities can be created by any upload key.
func (r *AudioRouter) attributionCandidates(all []string, claimed map[string]bool) []string {
	cands := all
	var unclaimed []string
	for _, id := range all {
		if !claimed[id] {
			unclaimed = append(unclaimed, id)
		}
	}
	if len(unclaimed) > 0 {
		cands = unclaimed
	}
	var real, watch, upload []string
	for _, id := range cands {
		switch {
		case id != "" && id == r.watchInstanceID:
			watch = append(watch, id)
		case id != "" && id == r.uploadInstanceID:
			upload = append(upload, id)
		default:
			real = append(real, id)
		}
	}
	switch {
	case len(real) > 0:
		return real
	case len(watch) > 0:
		return watch
	}
	return upload
}

// sameSystem reports whether shortName resolves to one system on every
// instance in instances.
func (r *AudioRouter) sameSystem(instances []string, shortName string) bool {
	first := 0
	for i, instID := range instances {
		sys, _, ok := r.identity.LookupByShortName(instID, shortName)
		if !ok || (i > 0 && sys != first) {
			return false
		}
		first = sys
	}
	return true
}

// processChunk resolves identity, applies dedup logic, and publishes a frame.
func (r *AudioRouter) processChunk(chunk AudioChunk) {
	systemID := chunk.SystemID
	siteID := chunk.SiteID

	// Resolve identity from short name if system ID is not already set.
	if systemID == 0 && chunk.ShortName != "" {
		instanceID, ok := r.resolveInstanceID(chunk)
		if !ok {
			return
		}
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
