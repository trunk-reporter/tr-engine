package auth

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxActorLength is the most runes of an X-Actor header that are kept.
const MaxActorLength = 200

// maxExpiresDays is the largest "Nd" whose duration fits in a time.Duration.
const maxExpiresDays = math.MaxInt64 / int64(24*time.Hour)

// ParseExpires parses the CLI --expires syntax (§10.2) relative to now:
//
//   - a Go duration ("720h", "90m");
//   - a whole number of days with a "d" suffix ("90d");
//   - a date ("2026-12-31"), meaning midnight UTC;
//   - an RFC3339 time ("2026-12-31T00:00:00Z").
//
// The result is in UTC and must be after now.
func ParseExpires(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	var t time.Time
	if days, ok := strings.CutSuffix(s, "d"); ok && days != "" && isASCIIDigits(days) {
		n, err := strconv.ParseInt(days, 10, 64)
		if err != nil || n > maxExpiresDays {
			return time.Time{}, fmt.Errorf("expiry %q is too far in the future", s)
		}
		t = now.Add(time.Duration(n) * 24 * time.Hour)
	} else if d, err := time.Parse(time.DateOnly, s); err == nil {
		t = d
	} else if d, err := time.Parse(time.RFC3339, s); err == nil {
		t = d
	} else if d, err := time.ParseDuration(s); err == nil {
		t = now.Add(d)
	} else {
		if s == "" {
			return time.Time{}, errors.New("expiry is empty")
		}
		return time.Time{}, fmt.Errorf("invalid expiry %q: use a duration (720h), days (90d), a date (2026-12-31) or an RFC3339 time (2026-12-31T00:00:00Z)", s)
	}
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("expiry %q is not in the future", s)
	}
	return t.UTC(), nil
}

func isASCIIDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// SanitizeActor cleans an X-Actor header value (§9): invalid UTF-8 and every
// rune in Unicode categories Cc (controls, including NUL and line breaks) and
// Cf (format characters such as bidi overrides and zero-width joiners) are
// removed, surrounding white space is trimmed, and the result is cut to
// MaxActorLength runes. An empty result means no actor.
func SanitizeActor(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxActorLength {
		s = strings.TrimSpace(string([]rune(s)[:MaxActorLength]))
	}
	return s
}
