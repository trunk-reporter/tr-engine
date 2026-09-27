package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Ticket parameters (§3.5).
const (
	TicketPrefix          = "trt_"
	MaxTicketLength       = 2048 // bytes, whole encoded ticket
	TicketMinTTL          = time.Minute
	TicketMaxTTL          = time.Hour
	TicketDefaultTTL      = 10 * time.Minute
	TicketClockSkew       = time.Minute // tolerated beyond TicketMaxTTL at verification
	MinTicketSecretLength = 32          // bytes; the stored secret is 32 random bytes
)

// ErrInvalidTicket is wrapped by every VerifyTicket error; all of them map to
// 401 invalid_ticket. ErrTicketExpired (which also matches ErrInvalidTicket)
// distinguishes the ordinary end of a ticket's life.
var (
	ErrInvalidTicket = errors.New("invalid ticket")
	ErrTicketExpired = fmt.Errorf("%w: expired", ErrInvalidTicket)
)

// TicketPayload is what a ticket carries. The minting key's own restriction is
// not part of it: verification applies the key's current restriction.
type TicketPayload struct {
	KeyID     int          // "k": the minting key
	ExpiresAt time.Time    // "e": unix seconds; sub-second precision is dropped
	Narrowing *Restriction // "n": the requested narrowing; nil = none
}

// ticketWire is the compact JSON form. Empty narrowing arrays are omitted; an
// allow-nothing narrowing encodes as "n":{}, which is not the same as no "n".
type ticketWire struct {
	K int            `json:"k"`
	E int64          `json:"e"`
	N *narrowingWire `json:"n,omitempty"`
}

type narrowingWire struct {
	AllowAll          bool  `json:"allow_all,omitempty"`
	Systems           []int `json:"systems,omitempty"`
	Talkgroups        []TG  `json:"talkgroups,omitempty"`
	ExcludeTalkgroups []TG  `json:"exclude_talkgroups,omitempty"`
}

// ClampTicketTTL clamps a requested ticket lifetime to TicketMinTTL..TicketMaxTTL.
func ClampTicketTTL(d time.Duration) time.Duration {
	return min(max(d, TicketMinTTL), TicketMaxTTL)
}

// SignTicket encodes and signs p as "trt_<payload>.<mac>": the payload is
// unpadded base64url of compact JSON {"k","e","n"}, and the MAC is unpadded
// base64url of HMAC-SHA256(secret, ASCII of the payload segment).
//
// The narrowing is validated as a KindTicket restriction and normalized
// before encoding. A narrowing with too many entries, or one that makes the
// ticket longer than MaxTicketLength, returns an error wrapping
// ErrTicketTooLarge. SignTicket does not limit the lifetime; the caller
// computes ExpiresAt with ClampTicketTTL, and VerifyTicket rejects anything
// beyond TicketMaxTTL + TicketClockSkew.
func SignTicket(secret []byte, p TicketPayload) (string, error) {
	if len(secret) < MinTicketSecretLength {
		return "", fmt.Errorf("ticket secret must be at least %d bytes", MinTicketSecretLength)
	}
	if !validID(p.KeyID) {
		return "", fmt.Errorf("ticket key id %d is not a positive 32-bit integer", p.KeyID)
	}
	if p.ExpiresAt.IsZero() || p.ExpiresAt.Unix() <= 0 {
		return "", errors.New("ticket expiry is not set")
	}
	w := ticketWire{K: p.KeyID, E: p.ExpiresAt.Unix()}
	if p.Narrowing != nil {
		if err := p.Narrowing.Validate(KindTicket); err != nil {
			return "", err
		}
		n := p.Narrowing.Normalize()
		w.N = &narrowingWire{
			AllowAll:          n.AllowAll,
			Systems:           n.Systems,
			Talkgroups:        n.Talkgroups,
			ExcludeTalkgroups: n.ExcludeTalkgroups,
		}
	}
	js, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("encode ticket: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(js)
	token := TicketPrefix + payload + "." + base64.RawURLEncoding.EncodeToString(ticketMAC(secret, payload))
	if len(token) > MaxTicketLength {
		return "", fmt.Errorf("%w (%d bytes encoded, at most %d)", ErrTicketTooLarge, len(token), MaxTicketLength)
	}
	return token, nil
}

// VerifyTicket checks token against secret at time now and returns its
// payload. In order: the length cap and the "trt_" prefix; strict base64url
// for both segments (no padding, no line breaks, no non-canonical trailing
// bits); the MAC, compared in constant time before the JSON is decoded; strict
// JSON decoding; ExpiresAt after now and no later than now + TicketMaxTTL +
// TicketClockSkew; and a valid KindTicket narrowing.
//
// Every error wraps ErrInvalidTicket. The caller still has to resolve the key
// (it must exist, be active and have listen) and reject narrowings that
// reference a merged-away system (see Restriction.ReferencedSystems).
func VerifyTicket(secret []byte, token string, now time.Time) (TicketPayload, error) {
	invalid := func(why string) (TicketPayload, error) {
		return TicketPayload{}, fmt.Errorf("%w: %s", ErrInvalidTicket, why)
	}
	if len(secret) < MinTicketSecretLength {
		return invalid("ticket secret is not configured")
	}
	if len(token) > MaxTicketLength {
		return invalid("too long")
	}
	rest, ok := strings.CutPrefix(token, TicketPrefix)
	if !ok {
		return invalid("malformed")
	}
	payloadSeg, macSeg, ok := strings.Cut(rest, ".")
	if !ok {
		return invalid("malformed")
	}
	payload, okPayload := decodeSegment(payloadSeg)
	mac, okMAC := decodeSegment(macSeg)
	if !okPayload || !okMAC {
		return invalid("malformed")
	}
	if !hmac.Equal(mac, ticketMAC(secret, payloadSeg)) {
		return invalid("bad signature")
	}

	var w ticketWire
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return invalid("malformed payload")
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("malformed payload")
	}
	if !validID(w.K) || w.E <= 0 {
		return invalid("malformed payload")
	}

	exp := time.Unix(w.E, 0).UTC()
	if !exp.After(now) {
		return TicketPayload{}, ErrTicketExpired
	}
	if exp.After(now.Add(TicketMaxTTL + TicketClockSkew)) {
		return invalid("expiry too far in the future")
	}

	p := TicketPayload{KeyID: w.K, ExpiresAt: exp}
	if w.N != nil {
		n := &Restriction{
			AllowAll:          w.N.AllowAll,
			Systems:           w.N.Systems,
			Talkgroups:        w.N.Talkgroups,
			ExcludeTalkgroups: w.N.ExcludeTalkgroups,
		}
		if err := n.Validate(KindTicket); err != nil {
			return invalid("bad narrowing")
		}
		p.Narrowing = n.Normalize()
	}
	return p, nil
}

func ticketMAC(secret []byte, payloadSeg string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payloadSeg))
	return m.Sum(nil)
}

// decodeSegment decodes unpadded base64url and accepts only the canonical
// encoding: re-encoding must reproduce the input exactly. That rejects
// padding, embedded line breaks (which the decoder skips) and non-zero
// trailing bits.
func decodeSegment(s string) ([]byte, bool) {
	if s == "" {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}
