package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRandIntnRangeAndCoverage(t *testing.T) {
	seen := make(map[int]bool)
	for range 20000 {
		v := randIntn(len(alfnum))
		if v < 0 || v >= len(alfnum) {
			t.Fatalf("randIntn(%d) = %d, out of range", len(alfnum), v)
		}
		seen[v] = true
	}
	if len(seen) != len(alfnum) {
		t.Fatalf("randIntn hit %d of %d values", len(seen), len(alfnum))
	}
}

func TestPeerIDRange(t *testing.T) {
	for range 10000 {
		// Same expression ServeWS uses; 1 is reserved for the host.
		id := randIntn(math.MaxInt32-1) + 2
		if id < 2 || id > math.MaxInt32 {
			t.Fatalf("peer id %d outside [2, MaxInt32]", id)
		}
	}
}

func TestRandomCode(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		code := randomCode(32)
		if len(code) != 32 {
			t.Fatalf("code %q has length %d", code, len(code))
		}
		if strings.Trim(code, alfnum) != "" {
			t.Fatalf("code %q has characters outside the alphabet", code)
		}
		if seen[code] {
			t.Fatalf("duplicate code %q", code)
		}
		seen[code] = true
	}
}

func TestCheckOrigin(t *testing.T) {
	h := newHub(Config{AllowedOrigins: []string{"https://ssebs.github.io/"}})
	tests := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"no origin (Godot native)", http.Header{}, true},
		{"allowed origin (web build)", http.Header{"Origin": {"https://ssebs.github.io"}}, true},
		{"allowed origin, different case", http.Header{"Origin": {"https://SSEBS.github.io"}}, true},
		{"other origin", http.Header{"Origin": {"https://evil.example"}}, false},
		{"allowed host over http", http.Header{"Origin": {"http://ssebs.github.io"}}, false},
		{"lookalike subdomain", http.Header{"Origin": {"https://ssebs.github.io.evil.example"}}, false},
		{"empty origin header", http.Header{"Origin": {""}}, false},
		{"multiple origin headers", http.Header{"Origin": {"https://ssebs.github.io", "https://evil.example"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header = tt.header
			if got := h.checkOrigin(r); got != tt.want {
				t.Fatalf("checkOrigin = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" CF-Connecting-IP, ,X-Real-IP,")
	if len(got) != 2 || got[0] != "CF-Connecting-IP" || got[1] != "X-Real-IP" {
		t.Fatalf("splitList = %q", got)
	}
	if got := splitList(""); got != nil {
		t.Fatalf("splitList(\"\") = %q, want nil", got)
	}
}

// TestDefaultsCloudflareNPM checks that the flag defaults work for the
// production path: browser/Godot -> Cloudflare -> Nginx Proxy Manager -> container.
func TestDefaultsCloudflareNPM(t *testing.T) {
	h := newHub(Config{
		RealIPHeaders:  splitList(defaultRealIPHeaders),
		AllowedOrigins: splitList(defaultAllowedOrigins),
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.5:41000" // NPM on the Docker network
	r.Header.Set("CF-Connecting-IP", "198.51.100.1")
	r.Header.Set("X-Real-IP", "172.70.1.1") // NPM's view: the Cloudflare edge
	r.Header.Set("X-Forwarded-For", "198.51.100.1, 172.70.1.1")
	if got := h.clientIP(r); got != "198.51.100.1" {
		t.Fatalf("clientIP = %q, want the player's IP from CF-Connecting-IP", got)
	}

	for _, origin := range []string{"https://ssebs.github.io", "https://danknooner.ssebs.com"} {
		r.Header.Set("Origin", origin)
		if !h.checkOrigin(r) {
			t.Fatalf("web build origin %s rejected by default", origin)
		}
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		headers    []string // Config.RealIPHeaders
		remoteAddr string
		reqHeader  http.Header
		want       string
	}{
		{"direct ipv4", nil, "203.0.113.7:5000", nil, "203.0.113.7"},
		{"header disabled", nil, "127.0.0.1:5000", http.Header{"X-Real-Ip": {"198.51.100.1"}}, "127.0.0.1"},
		{"loopback proxy trusted", []string{"X-Real-IP"}, "127.0.0.1:5000", http.Header{"X-Real-Ip": {"198.51.100.1"}}, "198.51.100.1"},
		{"docker network proxy trusted", []string{"X-Real-IP"}, "172.18.0.5:5000", http.Header{"X-Real-Ip": {"198.51.100.1"}}, "198.51.100.1"},
		{"public peer header ignored", []string{"X-Real-IP"}, "203.0.113.7:5000", http.Header{"X-Real-Ip": {"198.51.100.1"}}, "203.0.113.7"},
		{"proxy without header", []string{"X-Real-IP"}, "172.18.0.5:5000", nil, "172.18.0.5"},
		{"garbage header ignored", []string{"X-Real-IP"}, "172.18.0.5:5000", http.Header{"X-Real-Ip": {"not-an-ip"}}, "172.18.0.5"},
		{"xff uses last hop", []string{"X-Forwarded-For"}, "10.0.0.2:5000", http.Header{"X-Forwarded-For": {"1.1.1.1, 198.51.100.9"}}, "198.51.100.9"},
		{"xff multiple header lines", []string{"X-Forwarded-For"}, "10.0.0.2:5000", http.Header{"X-Forwarded-For": {"1.1.1.1", "198.51.100.9"}}, "198.51.100.9"},
		{"ipv6 grouped by /64", nil, "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:5000", nil, "2001:db8:1:2::/64"},
		{"forwarded ipv6 grouped by /64", []string{"X-Real-IP"}, "127.0.0.1:5000", http.Header{"X-Real-Ip": {"2001:db8:1:2::99"}}, "2001:db8:1:2::/64"},
		{"ipv4-mapped ipv6 unmapped", nil, "[::ffff:203.0.113.7]:5000", nil, "203.0.113.7"},
		{"first header wins", []string{"CF-Connecting-IP", "X-Real-IP"}, "172.18.0.5:5000", http.Header{"Cf-Connecting-Ip": {"198.51.100.1"}, "X-Real-Ip": {"172.70.1.1"}}, "198.51.100.1"},
		{"falls back to next header", []string{"CF-Connecting-IP", "X-Real-IP"}, "172.18.0.5:5000", http.Header{"X-Real-Ip": {"198.51.100.2"}}, "198.51.100.2"},
		{"skips invalid header", []string{"CF-Connecting-IP", "X-Real-IP"}, "172.18.0.5:5000", http.Header{"Cf-Connecting-Ip": {"junk"}, "X-Real-Ip": {"198.51.100.2"}}, "198.51.100.2"},
		{"public peer ignores all headers", []string{"CF-Connecting-IP", "X-Real-IP"}, "203.0.113.7:5000", http.Header{"Cf-Connecting-Ip": {"198.51.100.1"}}, "203.0.113.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHub(Config{RealIPHeaders: tt.headers})
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.reqHeader != nil {
				r.Header = tt.reqHeader
			}
			if got := h.clientIP(r); got != tt.want {
				t.Fatalf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAcquireReleaseConn(t *testing.T) {
	h := newHub(Config{MaxConns: 3, MaxConnsPerIP: 2})

	for range 2 {
		if err := h.acquireConn("a"); err != nil {
			t.Fatalf("acquire a: %v", err)
		}
	}
	if err := h.acquireConn("a"); err != errTooManyConn {
		t.Fatalf("third acquire for a: got %v, want %v", err, errTooManyConn)
	}
	if err := h.acquireConn("b"); err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if err := h.acquireConn("c"); err != errServerFull {
		t.Fatalf("acquire past global cap: got %v, want %v", err, errServerFull)
	}

	h.releaseConn("b")
	if _, ok := h.connsByIP["b"]; ok {
		t.Fatal("released IP should be removed from the map")
	}
	h.releaseConn("a")
	if err := h.acquireConn("a"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if h.conns != 2 || h.connsByIP["a"] != 2 {
		t.Fatalf("conns = %d, a = %d, want 2 and 2", h.conns, h.connsByIP["a"])
	}
}

// assertMsgs compares outbound messages by recipient and content, in order.
func assertMsgs(t *testing.T, label string, got []outMsg, want []outMsg) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d messages %+v, want %d", label, len(got), got, len(want))
	}
	for i := range got {
		if got[i].to != want[i].to || got[i].msg != want[i].msg {
			t.Fatalf("%s[%d]: got to=%d %+v, want to=%d %+v",
				label, i, got[i].to.id, got[i].msg, want[i].to.id, want[i].msg)
		}
	}
}

func TestLobbyJoin(t *testing.T) {
	host := &Peer{id: 100}
	c1 := &Peer{id: 200}
	c2 := &Peer{id: 300}
	l := newLobby(host.id)

	toJoiner, toOthers, ok := l.join(host)
	if !ok {
		t.Fatal("host join failed")
	}
	assertMsgs(t, "host toJoiner", toJoiner, []outMsg{{host, sigMsg{Type: msgID, ID: targetServer}}})
	assertMsgs(t, "host toOthers", toOthers, nil)

	toJoiner, toOthers, _ = l.join(c1)
	assertMsgs(t, "c1 toJoiner", toJoiner, []outMsg{
		{c1, sigMsg{Type: msgID, ID: c1.id}},
		{c1, sigMsg{Type: msgPeerConnect, ID: targetServer}},
	})
	assertMsgs(t, "c1 toOthers", toOthers, []outMsg{{host, sigMsg{Type: msgPeerConnect, ID: c1.id}}})

	// Client-server mode: c2 sees only the host, and c1 isn't told about c2.
	toJoiner, toOthers, _ = l.join(c2)
	assertMsgs(t, "c2 toJoiner", toJoiner, []outMsg{
		{c2, sigMsg{Type: msgID, ID: c2.id}},
		{c2, sigMsg{Type: msgPeerConnect, ID: targetServer}},
	})
	assertMsgs(t, "c2 toOthers", toOthers, []outMsg{{host, sigMsg{Type: msgPeerConnect, ID: c2.id}}})

	if len(l.peers) != 3 {
		t.Fatalf("lobby has %d peers, want 3", len(l.peers))
	}
}

func TestLobbyLeave(t *testing.T) {
	host := &Peer{id: 100}
	c1 := &Peer{id: 200}
	c2 := &Peer{id: 300}

	t.Run("client leaves", func(t *testing.T) {
		l := newLobby(host.id)
		l.join(host)
		l.join(c1)
		notify, kick, destroy := l.leave(c1)
		if destroy || len(kick) != 0 {
			t.Fatalf("client leave: destroy=%v kick=%d", destroy, len(kick))
		}
		assertMsgs(t, "notify", notify, []outMsg{{host, sigMsg{Type: msgPeerDisconnect, ID: c1.id}}})
		if _, ok := l.peers[c1.id]; ok {
			t.Fatal("client still in lobby")
		}
	})

	t.Run("host leaves", func(t *testing.T) {
		l := newLobby(host.id)
		l.join(host)
		l.join(c1)
		l.join(c2)
		notify, kick, destroy := l.leave(host)
		if !destroy || len(notify) != 0 || len(kick) != 2 {
			t.Fatalf("host leave: destroy=%v notify=%d kick=%d", destroy, len(notify), len(kick))
		}
	})

	t.Run("sealed lobby is silent", func(t *testing.T) {
		l := newLobby(host.id)
		l.join(host)
		l.join(c1)
		l.seal(host.id)
		notify, kick, _ := l.leave(c1)
		if len(notify) != 0 || len(kick) != 0 {
			t.Fatalf("sealed leave: notify=%d kick=%d", len(notify), len(kick))
		}
	})
}

func TestLobbySeal(t *testing.T) {
	host := &Peer{id: 100}
	c1 := &Peer{id: 200}
	l := newLobby(host.id)
	l.join(host)
	l.join(c1)

	if _, ok := l.seal(c1.id); ok || l.sealed {
		t.Fatal("non-host must not be able to seal")
	}
	msgs, ok := l.seal(host.id)
	if !ok || !l.sealed {
		t.Fatal("host seal failed")
	}
	if len(msgs) != 2 {
		t.Fatalf("seal broadcast to %d peers, want 2", len(msgs))
	}
	for _, m := range msgs {
		if m.msg != (sigMsg{Type: msgSeal}) {
			t.Fatalf("unexpected seal message %+v", m.msg)
		}
	}
	if len(l.peers) != 0 {
		t.Fatal("sealed lobby should drop its peers")
	}
	if _, _, ok := l.join(&Peer{id: 300}); ok {
		t.Fatal("join after seal must fail")
	}
}
