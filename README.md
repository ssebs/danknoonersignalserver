# Dank Nooner Signal Server

WebRTC signaling server for [DankNooner](https://github.com/ssebs/DankNooner). Brokers WebRTC peer connections between players via lobby codes — once peers are connected, all game traffic flows directly peer-to-peer.

https://hub.docker.com/r/ssebs/danknoonersignalserver

## How it works

1. Host connects via WebSocket → server creates a lobby and returns a 32-char code
2. Client connects with that code → server notifies host of new peer
3. Server relays WebRTC offer/answer/ICE candidates between peers
4. Once WebRTC is established, the game traffic is direct (P2P); signaling is no longer needed
5. Host can send `SEAL` to close the lobby to new joiners (auto-cleaned up after 10s)

## Running

### Docker


```bash
docker run -p 9080:9080 ssebs/danknoonersignalserver:latest
```

### Manual

```bash
go run . -addr :9080
```

### Building

```bash
go build -o danknoonersignalserver .
./danknoonersignalserver -addr :9080
```

Or build the Docker image locally:

```bash
docker build -t ssebs/danknoonersignalserver:latest .
```

Default port: `:9080`. The server listens for WebSocket connections at `/`.

### Flags

| Flag                | Default     | Description                                                                                 |
| ------------------- | ----------- | ------------------------------------------------------------------------------------------- |
| `-addr`             | `:9080`     | Listen address                                                                              |
| `-max-conns`        | `1000`      | Max concurrent WebSocket connections                                                        |
| `-max-conns-per-ip` | `16`        | Max concurrent connections per client IP (IPv6: per /64)                                    |
| `-max-lobbies`      | `500`       | Max concurrent lobbies                                                                      |
| `-real-ip-header`   | `CF-Connecting-IP,X-Real-IP` | Comma-separated client-IP headers set by reverse proxies, first valid one wins; only trusted from loopback/private addresses |
| `-allowed-origins`  | `https://ssebs.github.io,https://danknooner.ssebs.com` | Comma-separated browser origins allowed to connect (the web build). Native Godot clients send no `Origin` and are always allowed |

The defaults fit the production path (Cloudflare → Nginx Proxy Manager → container): Cloudflare puts the player's IP in `CF-Connecting-IP`, while NPM's `X-Real-IP` only holds the Cloudflare edge IP. Without Cloudflare, NPM's `X-Real-IP` is used. If NPM isn't behind Cloudflare, clients can set `CF-Connecting-IP` themselves to dodge the per-IP limit; pass `-real-ip-header X-Real-IP` in that case.

### Testing

```bash
go test -race ./...
```

Unit tests cover the lobby logic, limits and helpers; integration tests run a real server and drive it the way `multiplayer_webrtc.gd` does. CI runs them before building the Docker image.

### Publishing

```bash
./deploy-version.sh
```

## Integration with DankNooner

The Godot client uses `MultiplayerWebRTC` (`managers/network/multiplayer_webrtc.gd`).

Key export vars on that node:

| Export          | Default                    | Description         |
| --------------- | -------------------------- | ------------------- |
| `signaling_url` | `wss://signal.ssebs.com`   | URL of this server  |
| `stun_server`   | `stun:stun.ssebs.com:3478` | STUN server for ICE |
| `turn_server`   | `turn:stun.ssebs.com:3478` | TURN relay fallback |

Both `signaling_url` and the STUN/TURN host can be overridden at runtime via `SettingsManager` (`signal_relay_host` key).

**Host flow:** `MultiplayerWebRTC.start_server()` → waits for `ID` message → `WebRTCMultiplayerPeer.create_server()` → returns lobby code via `get_addr()`

**Client flow:** `MultiplayerWebRTC.connect_client(lobby_code)` → WebRTC handshake relayed through server → `connection_succeeded` emitted

## Message Protocol

JSON over WebSocket: `{"type": <int>, "id": <int>, "data": "<string>"}`

| Type            | Value | Direction | Description                                           |
| --------------- | ----- | --------- | ----------------------------------------------------- |
| JOIN            | 0     | C→S / S→C | Create or join lobby; `data` = code (empty to create) |
| ID              | 1     | S→C       | Server assigns peer ID                                |
| PEER_CONNECT    | 2     | S→C       | A new peer joined                                     |
| PEER_DISCONNECT | 3     | S→C       | A peer left                                           |
| OFFER           | 4     | relay     | WebRTC SDP offer                                      |
| ANSWER          | 5     | relay     | WebRTC SDP answer                                     |
| CANDIDATE       | 6     | relay     | ICE candidate (`mid\nindex\nsdp`)                     |
| SEAL            | 7     | C→S / S→C | Close lobby to new joiners                            |

## Dependencies

- Go 1.24+
- [gorilla/websocket](https://github.com/gorilla/websocket)

## License

[AGPL](./LICENSE)
