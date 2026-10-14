package main

// End-to-end tests against a real HTTP server, driving the protocol the same
// way DankNooner's MultiplayerWebRTC (multiplayer_webrtc.gd) does.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const readTimeout = 5 * time.Second

func testConfig() Config {
	return Config{MaxConns: 100, MaxConnsPerIP: 100, MaxLobbies: 100, RealIPHeaders: []string{"X-Real-IP"}, AllowedOrigins: []string{"https://ssebs.github.io"}}
}

func startServer(t *testing.T, cfg Config) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(newHub(cfg).ServeWS))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func tryDial(t *testing.T, url string, hdr http.Header) (*wsClient, *http.Response, error) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, hdr)
	if err != nil {
		return nil, resp, err
	}
	t.Cleanup(func() { conn.Close() })
	return &wsClient{t: t, conn: conn}, resp, nil
}

func dial(t *testing.T, url string, hdr http.Header) *wsClient {
	t.Helper()
	c, _, err := tryDial(t, url, hdr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func (c *wsClient) send(typ, id int, data string) {
	c.t.Helper()
	if err := c.conn.WriteJSON(sigMsg{Type: typ, ID: id, Data: data}); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *wsClient) read() sigMsg {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	var m sigMsg
	if err := c.conn.ReadJSON(&m); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return m
}

// expect reads the next message and checks its type and id (id < 0 = any).
func (c *wsClient) expect(typ, id int) sigMsg {
	c.t.Helper()
	m := c.read()
	if m.Type != typ || (id >= 0 && m.ID != id) {
		c.t.Fatalf("got %+v, want type=%d id=%d", m, typ, id)
	}
	return m
}

// expectClosed asserts the server closes the connection without sending any
// further messages, and returns the read error.
func (c *wsClient) expectClosed() error {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	_, raw, err := c.conn.ReadMessage()
	if err == nil {
		c.t.Fatalf("expected close, got message %s", raw)
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		c.t.Fatalf("expected close, connection stayed open: %v", err)
	}
	return err
}

func (c *wsClient) expectNormalClose(reason string) {
	c.t.Helper()
	err := c.expectClosed()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseNormalClosure || ce.Text != reason {
		c.t.Fatalf("got %v, want normal close %q", err, reason)
	}
}

// hostLobby connects like start_server(): JOIN with id=1 and no code.
func hostLobby(t *testing.T, url string) (*wsClient, string) {
	t.Helper()
	h := dial(t, url, nil)
	h.send(msgJoin, 1, "")
	h.expect(msgID, targetServer)
	code := h.expect(msgJoin, 0).Data
	if len(code) != 32 {
		t.Fatalf("lobby code %q, want 32 chars", code)
	}
	return h, code
}

// clientJoin connects like connect_client(code) and checks the client's
// messages arrive as ID, PEER_CONNECT(host), JOIN(code). It returns the
// client's peer ID; the host's PEER_CONNECT is left unread.
func clientJoin(t *testing.T, url, code string) (*wsClient, int) {
	t.Helper()
	c := dial(t, url, nil)
	c.send(msgJoin, 1, code)
	id := c.expect(msgID, -1).ID
	if id < 2 {
		t.Fatalf("client got peer id %d, want >= 2", id)
	}
	c.expect(msgPeerConnect, targetServer)
	if got := c.expect(msgJoin, 0).Data; got != code {
		t.Fatalf("JOIN reply code %q, want %q", got, code)
	}
	return c, id
}

// connectClient joins a client and consumes the host's PEER_CONNECT for it.
func connectClient(t *testing.T, url string, host *wsClient, code string) (*wsClient, int) {
	t.Helper()
	c, id := clientJoin(t, url, code)
	host.expect(msgPeerConnect, id)
	return c, id
}

// waitFor polls cond until it's true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWebRTCHandshake(t *testing.T) {
	url := startServer(t, testConfig())
	host, code := hostLobby(t, url)
	client, clientID := clientJoin(t, url, code)

	// The client (higher ID) creates the offer as soon as it sees PEER_CONNECT.
	// Godot drops offers from unknown peers, so the host must get PEER_CONNECT first.
	client.send(msgOffer, targetServer, "v=0 offer")
	host.expect(msgPeerConnect, clientID)
	if got := host.expect(msgOffer, clientID).Data; got != "v=0 offer" {
		t.Fatalf("offer data %q", got)
	}

	host.send(msgAnswer, clientID, "v=0 answer")
	if got := client.expect(msgAnswer, targetServer).Data; got != "v=0 answer" {
		t.Fatalf("answer data %q", got)
	}

	// Candidates use the "mid\nindex\nsdp" format and must pass through verbatim.
	cand := "0\n0\ncandidate:1 1 UDP 2122317823 192.168.1.5 50000 typ host"
	client.send(msgCandidate, targetServer, cand)
	if got := host.expect(msgCandidate, clientID).Data; got != cand {
		t.Fatalf("host candidate %q, want %q", got, cand)
	}
	host.send(msgCandidate, clientID, cand)
	if got := client.expect(msgCandidate, targetServer).Data; got != cand {
		t.Fatalf("client candidate %q, want %q", got, cand)
	}
}

func TestMultipleClientsOnlySeeHost(t *testing.T) {
	url := startServer(t, testConfig())
	host, code := hostLobby(t, url)
	c1, id1 := connectClient(t, url, host, code)
	c2, id2 := connectClient(t, url, host, code)
	if id1 == id2 {
		t.Fatalf("clients share peer id %d", id1)
	}

	// c1 must not have been told about c2: its next message is the relay below.
	host.send(msgOffer, id1, "for c1")
	if got := c1.expect(msgOffer, targetServer).Data; got != "for c1" {
		t.Fatalf("c1 got %q", got)
	}
	host.send(msgOffer, id2, "for c2")
	if got := c2.expect(msgOffer, targetServer).Data; got != "for c2" {
		t.Fatalf("c2 got %q", got)
	}
}

func TestRelayToUnknownPeerIsDropped(t *testing.T) {
	url := startServer(t, testConfig())
	host, code := hostLobby(t, url)
	client, clientID := connectClient(t, url, host, code)

	host.send(msgOffer, 999999, "nobody")
	host.send(msgOffer, clientID, "real")
	if got := client.expect(msgOffer, targetServer).Data; got != "real" {
		t.Fatalf("client got %q, want only the real offer", got)
	}
}

func TestRelayIsScopedToLobby(t *testing.T) {
	url := startServer(t, testConfig())
	hostA, codeA := hostLobby(t, url)
	_, idA := connectClient(t, url, hostA, codeA)
	hostB, codeB := hostLobby(t, url)
	clientB, idB := connectClient(t, url, hostB, codeB)

	// Host B can't reach lobby A's client by ID; only its own client gets data.
	hostB.send(msgOffer, idA, "cross-lobby")
	hostB.send(msgOffer, idB, "same-lobby")
	if got := clientB.expect(msgOffer, targetServer).Data; got != "same-lobby" {
		t.Fatalf("clientB got %q", got)
	}
}

func TestClientLeaveNotifiesHost(t *testing.T) {
	url := startServer(t, testConfig())
	host, code := hostLobby(t, url)
	client, clientID := connectClient(t, url, host, code)

	client.conn.Close()
	host.expect(msgPeerDisconnect, clientID)
}

func TestHostLeaveClosesLobby(t *testing.T) {
	url := startServer(t, testConfig())
	host, code := hostLobby(t, url)
	client, _ := connectClient(t, url, host, code)

	host.conn.Close()
	client.expectClosed()

	late := dial(t, url, nil)
	late.send(msgJoin, 1, code)
	late.expectClosed()
}

func TestJoinUnknownLobby(t *testing.T) {
	url := startServer(t, testConfig())
	c := dial(t, url, nil)
	c.send(msgJoin, 1, "doesnotexistdoesnotexistdoesnotx")
	c.expectClosed()
}

func TestSeal(t *testing.T) {
	url := startServer(t, testConfig())
	host, code := hostLobby(t, url)
	client, clientID := connectClient(t, url, host, code)

	// Only the host may seal; a client's SEAL is ignored.
	client.send(msgSeal, 0, "")
	client.send(msgOffer, targetServer, "sync") // handled after the SEAL, so proves it was processed
	host.expect(msgOffer, clientID)
	other, _ := connectClient(t, url, host, code)

	host.send(msgSeal, 0, "")
	host.expect(msgSeal, 0)
	client.expect(msgSeal, 0)
	other.expect(msgSeal, 0)

	late := dial(t, url, nil)
	late.send(msgJoin, 1, code)
	late.expectClosed()

	// Signaling is over after SEAL: relays are dropped, connections stay up.
	host.send(msgOffer, clientID, "after seal")
	client.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, raw, err := client.conn.ReadMessage(); err == nil {
		t.Fatalf("relay after seal was delivered: %s", raw)
	}
}

func TestProtocolViolationsDisconnect(t *testing.T) {
	url := startServer(t, testConfig())
	tests := []struct {
		name string
		run  func(c *wsClient)
	}{
		{"bad json", func(c *wsClient) { c.conn.WriteMessage(websocket.TextMessage, []byte("{nope")) }},
		{"unknown type", func(c *wsClient) { c.send(42, 0, "") }},
		{"relay before join", func(c *wsClient) { c.send(msgOffer, targetServer, "x") }},
		{"seal before join", func(c *wsClient) { c.send(msgSeal, 0, "") }},
		{"double join", func(c *wsClient) {
			c.send(msgJoin, 1, "")
			c.expect(msgID, targetServer)
			c.expect(msgJoin, 0)
			c.send(msgJoin, 1, "")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := dial(t, url, nil)
			tt.run(c)
			c.expectClosed()
		})
	}
}

func TestJoinTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.JoinTimeout = 200 * time.Millisecond
	url := startServer(t, cfg)

	c := dial(t, url, nil)
	start := time.Now()
	c.expectClosed()
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("idle connection closed after %v, want ~%v", el, cfg.JoinTimeout)
	}
}

func TestOriginRejected(t *testing.T) {
	url := startServer(t, testConfig())
	_, resp, err := tryDial(t, url, http.Header{"Origin": {"https://evil.example"}})
	if err == nil {
		t.Fatal("upgrade with disallowed Origin succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %v, want 403", resp)
	}
}

func TestAllowedOriginConnects(t *testing.T) {
	url := startServer(t, testConfig())
	// The web build's browser WebSocket always sends its page's Origin.
	c := dial(t, url, http.Header{"Origin": {"https://ssebs.github.io"}})
	c.send(msgJoin, 1, "")
	c.expect(msgID, targetServer)
}

func TestReadLimit(t *testing.T) {
	url := startServer(t, testConfig())
	host, code := hostLobby(t, url)
	client, clientID := connectClient(t, url, host, code)

	// A large but legitimate SDP still relays.
	big := strings.Repeat("a", 60<<10)
	client.send(msgOffer, targetServer, big)
	if got := host.expect(msgOffer, clientID).Data; got != big {
		t.Fatalf("60KB offer corrupted (len %d)", len(got))
	}

	// Anything over 64KB disconnects the sender.
	client.send(msgOffer, targetServer, strings.Repeat("a", 70<<10))
	host.expect(msgPeerDisconnect, clientID)
}

func TestConnectionCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConns = 2
	url := startServer(t, cfg)

	first := dial(t, url, nil)
	dial(t, url, nil)
	dial(t, url, nil).expectNormalClose(errServerFull.Error())

	// Freed slots are reusable.
	first.conn.Close()
	waitFor(t, readTimeout, "slot to free up", func() bool {
		c, _, err := tryDial(t, url, nil)
		if err != nil {
			return false
		}
		c.send(msgJoin, 1, "")
		c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		var m sigMsg
		return c.conn.ReadJSON(&m) == nil && m.Type == msgID
	})
}

func TestPerIPCapBehindProxy(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConnsPerIP = 2
	url := startServer(t, cfg)

	// httptest's loopback peer stands in for NPM, which sets X-Real-IP.
	alice := http.Header{"X-Real-IP": {"198.51.100.1"}}
	bob := http.Header{"X-Real-IP": {"198.51.100.2"}}

	dial(t, url, alice)
	dial(t, url, alice)
	dial(t, url, alice).expectNormalClose(errTooManyConn.Error())

	// A different client behind the same proxy isn't affected.
	h := dial(t, url, bob)
	h.send(msgJoin, 1, "")
	h.expect(msgID, targetServer)
}

func TestLobbyCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxLobbies = 1
	url := startServer(t, cfg)

	host, code := hostLobby(t, url)

	second := dial(t, url, nil)
	second.send(msgJoin, 1, "")
	second.expectNormalClose(errLobbyLimit.Error())

	// Joining an existing lobby is unaffected by the cap.
	connectClient(t, url, host, code)

	// Once the host leaves, a new lobby can be created.
	host.conn.Close()
	waitFor(t, readTimeout, "lobby slot to free up", func() bool {
		c, _, err := tryDial(t, url, nil)
		if err != nil {
			return false
		}
		c.send(msgJoin, 1, "")
		c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		var m sigMsg
		return c.conn.ReadJSON(&m) == nil && m.Type == msgID
	})
}

// A peer that stops reading must not stall other lobbies, and must be dropped
// once a write to it times out.
func TestSlowPeerDoesNotStallOtherLobbies(t *testing.T) {
	cfg := testConfig()
	cfg.WriteTimeout = 3 * time.Second
	url := startServer(t, cfg)

	hostA, codeA := hostLobby(t, url)
	_, stalledID := connectClient(t, url, hostA, codeA) // never reads again

	// Flood the stalled peer until the server's write to it blocks, which in
	// turn stops the server reading from hostA.
	var sent atomic.Int64
	var stop atomic.Bool
	done := make(chan struct{})
	payload := strings.Repeat("x", 48<<10)
	go func() {
		defer close(done)
		for !stop.Load() {
			if hostA.conn.WriteJSON(sigMsg{Type: msgOffer, ID: stalledID, Data: payload}) != nil {
				return
			}
			sent.Add(1)
		}
	}()
	t.Cleanup(func() {
		stop.Store(true)
		hostA.conn.Close()
		<-done
	})

	var stalledAt int64
	waitFor(t, readTimeout, "relay to stall", func() bool {
		before := sent.Load()
		time.Sleep(250 * time.Millisecond)
		stalledAt = sent.Load()
		return before > 0 && stalledAt == before
	})

	// While that write is blocked, an unrelated lobby must work normally.
	start := time.Now()
	hostB, codeB := hostLobby(t, url)
	clientB, idB := connectClient(t, url, hostB, codeB)
	clientB.send(msgOffer, targetServer, "offer")
	hostB.expect(msgOffer, idB)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("other lobby took %v while a peer was stalled", el)
	}
	if sent.Load() != stalledAt {
		t.Fatal("stall ended before the other lobby was exercised; test proved nothing")
	}

	// The write deadline fires, the stalled peer is dropped and the host told.
	hostA.expect(msgPeerDisconnect, stalledID)
}
