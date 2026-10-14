package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	msgJoin           = 0
	msgID             = 1
	msgPeerConnect    = 2
	msgPeerDisconnect = 3
	msgOffer          = 4
	msgAnswer         = 5
	msgCandidate      = 6
	msgSeal           = 7

	// TARGET_PEER_SERVER in Godot's MultiplayerPeer — means "send to host"
	targetServer = 1

	sigTimeout = 10 * time.Second
	sealDelay  = 10 * time.Second

	// writeWait bounds every write so a peer that stops reading can't hang the writer.
	writeWait = 5 * time.Second
	// maxMessageSize caps inbound frames; SDP/ICE payloads are a few KB at most.
	maxMessageSize = 64 << 10
)

// Defaults match the production deployment: the web build is served from
// GitHub Pages and danknooner.ssebs.com, and traffic arrives via
// Cloudflare -> Nginx Proxy Manager.
const (
	defaultAllowedOrigins = "https://ssebs.github.io,https://danknooner.ssebs.com"
	// Cloudflare sets CF-Connecting-IP (overwriting any client value). NPM's
	// X-Real-IP is the fallback for deployments without Cloudflare; behind
	// Cloudflare it holds the Cloudflare edge IP, not the player's.
	defaultRealIPHeaders = "CF-Connecting-IP,X-Real-IP"
)

var (
	errServerFull  = errors.New("server full")
	errTooManyConn = errors.New("too many connections from this address")
	errLobbyLimit  = errors.New("lobby limit reached")
	errNoLobby     = errors.New("lobby not found")
	errSealed      = errors.New("lobby sealed")
)

// Config holds the hub's abuse limits.
type Config struct {
	MaxConns      int
	MaxConnsPerIP int
	MaxLobbies    int
	// RealIPHeaders names headers set by reverse proxies that carry the client
	// IP, checked in order; the first holding a valid IP wins. They are only
	// honored when the TCP peer is a loopback or private address (i.e. the
	// proxy), so internet clients can't spoof them directly. Empty means always
	// use the TCP remote address.
	RealIPHeaders []string
	// AllowedOrigins lists browser origins (e.g. https://ssebs.github.io) that
	// may connect. Requests without an Origin header (native Godot clients)
	// are always allowed.
	AllowedOrigins []string

	// JoinTimeout is how long a new connection has to send JOIN, and
	// WriteTimeout bounds every write. Zero means sigTimeout / writeWait.
	JoinTimeout  time.Duration
	WriteTimeout time.Duration
}

type sigMsg struct {
	Type int    `json:"type"`
	ID   int    `json:"id"`
	Data string `json:"data"`
}

// Peer represents a connected WebSocket client.
type Peer struct {
	id        int
	conn      *websocket.Conn
	lobby     string
	mu        sync.Mutex // guards writes
	writeWait time.Duration
	joinedAt  time.Time
}

// send writes one message with a deadline. A peer that can't be written to is
// closed, which ends its read loop and removes it from its lobby.
func (p *Peer) send(msgType, id int, data string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn.SetWriteDeadline(time.Now().Add(p.writeWait))
	err := p.conn.WriteJSON(sigMsg{Type: msgType, ID: id, Data: data})
	if err != nil {
		p.conn.Close()
	}
	return err
}

// outMsg is a message queued while holding the hub lock and delivered after
// releasing it, so a slow peer can never stall other lobbies.
type outMsg struct {
	to  *Peer
	msg sigMsg
}

func deliver(msgs []outMsg) {
	for _, m := range msgs {
		m.to.send(m.msg.Type, m.msg.ID, m.msg.Data)
	}
}

// Lobby tracks the host and all connected peers for one game session.
type Lobby struct {
	host   int
	peers  map[int]*Peer
	sealed bool
}

func newLobby(hostID int) *Lobby {
	return &Lobby{host: hostID, peers: make(map[int]*Peer)}
}

// join adds the peer to the lobby and returns the messages to deliver: first
// those for the joining peer (its ID, then the peers it can see), then the
// notifications for existing peers. In client-server mode only the host is
// visible to newly joining clients (and vice-versa).
func (l *Lobby) join(p *Peer) (toJoiner, toOthers []outMsg, ok bool) {
	if l.sealed {
		return nil, nil, false
	}
	assignedID := p.id
	if p.id == l.host {
		assignedID = targetServer // host always appears as ID 1
	}
	toJoiner = append(toJoiner, outMsg{p, sigMsg{Type: msgID, ID: assignedID}})

	for _, existing := range l.peers {
		if existing.id != l.host {
			continue // only host visible in client-server mode
		}
		toOthers = append(toOthers, outMsg{existing, sigMsg{Type: msgPeerConnect, ID: p.id}})
		existingID := existing.id
		if existingID == l.host {
			existingID = targetServer
		}
		toJoiner = append(toJoiner, outMsg{p, sigMsg{Type: msgPeerConnect, ID: existingID}})
	}
	l.peers[p.id] = p
	return toJoiner, toOthers, true
}

// leave removes a peer and returns the notifications to deliver and the peers
// to disconnect. destroy is true if the lobby should be destroyed (host left).
func (l *Lobby) leave(p *Peer) (notify []outMsg, kick []*Peer, destroy bool) {
	delete(l.peers, p.id)
	destroy = p.id == l.host
	if l.sealed {
		return nil, nil, destroy
	}
	for _, other := range l.peers {
		if destroy {
			kick = append(kick, other)
		} else {
			notify = append(notify, outMsg{other, sigMsg{Type: msgPeerDisconnect, ID: p.id}})
		}
	}
	return notify, kick, destroy
}

// seal closes the lobby for new connections and returns the SEAL broadcast.
func (l *Lobby) seal(peerID int) ([]outMsg, bool) {
	if l.host != peerID {
		return nil, false
	}
	l.sealed = true
	msgs := make([]outMsg, 0, len(l.peers))
	for _, p := range l.peers {
		msgs = append(msgs, outMsg{p, sigMsg{Type: msgSeal}})
	}
	l.peers = make(map[int]*Peer) // signaling no longer needed
	return msgs, true
}

// Hub manages all active lobbies.
type Hub struct {
	cfg      Config
	upgrader websocket.Upgrader

	mu        sync.Mutex
	lobbies   map[string]*Lobby
	conns     int
	connsByIP map[string]int
}

func newHub(cfg Config) *Hub {
	if cfg.JoinTimeout <= 0 {
		cfg.JoinTimeout = sigTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = writeWait
	}
	h := &Hub{
		cfg:       cfg,
		lobbies:   make(map[string]*Lobby),
		connsByIP: make(map[string]int),
	}
	h.upgrader.CheckOrigin = h.checkOrigin
	return h
}

// checkOrigin allows native Godot clients, which send no Origin header, and
// browsers on an allowed origin (the web build). Refusing other origins stops
// arbitrary web pages from driving the server via visitors' browsers.
func (h *Hub) checkOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) > 1 {
		return false
	}
	for _, allowed := range h.cfg.AllowedOrigins {
		if allowed != "" && strings.EqualFold(origins[0], strings.TrimSuffix(allowed, "/")) {
			return true
		}
	}
	return false
}

// splitList splits a comma-separated flag value, dropping empty entries.
func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// randIntn returns a uniform random int in [0, n) from crypto/rand.
func randIntn(n int) int {
	un := uint32(n)
	limit := math.MaxUint32 - math.MaxUint32%un // reject the biased tail
	var b [4]byte
	for {
		rand.Read(b[:]) // never fails since Go 1.24
		if v := binary.LittleEndian.Uint32(b[:]); v < limit {
			return int(v % un)
		}
	}
}

const alfnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomCode returns an unguessable lobby code; the code is the lobby's only auth.
func randomCode(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alfnum[randIntn(len(alfnum))]
	}
	return string(b)
}

// clientIP returns the key used for per-IP connection limits. IPv6 clients
// are grouped by /64, since one host typically controls a whole /64.
func (h *Hub) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()

	if addr.IsLoopback() || addr.IsPrivate() {
		for _, name := range h.cfg.RealIPHeaders {
			vals := r.Header.Values(name)
			if len(vals) == 0 {
				continue
			}
			// Proxies append to X-Forwarded-For; the last entry is the one our proxy added.
			v := vals[len(vals)-1]
			if i := strings.LastIndexByte(v, ','); i >= 0 {
				v = v[i+1:]
			}
			if fwd, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
				addr = fwd.Unmap()
				break
			}
		}
	}

	if addr.Is6() {
		prefix, _ := addr.Prefix(64)
		return prefix.String()
	}
	return addr.String()
}

// acquireConn reserves a connection slot for ip; pair with releaseConn.
func (h *Hub) acquireConn(ip string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns >= h.cfg.MaxConns {
		return errServerFull
	}
	if h.connsByIP[ip] >= h.cfg.MaxConnsPerIP {
		return errTooManyConn
	}
	h.conns++
	h.connsByIP[ip]++
	return nil
}

func (h *Hub) releaseConn(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns--
	if h.connsByIP[ip]--; h.connsByIP[ip] <= 0 {
		delete(h.connsByIP, ip)
	}
}

// joinLobby creates a new lobby (code="") or joins an existing one, then sends
// the joining peer its ID, visible peers and JOIN reply before notifying the
// host, so the joiner is fully set up before any OFFER can be relayed to it.
func (h *Hub) joinLobby(p *Peer, code string) (string, error) {
	h.mu.Lock()
	if code == "" {
		if len(h.lobbies) >= h.cfg.MaxLobbies {
			h.mu.Unlock()
			return "", errLobbyLimit
		}
		for {
			code = randomCode(32)
			if _, exists := h.lobbies[code]; !exists {
				break
			}
		}
		h.lobbies[code] = newLobby(p.id)
	} else {
		if _, exists := h.lobbies[code]; !exists {
			h.mu.Unlock()
			return "", errNoLobby
		}
	}
	toJoiner, toOthers, ok := h.lobbies[code].join(p)
	if !ok {
		h.mu.Unlock()
		return "", errSealed
	}
	p.lobby = code
	h.mu.Unlock()

	log.Printf("peer %d joined lobby %s", p.id, code)
	deliver(toJoiner)
	p.send(msgJoin, 0, code)
	deliver(toOthers)
	return code, nil
}

func (h *Hub) removePeer(p *Peer) {
	if p.lobby == "" {
		return
	}
	h.mu.Lock()
	lobby, ok := h.lobbies[p.lobby]
	if !ok {
		h.mu.Unlock()
		return
	}
	notify, kick, destroy := lobby.leave(p)
	if destroy {
		delete(h.lobbies, p.lobby)
	}
	h.mu.Unlock()

	if destroy {
		log.Printf("lobby %s closed (host left)", p.lobby)
	}
	deliver(notify)
	for _, other := range kick {
		other.conn.Close()
	}
}

func (h *Hub) relay(p *Peer, msg sigMsg) bool {
	h.mu.Lock()
	lobby, ok := h.lobbies[p.lobby]
	if !ok {
		h.mu.Unlock()
		return false
	}

	destID := msg.ID
	if destID == targetServer {
		destID = lobby.host
	}

	dest, ok := lobby.peers[destID]
	if !ok || dest.lobby != p.lobby {
		h.mu.Unlock()
		return false
	}

	sourceID := p.id
	if p.id == lobby.host {
		sourceID = targetServer
	}
	h.mu.Unlock()

	return dest.send(msg.Type, sourceID, msg.Data) == nil
}

func (h *Hub) sealLobby(p *Peer) bool {
	h.mu.Lock()
	lobby, ok := h.lobbies[p.lobby]
	if !ok {
		h.mu.Unlock()
		return false
	}
	msgs, ok := lobby.seal(p.id)
	h.mu.Unlock()
	if !ok {
		return false
	}

	deliver(msgs)
	// Remove the sealed lobby after the delay in a background goroutine.
	code := p.lobby
	go func() {
		time.Sleep(sealDelay)
		h.mu.Lock()
		delete(h.lobbies, code)
		h.mu.Unlock()
		log.Printf("lobby %s cleaned up after seal", code)
	}()
	return true
}

// closeNormal sends a normal-closure frame so the client sees a clean
// rejection, then closes the connection.
func (h *Hub) closeNormal(conn *websocket.Conn, reason string) {
	conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason),
		time.Now().Add(h.cfg.WriteTimeout))
	conn.Close()
}

// ServeWS upgrades the HTTP connection and handles one peer for its lifetime.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	ip := h.clientIP(r)
	limitErr := h.acquireConn(ip)
	if limitErr == nil {
		defer h.releaseConn(ip)
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	if limitErr != nil {
		log.Printf("rejecting connection from %s: %v", ip, limitErr)
		h.closeNormal(conn, limitErr.Error())
		return
	}
	conn.SetReadLimit(maxMessageSize)

	p := &Peer{
		id:        randIntn(math.MaxInt32-1) + 2, // Godot peer IDs: [1, 2^31-1]; 1 is reserved for host
		conn:      conn,
		writeWait: h.cfg.WriteTimeout,
		joinedAt:  time.Now(),
	}
	log.Printf("peer %d connected from %s", p.id, r.RemoteAddr)

	defer func() {
		h.removePeer(p)
		conn.Close()
		log.Printf("peer %d disconnected", p.id)
	}()

	// Enforce join timeout: peer must join a lobby within JoinTimeout.
	conn.SetReadDeadline(time.Now().Add(h.cfg.JoinTimeout))

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var msg sigMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("peer %d: bad JSON: %v", p.id, err)
			return
		}

		switch msg.Type {
		case msgJoin:
			if p.lobby != "" {
				return // already in a lobby
			}
			// joinLobby sends the JOIN reply itself, ordered before the host is notified.
			if _, err := h.joinLobby(p, msg.Data); err != nil {
				log.Printf("peer %d: join failed for code %q: %v", p.id, msg.Data, err)
				if errors.Is(err, errLobbyLimit) {
					h.closeNormal(conn, err.Error())
				}
				return
			}
			// Joined — clear the deadline, stay connected as long as needed.
			conn.SetReadDeadline(time.Time{})
			// Add keepalive pings
			const pingInterval = 30 * time.Second
			const pongTimeout = 10 * time.Second

			conn.SetPongHandler(func(string) error {
				conn.SetReadDeadline(time.Time{})
				return nil
			})

			go func() {
				ticker := time.NewTicker(pingInterval)
				defer ticker.Stop()
				for range ticker.C {
					p.mu.Lock()
					conn.SetWriteDeadline(time.Now().Add(pongTimeout))
					err := conn.WriteMessage(websocket.PingMessage, nil)
					conn.SetWriteDeadline(time.Time{})
					p.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		case msgSeal:
			if p.lobby == "" {
				return
			}
			h.sealLobby(p)

		case msgOffer, msgAnswer, msgCandidate:
			if p.lobby == "" {
				return
			}
			h.relay(p, msg)

		default:
			log.Printf("peer %d: unknown message type %d", p.id, msg.Type)
			return
		}
	}
}
