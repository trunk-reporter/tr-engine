package auth

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseExpires(t *testing.T) {
	now := time.Date(2026, 9, 26, 15, 4, 5, 0, time.FixedZone("EDT", -4*3600))
	tests := []struct {
		in      string
		want    time.Time
		wantErr string
	}{
		{"720h", now.Add(720 * time.Hour), ""},
		{"90m", now.Add(90 * time.Minute), ""},
		{"1h30m", now.Add(90 * time.Minute), ""},
		{"1.5h", now.Add(90 * time.Minute), ""},
		{"90d", now.Add(90 * 24 * time.Hour), ""},
		{"1d", now.Add(24 * time.Hour), ""},
		{"007d", now.Add(7 * 24 * time.Hour), ""},
		{"  30d ", now.Add(30 * 24 * time.Hour), ""},
		{"2026-12-31", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), ""},
		{"2026-12-31T00:00:00Z", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), ""},
		{"2026-12-31T00:00:00-05:00", time.Date(2026, 12, 31, 5, 0, 0, 0, time.UTC), ""},
		{"2026-12-31T00:00:00.5Z", time.Date(2026, 12, 31, 0, 0, 0, 5e8, time.UTC), ""},
		{"2026-09-27", time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), ""}, // 20:00 local on the 26th is before it

		{"", time.Time{}, "empty"},
		{"   ", time.Time{}, "empty"},
		{"d", time.Time{}, "invalid expiry"},
		{"0d", time.Time{}, "not in the future"},
		{"-5d", time.Time{}, "invalid expiry"},
		{"1.5d", time.Time{}, "invalid expiry"},
		{"5 d", time.Time{}, "invalid expiry"},
		{"90days", time.Time{}, "invalid expiry"},
		{"99999999999d", time.Time{}, "too far"},
		{"99999999999999999999999d", time.Time{}, "too far"},
		{"0s", time.Time{}, "not in the future"},
		{"-1h", time.Time{}, "not in the future"},
		{"2026-09-26", time.Time{}, "not in the future"}, // midnight UTC today, already past
		{"2020-01-01T00:00:00Z", time.Time{}, "not in the future"},
		{"2026-13-01", time.Time{}, "invalid expiry"},
		{"2026/12/31", time.Time{}, "invalid expiry"},
		{"12/31/2026", time.Time{}, "invalid expiry"},
		{"2026-12-31 00:00:00", time.Time{}, "invalid expiry"},
		{"never", time.Time{}, "invalid expiry"},
		{"1y", time.Time{}, "invalid expiry"},
	}
	for _, tt := range tests {
		got, err := ParseExpires(tt.in, now)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseExpires(%q) = %v, %v; want error containing %q", tt.in, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseExpires(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if !got.Equal(tt.want) || got.Location() != time.UTC {
			t.Errorf("ParseExpires(%q) = %v, want %v in UTC", tt.in, got, tt.want.UTC())
		}
	}
}

func TestSanitizeActor(t *testing.T) {
	long := strings.Repeat("é", MaxActorLength+50)
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"alice", "alice"},
		{"  alice#1234  ", "alice#1234"},
		{"Ålice Ünïcode 名前", "Ålice Ünïcode 名前"},
		{"bob\r\nX-Injected: 1", "bobX-Injected: 1"},
		{"tab\there", "tabhere"},
		{"nul\x00byte", "nulbyte"},
		{"del\x7fchar", "delchar"},
		{"c1\u0085control", "c1control"},
		{"\u202egnp.exe", "gnp.exe"},                         // bidi override (Cf)
		{"zero\u200bwidth\u200djoin\ufeff", "zerowidthjoin"}, // ZWSP is Cf, ZWJ is Cf, BOM is Cf
		{"\u2066isolate\u2069", "isolate"},
		{"soft\u00adhyphen", "softhyphen"},
		{"\u200b  spaced  \u200b", "spaced"},
		{"bad\xff\xfeutf8", "badutf8"},
		{"\x00\x01\x02", ""},
		{"\u200b", ""},
		{"inner  space", "inner  space"},
		{long, strings.Repeat("é", MaxActorLength)},
		{strings.Repeat("a", MaxActorLength-1) + " b", strings.Repeat("a", MaxActorLength-1)}, // cut leaves a trailing space
	}
	for _, tt := range tests {
		got := SanitizeActor(tt.in)
		if got != tt.want {
			t.Errorf("SanitizeActor(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > MaxActorLength {
			t.Errorf("SanitizeActor(%q) = %q: invalid UTF-8 or too long", tt.in, got)
		}
	}
}
