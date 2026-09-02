# Design decisions

ADR-style log. Each entry: context → decision → consequences.

---

## 2026-09-01 — Server relay over WebRTC P2P

**Context.** Browser-to-browser file transfer can go peer-to-peer (WebRTC
data channels) or through a relay.

**Decision.** Relay everything through the server over WebSockets.

**Consequences.** Works through every NAT/firewall combination with no
STUN/TURN infrastructure, no ICE failures, and a protocol simple enough to
validate exhaustively. The cost is server bandwidth — every byte crosses
the relay. WebRTC with the relay as fallback is the highest-value future
work; the signaling channel it needs already exists.

---

## 2026-09-01 — One hub goroutine owns all state; channels only

**Context.** Rooms, clients, and transfers are shared mutable state
touched by hundreds of connections.

**Decision.** A single hub goroutine owns every map; pumps communicate
with it exclusively via channels; the hub never blocks (non-blocking sends
to clients), and every pump→hub send pairs with a shutdown escape channel.

**Consequences.** No mutexes, no lock ordering, nothing for the race
detector to find; the concurrency invariants live in ~5 lines of comments
instead of everyone's heads. The hub is a serialization point, but it does
only map lookups and channel handoffs — the loadtest shows hundreds of
MB/s through it. Sharding hubs is possible later; nothing in the protocol
assumes one.

---

## 2026-09-01 — Credits granted on receiver *write completion*

**Context.** Flow control could track what the server accepted, or what
the receiver actually consumed.

**Decision.** `writePump` reports each completed chunk write to the hub;
only then does the sender get a credit. Window: 16 chunks (1MB).

**Consequences.** The sender's window measures true end-to-end
consumption, so a slow receiver throttles its sender with zero server
buffering beyond the in-flight window. The final write doubles as the
completion signal, so `transfer-complete` means "the receiver's socket got
every byte", not "the server saw every byte". Cost: one extra channel hop
per chunk — invisible next to the socket writes.

---

## 2026-09-01 — Rooms keyed by public IP; XFF behind a flag

**Context.** "Devices on the same network see each other automatically"
needs a grouping signal available with zero configuration.

**Decision.** Key rooms by the TCP peer IP; honor `X-Forwarded-For` only
with `-trust-proxy`.

**Consequences.** Same-NAT devices pair instantly. Two sharp edges,
accepted and mitigated: carrier-grade NAT can group strangers (why every
transfer requires an explicit accept and identities are ephemeral), and
XFF is attacker-controlled unless a trusted proxy sets it (why it is
opt-in — otherwise anyone could join any LAN's room by spoofing a header).

---

## 2026-09-01 — Room codes alias the creator's room

**Context.** A cross-network code could create a fresh private room or
join the visitor into the creator's existing room.

**Decision.** Alias: `room-join` moves the joiner into the creator's
current room.

**Consequences.** Matches user intent ("connect my phone to what I see
here") — the joiner sees the creator's LAN peers too. One room concept
instead of two; codes are just a second index onto rooms. A visitor is
visible to the whole room, which the explicit-accept rule already covers.

---

## 2026-09-01 — Sender-generated transfer UUIDs

**Context.** Binary frames need a transfer ID; someone must mint it.

**Decision.** The sending browser generates the UUID
(`crypto.randomUUID`) and puts it in the offer; the server validates
format and uniqueness.

**Consequences.** No ID-assignment round trip — the sender can stream the
instant the acceptance arrives. A hostile client can only hurt itself:
collisions are rejected, and chunk ownership is checked against the
transfer's sender on every frame.

---

## 2026-09-01 — Receiver assembles a Blob in memory

**Context.** Browsers cannot stream to disk without permissions
(File System Access) or fragile service-worker shims.

**Decision.** Accumulate chunks in memory, assemble a `Blob`, trigger a
normal download. Document the ceiling.

**Consequences.** Zero-install receiving everywhere; received files must
fit in the receiver's RAM (sender side streams and has no such limit).
The protocol is already chunked, so swapping the sink later requires no
server change. Full analysis in [streaming.md](streaming.md).

---

## 2026-09-01 — 4-char codes from a 32-char alphabet + rate limiting

**Context.** Room codes must be typable across the room ("X K 7 P") yet
not guessable.

**Decision.** 4 chars from `23456789ABCDEFGHJKLMNPQRSTUVWXYZ` (no 0/O/1/I;
exactly 32 chars so a masked crypto/rand byte is bias-free), ~1M
combinations, 10-minute idle TTL, per-IP token bucket on joins (burst 5,
one per 2s).

**Consequences.** Codes survive being read aloud or retyped from a photo.
At the limited guess rate, brute-forcing the space takes on the order of a
month per IP against a 10-minute TTL. Longer codes were rejected as a UX
tax that buys security the rate limit already provides.

---

## 2026-09-01 — Vanilla JS frontend, embedded, no build step

**Context.** The deliverable is one static binary; the UI needs drag &
drop, progress rings, modals, QR codes.

**Decision.** Hand-written HTML/CSS/JS in `web/static`, embedded with
`embed.FS`. One vendored dependency (qrcode-generator, MIT, ~20KB). System
font stack — no webfonts, because beam must work on a LAN with no internet.

**Consequences.** `go build` is the entire pipeline; the binary is the
deployment. No framework runtime, no supply chain, instant load. The cost
is hand-rolled DOM code — acceptable at this app's size (~700 lines).

---

## 2026-09-01 — No TLS in-process

**Context.** Browsers require HTTPS/WSS for clipboard APIs and generally
for non-localhost use.

**Decision.** beam speaks plain HTTP/WS; production runs behind
Caddy/nginx for TLS termination.

**Consequences.** No certificate management in-process; the reverse proxy
also supplies `X-Forwarded-For`, which `-trust-proxy` is designed for.
Documented in the README quickstart.

---

## 2026-09-02 — WebRTC data channel as a second pipe, not a second protocol

**Context.** P2P would cut relay bandwidth and lift LAN transfers to
link speed, but WebRTC adds ICE failure modes the relay never had.

**Decision.** Browsers eagerly negotiate one data channel per peer pair
(perfect-negotiation pattern, STUN only) via an opaque `rtc` signaling
message the server forwards blindly. A transfer picks its pipe at offer
time: the open channel if there is one, otherwise the socket. The same
JSON envelopes and binary frames flow over either; over the channel the
receiver emits `transfer-complete` and the sender enforces the offer
timeout, since no server is watching.

**Consequences.** Zero server changes to the transfer state machine; a
P2P transfer is invisible to the server beyond signaling. Symmetric NAT
pairs simply stay on the relay. No TURN: it would be a second relay with
worse accounting than the one we already have.

---

## 2026-09-02 — End-to-end encryption via per-transfer ECDH on the offer

**Context.** The relay never stored bytes, but it could read them.

**Decision.** Each offer carries an ephemeral P-256 public key; the answer
carries the receiver's. AES-256-GCM per chunk, IV = chunk index, AAD =
transfer id. The server treats keys as opaque strings and accounts for the
16-byte tag per chunk (`WireSize`). Both sides display a 4-character code
hashed from both public keys.

**Consequences.** No identity keys, no pairing step, forward secrecy per
file. The relay can still substitute keys — the verification code is the
mitigation, and it is the user's to check. Insecure origins (plain HTTP on
a LAN IP) have no `crypto.subtle`, so encryption is negotiated per
transfer and a plaintext fallback stays possible; the lock badge is only
shown when it actually happened.

---

## 2026-09-02 — Stream to disk with File System Access, Blob elsewhere

**Context.** The in-memory Blob capped received files at the device's
RAM; phones fail well under 2GB.

**Decision.** On accept, call `showSaveFilePicker` inside the click's
user activation and pipe decrypted chunks straight into the writable.
Any failure (unsupported browser, picker dismissed) falls back to the
Blob path. The answer is sent only after the sink exists, so nothing is
buffered while the picker is open.

**Consequences.** Chromium receives files of any size in flat memory.
Firefox and Safari keep the old ceiling. A user who dawdles in the picker
past the 30s offer timeout sees the offer withdrawn, not a half-written
file.

