package audio

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// simplestreamMeta represents the JSON metadata in a simplestream sendJSON packet.
type simplestreamMeta struct {
	Src             int     `json:"src"`
	SrcTag          string  `json:"src_tag"`
	Talkgroup       int     `json:"talkgroup"`
	TalkgroupTag    string  `json:"talkgroup_tag"`
	Freq            float64 `json:"freq"`
	ShortName       string  `json:"short_name"`
	AudioSampleRate int     `json:"audio_sample_rate"`
}

// SimplestreamSource receives UDP packets from trunk-recorder's simplestream
// plugin and produces AudioChunks.
type SimplestreamSource struct {
	listenAddr        string
	defaultSampleRate int
	log               zerolog.Logger

	mu   sync.Mutex
	addr string // actual bound address, set after Start binds
}

// NewSimplestreamSource creates a new SimplestreamSource that listens on the
// given address. Use ":0" for a random port (useful in tests).
func NewSimplestreamSource(listenAddr string, defaultSampleRate int) *SimplestreamSource {
	return &SimplestreamSource{
		listenAddr:        listenAddr,
		defaultSampleRate: defaultSampleRate,
		log:               zerolog.Nop(),
	}
}

// SetLogger assigns a real logger (replaces the default no-op logger).
func (s *SimplestreamSource) SetLogger(l zerolog.Logger) {
	s.log = l.With().Str("component", "simplestream").Logger()
}

// Addr returns the actual listen address after binding. Returns empty string
// until Start has bound the socket.
func (s *SimplestreamSource) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Start listens for UDP packets, parses them, and sends AudioChunks to out.
// Blocks until ctx is cancelled. Returns nil on clean shutdown.
func (s *SimplestreamSource) Start(ctx context.Context, out chan<- AudioChunk) error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.listenAddr)
	if err != nil {
		return err
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}

	// Store the actual bound address
	s.mu.Lock()
	s.addr = conn.LocalAddr().String()
	s.mu.Unlock()

	// Increase kernel UDP receive buffer to absorb bursts (1 MB).
	if err := conn.SetReadBuffer(1 << 20); err != nil {
		s.log.Warn().Err(err).Msg("failed to set UDP read buffer size")
	}

	s.log.Info().Str("addr", s.addr).Msg("simplestream listening")

	// Close the connection when ctx is cancelled to unblock ReadFromUDP
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 65536) // max UDP packet size
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Check if we were cancelled
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			s.log.Warn().Err(err).Msg("simplestream read error")
			continue
		}

		chunk, ok := s.parsePacket(buf[:n])
		if ok && remoteAddr != nil {
			chunk.SourceAddr = remoteAddr.IP.String()
		}
		if !ok {
			continue
		}

		// Non-blocking send: drop chunks rather than stalling the UDP read
		// loop, which would cause kernel buffer overflow and packet loss.
		select {
		case out <- chunk:
		default:
			s.log.Warn().Int("tgid", chunk.TGID).Msg("dropping audio chunk: router channel full")
		case <-ctx.Done():
			return nil
		}
	}
}

// parsePacket attempts to parse a simplestream UDP packet.
// Supports three formats:
//  1. sendJSON with valid length prefix: [4-byte JSON length LE][JSON][PCM]
//  2. sendJSON with corrupt length prefix: [4 bytes junk][JSON delimited by {}][PCM]
//     (some TR builds have a scatter-gather bug that corrupts the length prefix)
//  3. sendTGID: [4-byte TGID LE][int16 PCM samples]
//
// Returns false for malformed packets.
func (s *SimplestreamSource) parsePacket(data []byte) (AudioChunk, bool) {
	if len(data) < 4 {
		s.log.Warn().Int("len", len(data)).Msg("simplestream packet too short")
		return AudioChunk{}, false
	}

	// Try sendJSON mode: look for JSON either via length prefix or by scanning for '{'.
	if chunk, ok := s.tryParseJSON(data); ok {
		return chunk, true
	}

	// Fall back to sendTGID mode: [4-byte TGID LE][int16 PCM samples]
	tgid := int(binary.LittleEndian.Uint32(data[0:4]))
	if tgid <= 0 {
		return AudioChunk{}, false
	}

	pcmData := make([]byte, len(data)-4)
	copy(pcmData, data[4:])

	return AudioChunk{
		TGID:       tgid,
		Format:     AudioFormatPCM,
		SampleRate: s.defaultSampleRate,
		Data:       pcmData,
		Timestamp:  time.Now(),
	}, true
}

// tryParseJSON attempts to extract JSON metadata from a sendJSON packet.
// It first tries the documented format (4-byte LE length prefix), then falls
// back to scanning for '{' near the start of the packet.
func (s *SimplestreamSource) tryParseJSON(data []byte) (AudioChunk, bool) {
	var jsonBytes []byte
	var pcmStart int

	// Method 1: trust the 4-byte length prefix
	jsonLen := int(binary.LittleEndian.Uint32(data[0:4]))
	if jsonLen > 0 && jsonLen <= len(data)-4 && data[4] == '{' {
		jsonBytes = data[4 : 4+jsonLen]
		pcmStart = 4 + jsonLen
	} else {
		// Method 2: scan for JSON object starting near byte 4
		// Some TR builds corrupt the length prefix (scatter-gather bug)
		jsonStart := -1
		for i := 0; i < min(8, len(data)); i++ {
			if data[i] == '{' {
				jsonStart = i
				break
			}
		}
		if jsonStart < 0 {
			return AudioChunk{}, false
		}

		// Find the closing '}' — JSON is a single flat object (no nested braces)
		jsonEnd := -1
		for i := jsonStart + 1; i < len(data); i++ {
			if data[i] == '}' {
				jsonEnd = i
				break
			}
		}
		if jsonEnd < 0 {
			return AudioChunk{}, false
		}

		jsonBytes = data[jsonStart : jsonEnd+1]
		pcmStart = jsonEnd + 1
	}

	var meta simplestreamMeta
	if json.Unmarshal(jsonBytes, &meta) != nil || meta.Talkgroup == 0 {
		return AudioChunk{}, false
	}

	pcmData := make([]byte, len(data)-pcmStart)
	copy(pcmData, data[pcmStart:])

	sampleRate := meta.AudioSampleRate
	if sampleRate == 0 {
		sampleRate = s.defaultSampleRate
	}

	return AudioChunk{
		ShortName:  meta.ShortName,
		TGID:       meta.Talkgroup,
		UnitID:     meta.Src,
		Freq:       meta.Freq,
		TGAlphaTag: meta.TalkgroupTag,
		Format:     AudioFormatPCM,
		SampleRate: sampleRate,
		Data:       pcmData,
		Timestamp:  time.Now(),
	}, true
}
