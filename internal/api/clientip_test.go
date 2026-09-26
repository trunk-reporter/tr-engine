package api

import (
	"net/http/httptest"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	for _, spec := range []string{"", "none", "loopback", "private", "loopback,private", "10.1.2.3", "192.0.2.0/24, 2001:db8::/32", " loopback , 198.51.100.7 "} {
		if _, err := ParseTrustedProxies(spec); err != nil {
			t.Errorf("ParseTrustedProxies(%q): unexpected error %v", spec, err)
		}
	}
	for _, spec := range []string{"bogus", "10.0.0.0/33", "1.2.3", "loopback,nope"} {
		if _, err := ParseTrustedProxies(spec); err == nil {
			t.Errorf("ParseTrustedProxies(%q): expected error", spec)
		}
	}
}

func TestTrustedProxiesClientIP(t *testing.T) {
	defaults, _ := ParseTrustedProxies("loopback,private")
	none, _ := ParseTrustedProxies("none")

	cases := []struct {
		name    string
		proxies *TrustedProxies
		remote  string
		xff     []string
		realIP  string
		want    string
	}{
		{
			name:    "untrusted peer: forwarding headers ignored",
			proxies: defaults,
			remote:  "203.0.113.9:4000",
			xff:     []string{"1.2.3.4"},
			realIP:  "5.6.7.8",
			want:    "203.0.113.9",
		},
		{
			name:    "nil proxies trust nobody",
			proxies: nil,
			remote:  "127.0.0.1:4000",
			xff:     []string{"1.2.3.4"},
			want:    "127.0.0.1",
		},
		{
			name:    "none trusts nobody",
			proxies: none,
			remote:  "10.0.0.2:4000",
			xff:     []string{"1.2.3.4"},
			want:    "10.0.0.2",
		},
		{
			name:    "trusted proxy: single hop",
			proxies: defaults,
			remote:  "172.18.0.5:4000",
			xff:     []string{"198.51.100.20"},
			want:    "198.51.100.20",
		},
		{
			name:    "client-supplied prefix is ignored (rightmost untrusted wins)",
			proxies: defaults,
			remote:  "172.18.0.5:4000",
			xff:     []string{"6.6.6.6, 198.51.100.20"},
			want:    "198.51.100.20",
		},
		{
			name:    "chained trusted proxies are skipped",
			proxies: defaults,
			remote:  "127.0.0.1:4000",
			xff:     []string{"198.51.100.20, 10.0.0.7", "192.168.1.1"},
			want:    "198.51.100.20",
		},
		{
			name:    "all hops trusted: leftmost hop",
			proxies: defaults,
			remote:  "127.0.0.1:4000",
			xff:     []string{"10.0.0.3, 10.0.0.7"},
			want:    "10.0.0.3",
		},
		{
			name:    "garbage hop stops the walk at the last verified address",
			proxies: defaults,
			remote:  "127.0.0.1:4000",
			xff:     []string{"198.51.100.20, not-an-ip"},
			want:    "127.0.0.1",
		},
		{
			name:    "X-Real-IP from trusted proxy",
			proxies: defaults,
			remote:  "127.0.0.1:4000",
			realIP:  "198.51.100.30",
			want:    "198.51.100.30",
		},
		{
			name:    "IPv6 peer and IPv4-mapped hop",
			proxies: defaults,
			remote:  "[::1]:4000",
			xff:     []string{"::ffff:198.51.100.40"},
			want:    "198.51.100.40",
		},
		{
			name:    "explicit single address",
			proxies: mustParseTrusted(t, "192.0.2.10"),
			remote:  "192.0.2.10:4000",
			xff:     []string{"198.51.100.50"},
			want:    "198.51.100.50",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}
			if got := tc.proxies.ClientIP(req); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func mustParseTrusted(t *testing.T, spec string) *TrustedProxies {
	t.Helper()
	tp, err := ParseTrustedProxies(spec)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}
