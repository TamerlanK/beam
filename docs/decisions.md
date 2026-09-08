# Design decisions

ADR-style log. Each entry: context → decision → consequences.

---

## 2026-09-08 — A terminal client in the same binary

**Context.** Every beam device was a browser. The machine that most often
has a file to hand off is the one without a screen: a build server, a
Raspberry Pi, a CI runner, an SSH session. Wormhole-style tools cover
that but need an install on both ends; beam's relay only needs one.

**Decision.** `beam ls`, `beam send` and `beam recv` live in the server
binary and speak the wire protocol from Go (`internal/client`): the same
offer/answer, the same commit-then-reveal ECDH, the same AES-GCM chunk
sealing with the chunk index as nonce, so a browser on the other side
shows a matching verification code and needs nothing new. The client
reconnects with backoff and resumes by offset in both directions while
the process runs. Each run is its own device, with a fresh id and the
saved name, so a `send` in one shell cannot replace a `recv` in another.
It never negotiates WebRTC: a peer that ignores signaling is relayed,
which is what the browser falls back to anyway. A receiver without
crypto (a browser on plain http) is served in the clear with a warning,
as the browser itself does.

**Consequences.** Headless boxes and scripts join the radar with no new
protocol and no new dependency (`crypto/ecdh`, `crypto/aes`), at the
cost of one more subcommand parser in `cmd/beam`. A vector pinned in Go
and checked under Node's WebCrypto keeps the two implementations from
drifting. Not done: drops, previews, resume across a process restart
(the browser keeps keys in IndexedDB, the terminal keeps them in
memory) and a `terminal` device type, so the radar shows the client as
a laptop.

---

## 2026-09-08 — Commit to the sender's key before revealing it

**Context.** The verification code is 20 bits of a hash over both
ephemeral public keys. The offer carried the sender's key in the clear, so
a malicious relay could forward its own key to the receiver, wait for the
receiver's key, and then grind roughly a million P-256 keypairs (seconds
on one core) for one whose code on the sender's screen matched the code on
the receiver's. The README's claim that the code answers a hostile relay
did not hold.

**Decision.** ZRTP-style commitment. The offer's `key` becomes the base64
SHA-256 of the sender's public key; the answer carries the receiver's key
as before; the sender then reveals its key in a new `transfer-key` message
before the first chunk, and the receiver rejects the transfer if the hash
does not match. The server forwards the message opaquely and only
enforces who may send it and when (sender, `accepted` state, encrypted
transfer). The receiver queues chunks behind the key derivation and fails
any chunk that arrives before a key on an encrypted transfer.

**Consequences.** A relay must now choose its key towards the receiver
before it knows the sender's, and its key towards the sender before it
knows what the sender will reveal, so a substituted pair matches with
probability 2⁻²⁰, which is what a 4-character code was always meant to
buy. Cost: one extra control message per transfer, no extra round trip
(the reveal rides ahead of the first chunk on the same link). A durable
receive record is first written once the key exists, so a revived receiver
never waits for a key that will not come. Drops are unchanged: the sender
is gone by pickup time, so they still rely on the link form for a hostile
relay.

---

## 2026-09-08 — IPv6 devices group by /64

**Context.** Rooms were keyed by the full client address. IPv4 devices
behind one NAT share it; IPv6 devices on one LAN each have their own
global address, and privacy extensions rotate it, so two laptops on the
same Wi-Fi never saw each other on a dual-stack network.

**Decision.** Public IPv6 addresses are keyed by their /64 prefix, the
standard size of one LAN's delegation; IPv4 stays keyed by address;
private, loopback and link-local addresses of either family share the one
`lan` room as before.

**Consequences.** Dual-stack LANs discover each other again. A /64 is the
common case, not a guarantee: a carrier that hands one /64 to many
customers groups them like carrier-grade NAT already did, which the
explicit-accept rule covers. A phone on IPv6 and a laptop on IPv4 behind
the same router still land in different rooms; the room code joins them.

---

## 2026-09-04 — Durable resume: receive into OPFS, remember handles in IndexedDB

**Context.** Resume only survived a dropped link for two minutes, because
everything lived in tab memory: the receiver's writable stream, the
sender's `File`, the derived AES key. A File System Access writable is no
help, since it commits to the real file only on `close()` and reopening
with `keepExistingData` copies the whole file so far.

**Decision.** The receiver writes into a part file in the origin-private
file system through a sync access handle in a worker, flushing every 4MB,
and records the flushed byte count, key and peer in IndexedDB; on
completion the part is streamed into the file the user picked (Chromium)
or downloaded. The sender records its file-system handle, key and offset
when it has a handle (the picker and drag-drop provide one on Chromium),
saving the offset once a second. On becoming the device's live tab, both
sides revive their records as paused rows; the sender re-offers when the
peer is back, the receiver answers with `min(held, hint)` and rewinds its
sink to it. Records older than a day and abandoned parts are reaped.

**Consequences.** A closed tab, a crashed browser or a sleeping laptop
resumes, and Firefox and Safari now stream to disk too, closing the
in-memory Blob ceiling for every browser with OPFS. The price is a second
write on Chromium (part file, then the chosen file) and a permission click
after a reload when the browser no longer remembers the grant. The wire
protocol is unchanged; the server's existing rule that an answer offset
may not exceed the sender's hint is what makes lazy offset saving safe.

---

## 2026-09-04 — Drops: the server may hold ciphertext, briefly

**Context.** Every transfer needed both browsers open at the same moment.
Phones kill background sockets, people step away, and "send it to my
phone in the other room" fell back to email. Perkoon-style queuing keeps
the sender's tab open; Wormhole-style cloud fallback writes to disk.

**Decision.** A *drop* is a sealed file the hub keeps in memory until the
addressee picks it up or a TTL reaps it. The sender seals chunks exactly
as for a live transfer; the key is agreed against the addressee's
long-lived device public key (published with its presence) or, for a
link, generated at random and carried only in the URL fragment. The hub
stores frames as received and replays them through the same credit-paced
`writePump` path, so a pickup is just a transfer whose sender is the hub.
Bounded by `-drop-max` per drop, 16 per IP, `-drop-budget` overall and
`-drop-ttl`; one pickup, then gone; partial uploads die with their sender.

**Consequences.** The sender can close the tab and the receiver can arrive
late, and the relay still never sees plaintext or touches disk. The
privacy page now has to say the server holds ciphertext for minutes, and
an operator who wants the old posture sets `-drop-max 0`. A relay that
lies about a device's public key could read a device-addressed drop; the
live-transfer verification code does not exist here because the sender is
gone, so the link form (key never reaches the server) is the honest
answer to a hostile relay. Device keys live in IndexedDB as
non-extractable `CryptoKey`s; clearing site data makes older drops to that
device undecryptable, which the receiver sees as an integrity failure.

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

## 2026-09-01 — Receiver assembles a Blob in memory (superseded 2026-09-04: OPFS part files)

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

---

## 2026-09-02 — One tab owns the device (Web Locks leader election)

**Context.** Identity lives in localStorage, which every tab of an origin
shares. Two tabs connected as two "devices" with the same name; the
server de-duplicated the id but not the confusion.

**Decision.** Client-side election, no server change. The first tab takes
a Web Lock and connects; other tabs show a gate and queue a blocking
request for the same lock. "Use this tab instead" broadcasts a takeover:
an idle leader closes its socket cleanly and releases, a busy leader
answers `busy` and keeps the lock, and a leader that says nothing for
1.5s (frozen, discarded) is stolen with `steal: true`, which rejects its
request promise and drops it behind the gate if it ever wakes.

**Consequences.** A browser is exactly one presence; the persistent name
stays meaningful. Closing the leader hands the connection to the next tab
automatically. Server-side "kick the older connection" was rejected
because both tabs auto-reconnect and would fight. Web Locks needs a
secure context, so plain-HTTP LAN deployments fall back to the old
tab-per-device behaviour; production is behind TLS anyway.

---

## 2026-09-03 — Per-IP connection cap and a global relay budget

**Context.** A public instance has two cheap abuse paths: one address
opening thousands of sockets, and a handful of transfers saturating the
operator's uplink. Neither needed a database or an external limiter.

**Decision.** Two hub-level limits, both off when zero. `-max-conns-per-ip`
(default 32) counts live sockets per IP in `addClient` and rejects the
overflow with `too-many-connections` before it joins a room; the client
stops reconnecting on that code. `-relay-bps` is a single token bucket on
the hub, refilled by the existing one-second ticker and charged in
`handleWritten` before a flow credit is granted. When the bucket is empty
the transfer is queued as starved and its credit is released on a later
tick.

**Consequences.** No frame is ever dropped: credits already bound how many
bytes a sender may have in flight, so throttling only delays the next
credit. The cost is granularity — a throttled transfer moves in one-second
bursts; a 100ms ticker is the upgrade if smoothness matters. The budget is
global, not per IP, so one heavy sender slows everyone equally; a per-IP
bucket is the next step if fairness becomes a problem. The cap keys on the
same IP as room grouping, so behind a proxy it needs `-trust-proxy`.

---

## 2026-09-03 — Resume by receiver-reported offset, no server state

**Context.** A Wi-Fi blip or a closed WebRTC data channel failed the whole
transfer, and a 4GB file had to start over. Up to 16 chunks can be in
flight when a link drops, so the sender's own offset overstates what
actually landed.

**Decision.** The receiver's committed byte count is the truth. On
connection loss both sides pause instead of failing: the receiver keeps
its File System Access writable open (aborting it discards the temp file),
the sender keeps the file handle, and both keep the derived key. When the
peer is back, the sender re-offers with the same transfer ID, the same
key, and its offset as a hint; the receiver recognises the ID, peer, name,
and size, auto-accepts without a prompt, and answers with its own offset.
The server validates both offsets (chunk-aligned, below size, answer at
most the hint) and starts its relayed and written counters at the wire
equivalent, so completion logic is untouched. Chunk alignment keeps the
AES-GCM sequence number equal to `offset / 64KB`, so a resumed stream never
reuses a nonce with different plaintext. A lost direct link resumes over
the relay without user action. Nothing is persisted anywhere.

To make this work after a silent drop, a device that reconnects with its
own id now **replaces** the ghost connection (which is told `replaced` and
closed) instead of being handed a fresh id. This reverses part of the Web
Locks ADR: two tabs no longer fight because the replaced tab stops
reconnecting and goes idle. A room peer who learns your id could knock you
offline this way; accepted for a LAN drop tool, since it needs your v4
UUID and cannot read encrypted chunks or guess transfer ids.

**Consequences.** Resume works only while both tabs stay open, since
neither the server nor disk holds partial state; a reload or a two-minute
gap fails the transfer for real. Memory-backed sinks (Firefox, Safari)
resume as well because the parts array survives the pause. The re-offer
is retried at most every 30 seconds until the two-minute timer expires.
