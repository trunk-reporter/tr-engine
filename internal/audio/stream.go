package audio

import (
	"context"
	"time"
)

// AudioFormat identifies the encoding of audio data in a chunk.
type AudioFormat int

const (
	AudioFormatPCM  AudioFormat = iota // 16-bit signed little-endian PCM
	AudioFormatOpus                    // Opus-encoded packet
)

func (f AudioFormat) String() string {
	switch f {
	case AudioFormatPCM:
		return "pcm"
	case AudioFormatOpus:
		return "opus"
	default:
		return "unknown"
	}
}

// AudioChunk is a single audio frame from any ingest source.
type AudioChunk struct {
	ShortName  string      // TR sys_name (e.g. "butco")
	SourceAddr string      // sender IP address (from UDP source)
	SystemID   int         // resolved system ID (0 if unresolved)
	SiteID     int         // resolved site ID (0 if unresolved)
	TGID       int         // talkgroup ID
	UnitID     int         // source unit ID
	Freq       float64     // frequency in Hz
	TGAlphaTag string      // talkgroup alpha tag (from simplestream JSON)
	Format     AudioFormat // PCM or Opus
	SampleRate int         // samples per second (8000, 16000, etc.)
	Data       []byte      // audio payload
	Timestamp  time.Time   // when the chunk was received
}

// AudioFrame is an encoded audio frame ready for WebSocket delivery.
type AudioFrame struct {
	SystemID   int
	TGID       int
	UnitID     int
	SampleRate int // samples per second (e.g. 8000)
	Seq        uint16 // per-tgid sequence number
	Timestamp  uint32 // ms since bus start
	Format     AudioFormat
	Data       []byte // PCM or Opus payload
}

// AudioChunkSource produces audio chunks from a transport (UDP, MQTT, etc.).
type AudioChunkSource interface {
	Start(ctx context.Context, out chan<- AudioChunk) error
}
