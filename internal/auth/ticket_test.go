package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	testSecret  = bytes.Repeat([]byte{0x5a}, 32)
	otherSecret = bytes.Repeat([]byte{0xa5}, 32)
	testNow     = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
)

// forge builds a ticket with a valid MAC over an arbitrary payload segment,
// to test what verification does after the MAC check passes.
func forge(secret []byte, payloadSeg string) string {
	return TicketPrefix + payloadSeg + "." + base64.RawURLEncoding.EncodeToString(ticketMAC(secret, payloadSeg))
}

func forgeJSON(secret []byte, js string) string {
	return forge(secret, base64.RawURLEncoding.EncodeToString([]byte(js)))
}

func mustSign(t *testing.T, p TicketPayload) string {
	t.Helper()
	tok, err := SignTicket(testSecret, p)
	if err != nil {
		t.Fatalf("SignTicket(%+v): %v", p, err)
	}
	return tok
}

func TestTicketRoundTrip(t *testing.T) {
	exp := testNow.Add(10 * time.Minute)
	tests := []struct {
		name string
		in   TicketPayload
		json string // expected decoded payload segment
		want TicketPayload
	}{
		{"no narrowing",
			TicketPayload{KeyID: 7, ExpiresAt: exp},
			`{"k":7,"e":` + strconv.FormatInt(exp.Unix(), 10) + `}`,
			TicketPayload{KeyID: 7, ExpiresAt: exp}},
		{"narrowing is normalized and compact",
			TicketPayload{KeyID: 7, ExpiresAt: exp, Narrowing: &Restriction{Talkgroups: tgs("2:9179", "2:9178", "2:9178")}},
			`{"k":7,"e":` + strconv.FormatInt(exp.Unix(), 10) + `,"n":{"talkgroups":["2:9178","2:9179"]}}`,
			TicketPayload{KeyID: 7, ExpiresAt: exp, Narrowing: (&Restriction{Talkgroups: tgs("2:9178", "2:9179")}).Normalize()}},
		{"allow_all narrowing with exclusions",
			TicketPayload{KeyID: 1, ExpiresAt: exp, Narrowing: &Restriction{AllowAll: true, ExcludeTalkgroups: tgs("1:5001")}},
			`{"k":1,"e":` + strconv.FormatInt(exp.Unix(), 10) + `,"n":{"allow_all":true,"exclude_talkgroups":["1:5001"]}}`,
			TicketPayload{KeyID: 1, ExpiresAt: exp, Narrowing: (&Restriction{AllowAll: true, ExcludeTalkgroups: tgs("1:5001")}).Normalize()}},
		{"allow-nothing narrowing survives as a restriction, not as none",
			TicketPayload{KeyID: 3, ExpiresAt: exp, Narrowing: &Restriction{}},
			`{"k":3,"e":` + strconv.FormatInt(exp.Unix(), 10) + `,"n":{}}`,
			TicketPayload{KeyID: 3, ExpiresAt: exp, Narrowing: (&Restriction{}).Normalize()}},
		{"sub-second expiry is truncated",
			TicketPayload{KeyID: 7, ExpiresAt: exp.Add(900 * time.Millisecond)},
			`{"k":7,"e":` + strconv.FormatInt(exp.Unix(), 10) + `}`,
			TicketPayload{KeyID: 7, ExpiresAt: exp}},
	}
	for _, tt := range tests {
		tok := mustSign(t, tt.in)
		rest, ok := strings.CutPrefix(tok, "trt_")
		if !ok || strings.Count(rest, ".") != 1 {
			t.Fatalf("%s: malformed ticket %q", tt.name, tok)
		}
		seg, mac, _ := strings.Cut(rest, ".")
		js, err := base64.RawURLEncoding.DecodeString(seg)
		if err != nil || string(js) != tt.json {
			t.Errorf("%s: payload = %s, %v; want %s", tt.name, js, err, tt.json)
		}
		if len(mac) != 43 {
			t.Errorf("%s: MAC segment has %d characters, want 43", tt.name, len(mac))
		}
		got, err := VerifyTicket(testSecret, tok, testNow)
		if err != nil {
			t.Errorf("%s: VerifyTicket: %v", tt.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: VerifyTicket = %+v, want %+v", tt.name, got, tt.want)
		}
		if got.ExpiresAt.Location() != time.UTC {
			t.Errorf("%s: ExpiresAt not in UTC", tt.name)
		}
	}
	if got, _ := VerifyTicket(testSecret, mustSign(t, TicketPayload{KeyID: 3, ExpiresAt: exp, Narrowing: &Restriction{}}), testNow); got.Narrowing == nil || !got.Narrowing.AllowsNothing() {
		t.Error("allow-nothing narrowing did not verify as allow-nothing")
	}
}

// nonCanonical changes the last character of a base64url segment so that it
// still decodes to the same bytes but sets unused trailing bits. It returns
// "" when the segment has no trailing bits.
func nonCanonical(seg string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var unused uint
	switch len(seg) % 4 {
	case 2:
		unused = 4
	case 3:
		unused = 2
	default:
		return ""
	}
	v := strings.IndexByte(alphabet, seg[len(seg)-1])
	if v < 0 || v&(1<<unused-1) != 0 {
		return ""
	}
	return seg[:len(seg)-1] + string(alphabet[v|1])
}

func TestVerifyTicketRejects(t *testing.T) {
	exp := testNow.Add(10 * time.Minute)
	good := mustSign(t, TicketPayload{KeyID: 7, ExpiresAt: exp})
	other := mustSign(t, TicketPayload{KeyID: 8, ExpiresAt: exp})
	seg, mac, _ := strings.Cut(strings.TrimPrefix(good, TicketPrefix), ".")
	otherSeg, _, _ := strings.Cut(strings.TrimPrefix(other, TicketPrefix), ".")

	flip := func(s string, i int) string {
		b := []byte(s)
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}

	// A payload segment with trailing bits, for the non-canonical cases.
	var tbSeg string
	for k := 1; tbSeg == ""; k++ {
		tok := mustSign(t, TicketPayload{KeyID: k, ExpiresAt: exp})
		s, _, _ := strings.Cut(strings.TrimPrefix(tok, TicketPrefix), ".")
		if nc := nonCanonical(s); nc != "" {
			tbSeg = s
		}
	}
	ncPayload := nonCanonical(tbSeg)
	ncMAC := nonCanonical(mac)
	if ncMAC == "" {
		t.Fatal("expected the 43-character MAC segment to have trailing bits")
	}
	// The lenient decoder accepts these, so only the canonical check stops them.
	for _, pair := range [][2]string{{tbSeg, ncPayload}, {mac, ncMAC}} {
		a, _ := base64.RawURLEncoding.DecodeString(pair[0])
		b, err := base64.RawURLEncoding.DecodeString(pair[1])
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("test setup: %q and %q should decode to the same bytes", pair[0], pair[1])
		}
	}

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"prefix only", "trt_"},
		{"no prefix", strings.TrimPrefix(good, TicketPrefix)},
		{"wrong prefix", "tre_" + strings.TrimPrefix(good, TicketPrefix)},
		{"upper-case prefix", "TRT_" + strings.TrimPrefix(good, TicketPrefix)},
		{"an API key", "tre_" + strings.Repeat("ab", 32)},
		{"no dot", TicketPrefix + seg + mac},
		{"empty payload", TicketPrefix + "." + mac},
		{"empty MAC", TicketPrefix + seg + "."},
		{"extra segment", good + ".AAAA"},
		{"tampered payload", TicketPrefix + flip(seg, 3) + "." + mac},
		{"tampered MAC", TicketPrefix + seg + "." + flip(mac, 0)},
		{"swapped payload", TicketPrefix + otherSeg + "." + mac},
		{"truncated by one", good[:len(good)-1]},
		{"truncated MAC", TicketPrefix + seg + "." + mac[:40]},
		{"truncated payload", TicketPrefix + seg[:len(seg)-4] + "." + mac},
		{"padded MAC", good + "="},
		{"padded MAC (==)", good + "=="},
		{"padded payload", TicketPrefix + seg + "=." + mac},
		{"padded payload with valid MAC", forge(testSecret, seg+"==")},
		{"non-canonical MAC trailing bits", TicketPrefix + seg + "." + ncMAC},
		{"non-canonical payload with valid MAC", forge(testSecret, ncPayload)},
		{"line break in payload with valid MAC", forge(testSecret, seg[:4]+"\n"+seg[4:])},
		{"CRLF in MAC", TicketPrefix + seg + "." + mac[:10] + "\r\n" + mac[10:]},
		{"standard base64 alphabet", TicketPrefix + strings.NewReplacer("-", "+", "_", "/").Replace(seg) + "+/." + mac},
		{"space", good + " "},
		{"wrong secret", mustSignWith(t, otherSecret, TicketPayload{KeyID: 7, ExpiresAt: exp})},
		{"too long", good + strings.Repeat("A", MaxTicketLength)},
	}
	for _, tt := range tests {
		got, err := VerifyTicket(testSecret, tt.token, testNow)
		if !errors.Is(err, ErrInvalidTicket) {
			t.Errorf("%s: VerifyTicket(%q) = %+v, %v; want ErrInvalidTicket", tt.name, tt.token, got, err)
		}
		if !reflect.DeepEqual(got, TicketPayload{}) {
			t.Errorf("%s: rejected ticket returned a payload: %+v", tt.name, got)
		}
		if errors.Is(err, ErrTicketExpired) {
			t.Errorf("%s: reported as expired", tt.name)
		}
	}

	// The same token verifies with the right secret: the rejections above are
	// not an artifact of a broken original.
	if _, err := VerifyTicket(testSecret, good, testNow); err != nil {
		t.Fatalf("original ticket does not verify: %v", err)
	}
	for _, s := range [][]byte{nil, {}, testSecret[:31]} {
		if _, err := VerifyTicket(s, good, testNow); !errors.Is(err, ErrInvalidTicket) {
			t.Errorf("VerifyTicket with a %d-byte secret = %v, want ErrInvalidTicket", len(s), err)
		}
	}
}

func mustSignWith(t *testing.T, secret []byte, p TicketPayload) string {
	t.Helper()
	tok, err := SignTicket(secret, p)
	if err != nil {
		t.Fatalf("SignTicket: %v", err)
	}
	return tok
}

func TestVerifyTicketExpiry(t *testing.T) {
	tests := []struct {
		name    string
		exp     time.Time
		expired bool // ErrTicketExpired
		ok      bool
	}{
		{"one second left", testNow.Add(time.Second), false, true},
		{"default TTL", testNow.Add(TicketDefaultTTL), false, true},
		{"max TTL", testNow.Add(TicketMaxTTL), false, true},
		{"max TTL + clock skew", testNow.Add(TicketMaxTTL + TicketClockSkew), false, true},
		{"one second beyond skew", testNow.Add(TicketMaxTTL + TicketClockSkew + time.Second), false, false},
		{"far future", testNow.Add(365 * 24 * time.Hour), false, false},
		{"expires now", testNow, true, false},
		{"expired", testNow.Add(-time.Second), true, false},
		{"long expired", testNow.Add(-48 * time.Hour), true, false},
	}
	for _, tt := range tests {
		tok := mustSign(t, TicketPayload{KeyID: 7, ExpiresAt: tt.exp})
		_, err := VerifyTicket(testSecret, tok, testNow)
		switch {
		case tt.ok && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case !tt.ok && !errors.Is(err, ErrInvalidTicket):
			t.Errorf("%s: err = %v, want ErrInvalidTicket", tt.name, err)
		case errors.Is(err, ErrTicketExpired) != tt.expired:
			t.Errorf("%s: err = %v, expired = %v", tt.name, err, tt.expired)
		}
	}
	// A sub-second "now" does not stretch the bounds.
	tok := mustSign(t, TicketPayload{KeyID: 7, ExpiresAt: testNow.Add(time.Second)})
	if _, err := VerifyTicket(testSecret, tok, testNow.Add(999*time.Millisecond)); err != nil {
		t.Errorf("1ms before expiry: %v", err)
	}
	if _, err := VerifyTicket(testSecret, tok, testNow.Add(time.Second)); !errors.Is(err, ErrTicketExpired) {
		t.Errorf("at expiry: %v, want ErrTicketExpired", err)
	}
}

func TestVerifyTicketForgedPayloads(t *testing.T) {
	e := strconv.FormatInt(testNow.Add(5*time.Minute).Unix(), 10)
	rejected := []string{
		`not json`,
		``,
		`[]`,
		`null`,
		`{}`,
		`{"k":7}`,
		`{"e":` + e + `}`,
		`{"k":0,"e":` + e + `}`,
		`{"k":-7,"e":` + e + `}`,
		`{"k":2147483648,"e":` + e + `}`,
		`{"k":7.5,"e":` + e + `}`,
		`{"k":"7","e":` + e + `}`,
		`{"k":7,"e":"` + e + `"}`,
		`{"k":7,"e":0}`,
		`{"k":7,"e":-1}`,
		`{"k":7,"e":` + e + `,"x":1}`,
		`{"k":7,"e":` + e + `} {}`,
		`{"k":7,"e":` + e + `}x`,
		`{"k":7,"e":` + e + `,"n":{"allow_all":true,"systems":[1]}}`,
		`{"k":7,"e":` + e + `,"n":{"talkgroups":["1:0"]}}`,
		`{"k":7,"e":` + e + `,"n":{"talkgroups":[9178]}}`,
		`{"k":7,"e":` + e + `,"n":{"systems":[0]}}`,
		`{"k":7,"e":` + e + `,"n":{"exclude_talkgroup":["1:1"]}}`,
		`{"k":7,"e":` + e + `,"n":{"systems":[` + strings.Trim(strings.Repeat("1,", 101), ",") + `]}}`,
		`{"k":7,"e":` + e + `,"n":[]}`,
	}
	for _, js := range rejected {
		if got, err := VerifyTicket(testSecret, forgeJSON(testSecret, js), testNow); !errors.Is(err, ErrInvalidTicket) {
			t.Errorf("payload %s: VerifyTicket = %+v, %v; want ErrInvalidTicket", js, got, err)
		}
	}
	// Explicit null means no narrowing; duplicates in a narrowing are normalized.
	got, err := VerifyTicket(testSecret, forgeJSON(testSecret, `{"k":7,"e":`+e+`,"n":null}`), testNow)
	if err != nil || got.Narrowing != nil || got.KeyID != 7 {
		t.Errorf(`"n":null = %+v, %v`, got, err)
	}
	got, err = VerifyTicket(testSecret, forgeJSON(testSecret, `{"k":7,"e":`+e+`,"n":{"systems":[3,1,3]}}`), testNow)
	if err != nil || got.Narrowing == nil || !reflect.DeepEqual(got.Narrowing.Systems, []int{1, 3}) {
		t.Errorf("duplicate systems = %+v, %v", got, err)
	}
}

func TestSignTicketErrors(t *testing.T) {
	exp := testNow.Add(time.Minute)
	bigTGs := make([]TG, 100)
	for i := range bigTGs {
		bigTGs[i] = TG{maxID - i, maxID - i}
	}
	tests := []struct {
		name   string
		secret []byte
		p      TicketPayload
		is     error
		msg    string
	}{
		{"nil secret", nil, TicketPayload{KeyID: 1, ExpiresAt: exp}, nil, "secret"},
		{"short secret", testSecret[:16], TicketPayload{KeyID: 1, ExpiresAt: exp}, nil, "secret"},
		{"no key", testSecret, TicketPayload{ExpiresAt: exp}, nil, "key id"},
		{"negative key", testSecret, TicketPayload{KeyID: -1, ExpiresAt: exp}, nil, "key id"},
		{"key beyond int4", testSecret, TicketPayload{KeyID: maxID + 1, ExpiresAt: exp}, nil, "key id"},
		{"no expiry", testSecret, TicketPayload{KeyID: 1}, nil, "expiry"},
		{"invalid narrowing", testSecret, TicketPayload{KeyID: 1, ExpiresAt: exp,
			Narrowing: &Restriction{AllowAll: true, Systems: []int{1}}}, nil, "allow_all"},
		{"101 entries", testSecret, TicketPayload{KeyID: 1, ExpiresAt: exp,
			Narrowing: &Restriction{Systems: manyInts(50), Talkgroups: manyTGs(51)}}, ErrTicketTooLarge, ""},
		{"100 entries over 2048 bytes", testSecret, TicketPayload{KeyID: 1, ExpiresAt: exp,
			Narrowing: &Restriction{Talkgroups: bigTGs}}, ErrTicketTooLarge, "bytes encoded"},
	}
	for _, tt := range tests {
		tok, err := SignTicket(tt.secret, tt.p)
		if err == nil {
			t.Errorf("%s: SignTicket = %q, want error", tt.name, tok)
			continue
		}
		if tt.is != nil && !errors.Is(err, tt.is) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.is)
		}
		if tt.msg != "" && !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: err = %v, want containing %q", tt.name, err, tt.msg)
		}
	}

	// 100 small entries fit.
	tok, err := SignTicket(testSecret, TicketPayload{KeyID: 1, ExpiresAt: exp,
		Narrowing: &Restriction{Systems: manyInts(50), Talkgroups: manyTGs(50)}})
	if err != nil || len(tok) > MaxTicketLength {
		t.Errorf("100 small entries: %d bytes, %v", len(tok), err)
	}
}

func TestClampTicketTTL(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:                TicketMinTTL,
		-time.Hour:       TicketMinTTL,
		30 * time.Second: TicketMinTTL,
		time.Minute:      time.Minute,
		10 * time.Minute: 10 * time.Minute,
		time.Hour:        time.Hour,
		2 * time.Hour:    TicketMaxTTL,
	} {
		if got := ClampTicketTTL(in); got != want {
			t.Errorf("ClampTicketTTL(%v) = %v, want %v", in, got, want)
		}
	}
}
