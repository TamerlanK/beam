# beam wire protocol (v1)

Everything travels over one WebSocket per client (`/ws`).

- **Control messages** are JSON text frames wrapped in a versioned envelope:

  ```json
  {"v": 1, "type": "message-type", "data": { ... }}
  ```

  Unknown versions, missing types, or malformed payloads are answered with an
  `error` message. The read limit for a control frame is 1MB.

- **File data** travels as binary frames:

  ```
  ┌────────────────────┬─────────────────────────┐
  │ transfer UUID      │ chunk payload           │
  │ 16 bytes, raw      │ 1 – 65552 bytes         │
  └────────────────────┴─────────────────────────┘
  ```

  Frames outside 17..65568 bytes are rejected. An end-to-end encrypted
  transfer carries a 16-byte AES-GCM tag per chunk, so its chunks are up to
  65552 bytes and its wire size is `size + 16 * ceil(size / 65536)`. Frames for unknown transfer
  IDs are dropped silently (a credit window's worth of chunks can legally
  trail a cancel or failure).

Directions below: **C→S** = client to server, **S→C** = server to client.

## Identity

A device presents its persisted identity as query parameters on the
`/ws` connect URL:

```
/ws?id=<uuid v4>&name=Purple%20Falcon&emoji=%F0%9F%A6%85
```

`id` is a stable per-browser UUID. It is a claim, not a proof: the server
only guarantees it is unique among *connected* clients. If the id is
already connected, the older connection receives `error` code `replaced`
and is closed, so a device that reconnects after a dropped link keeps its
identity and its peers can resume transfers to it. Missing or invalid fields get a random name/emoji. Names are
control-stripped, whitespace-collapsed and capped at 32 runes; emoji must be
a short non-ASCII glyph (≤32 bytes, ZWJ sequences and flags allowed).

### `profile` (C→S)

```json
{"v":1,"type":"profile","data":{"name":"Bob the Builder","emoji":"🦊"}}
```

Renames the client. Invalid values return `error` code `bad-message` and
leave the profile untouched.

### `peer-updated` (S→C)

Broadcast to everyone in the room, the renamed client included. Payload is
a peer object.

## Rooms & presence

### `room-state` (S→C)

Sent when a client connects or joins a room by code. Complete snapshot.

```json
{"v":1,"type":"room-state","data":{
  "self":  {"id":"4c9f…","name":"Purple Falcon","emoji":"🦅","device":"laptop"},
  "peers": [{"id":"a1b2…","name":"Amber Otter","emoji":"🦦","device":"phone"}]
}}
```

`device` is one of `phone`, `tablet`, `laptop` (User-Agent heuristic).

### `peer-joined` (S→C)

A peer entered the room. Payload is a single peer object (same shape as `self` above).

### `peer-left` (S→C)

```json
{"v":1,"type":"peer-left","data":{"id":"a1b2…"}}
```

### `room-create` (C→S)

Request a share code for the client's **current** room (`data` empty/omitted).
The code aliases the existing room, so a cross-network joiner sees the
creator's LAN peers too. Repeated requests reuse a live code and refresh its TTL.

### `room-created` (S→C)

```json
{"v":1,"type":"room-created","data":{"code":"XK7P","expiresIn":600}}
```

Codes are 4 chars from `23456789ABCDEFGHJKLMNPQRSTUVWXYZ` (no 0/O/1/I).
`expiresIn` is the idle TTL in seconds; joins reset it.

### `room-join` (C→S)

```json
{"v":1,"type":"room-join","data":{"code":"XK7P"}}
```

Moves the client out of its current room (its transfers fail with
`peer-left-room`) into the code's room. Rate-limited per IP (burst 5,
refill 1 per 2s) — exceeding it returns `error` code `rate-limited`;
unknown/expired codes return `bad-code`.

## Transfers

Lifecycle: `Offered → Accepted → Active → Completed`, with `Declined`,
`Canceled`, `Failed` as the other terminal states. Illegal transitions
(wrong client answering, double accept, cancel after complete, …) return an
`error` with code `bad-transfer` and never crash the server.

### `transfer-offer` (C→S, then S→C)

Sender → server. The **sender generates the UUID** so it can start
streaming immediately after acceptance without an ID round trip:

```json
{"v":1,"type":"transfer-offer","data":{
  "id":"7f3a1e90-6f0e-4d0a-9d1c-2f6b8a91c4e2",
  "to":"a1b2…","name":"holiday.mp4","size":734003200,"mime":"video/mp4",
  "key":"BGx1…"
}}
```

Validation: UUID well-formed and unused; `to` a distinct peer in the same
room; `name` sanitized (path components and control chars stripped) and
≤255 bytes; `0 < size ≤ 50GB`; `key` ≤128 chars; at most 32 live transfers
per sender.

`key` is optional: the sender's ephemeral ECDH P-256 public key (raw,
base64). The server forwards it opaquely and never sees a private key.

`preview` is optional: a `data:image/{jpeg,png,webp};base64,` thumbnail of
at most 40KB (first page for PDFs), shown in the receiver's accept prompt.
Anything else is rejected with `bad-message`.

`offset` is optional and used to resume: the plaintext byte position the
sender proposes to continue from. It must be a multiple of the chunk size
(64KB) and below `size`, else `bad-message`. It is only a hint; the
receiver's answer decides. Chunk alignment keeps the AES-GCM sequence
number equal to `offset / 64KB`, so a resumed stream never reuses a nonce
with different plaintext.

Server → receiver (note `from` added, `to` scrubbed):

```json
{"v":1,"type":"transfer-offer","data":{
  "id":"7f3a…","from":{"id":"4c9f…","name":"Purple Falcon","emoji":"🦅","device":"laptop"},
  "name":"holiday.mp4","size":734003200,"mime":"video/mp4"
}}
```

Unanswered offers fail after 30s with reason `offer-timeout`.

### `transfer-answer` (C→S, forwarded S→C)

```json
{"v":1,"type":"transfer-answer","data":{"id":"7f3a…","accept":true,"key":"BJk2…"}}
```

Only the offer's target may answer. On accept the sender may start
streaming with an initial window of **16 chunks** (see
[streaming.md](streaming.md)).

`offset` is optional: the receiver's committed plaintext byte count, the
position streaming resumes from. It must be chunk-aligned, below `size`,
and at most the offer's `offset` hint, else `bad-message`. The receiver is
the source of truth because up to 16 chunks can be in flight when a link
drops. The server starts its relayed and written counters at this offset
(its wire equivalent for encrypted transfers), so completion accounting is
unchanged. Omitted means zero, a fresh transfer.

`key` is the receiver's ephemeral public key, ≤128 chars. The server strips
it if the offer carried no key. When both sides supplied a key the transfer
is end-to-end encrypted: each chunk is AES-256-GCM sealed with a key derived
by ECDH, IV = 96-bit big-endian chunk index, AAD = the 16-byte transfer id,
and the server accounts for the 16-byte tag per chunk. Both browsers show
the same 4-character verification code derived from both public keys; a
relay that substitutes keys produces mismatched codes.

### `flow-credit` (S→C, sender only)

```json
{"v":1,"type":"flow-credit","data":{"id":"7f3a…","n":1}}
```

Grants the sender permission for `n` more in-flight chunks. Emitted only
after the receiver's socket write for a previous chunk **completed**.

### `transfer-cancel` (C→S, forwarded S→C)

```json
{"v":1,"type":"transfer-cancel","data":{"id":"7f3a…","reason":"changed my mind"}}
```

Either party, any non-terminal state. The other party receives the
forwarded cancel.

### `transfer-complete` (S→C, both parties)

```json
{"v":1,"type":"transfer-complete","data":{"id":"7f3a…","bytes":734003200}}
```

Emitted when the final byte has been flushed to the receiver's socket.

### `transfer-failed` (S→C, both parties)

```json
{"v":1,"type":"transfer-failed","data":{"id":"7f3a…","reason":"peer-disconnected"}}
```

Reasons: `offer-timeout`, `peer-disconnected`, `peer-left-room`,
`receiver-backpressure`, `sender-backpressure`, `server-shutdown`, or a
protocol-violation description.

## WebRTC signaling

### `rtc` (C→S, forwarded S→C)

```json
{"v":1,"type":"rtc","data":{"to":"a1b2…","signal":{"description":{"type":"offer","sdp":"…"}}}}
```

`signal` is an opaque JSON value (1..16384 bytes) forwarded verbatim to a
peer in the same room, with `from` set to the sender's id and `to`
scrubbed. Browsers use it to negotiate a direct data channel; once one is
open, offers, answers, chunks, cancels and completions for that pair travel
over the channel using the same envelopes and frames, and the server sees
nothing but the signaling. Transfers fall back to the relay whenever no
channel is open.

## Drops

A drop is a sealed file the server holds in memory until it is picked up,
for at most `-drop-ttl` (default 10 min). It is addressed either to one
device (`to`, the persisted device id) or to anyone holding the link
(`to` omitted). Wire size is always `size + 16 * ceil(size / 64KB)`: the
client seals every chunk, and the server assumes so. The server holds
frames exactly as received and replays them; it never has a key.

`room-state` advertises `"drops": {"maxBytes": N, "ttl": S}` when drops are
enabled. A device that can receive drops publishes a long-lived ECDH
public key as `pub` on its `/ws` URL; peers see it on the peer object.

### `drop-create` (C→S)

```json
{"v":1,"type":"drop-create","data":{
  "id":"7f3a…","to":"a1b2…","name":"report.pdf","size":4194304,"mime":"application/pdf",
  "key":"BGx1…","preview":"data:image/jpeg;base64,…"
}}
```

Same validation as an offer, plus: `0 < size ≤ maxBytes`, at most 16
drops per IP, and the global `-drop-budget`. `key` is the creator's
ephemeral public key for a device drop (the addressee derives the AES key
from it and its own private key); a link drop carries no key at all.
Errors use code `bad-drop`. Reply:

### `drop-created` (S→C)

```json
{"v":1,"type":"drop-created","data":{"id":"7f3a…","expiresIn":600}}
```

The creator then streams binary frames under the drop id with the usual
16-chunk window; the server answers each stored chunk with `flow-credit`
(subject to the relay budget). Overflowing the declared size deletes the
drop with an error. If the creator disconnects before the last chunk the
drop is deleted.

### `drop-stored` (S→C)

```json
{"v":1,"type":"drop-stored","data":{"id":"7f3a…","expiresIn":600}}
```

The drop is held; the creator may leave. The TTL restarts here.

### `drop-waiting` (S→C)

```json
{"v":1,"type":"drop-waiting","data":{
  "id":"7f3a…","from":{"id":"4c9f…","name":"Purple Falcon","emoji":"🦅","device":"laptop"},
  "name":"report.pdf","size":4194304,"mime":"application/pdf","key":"BGx1…","expiresIn":583,"anyone":false
}}
```

Sent to the addressee when the upload completes, again every time the
addressee connects while the drop is held, and after a failed pickup.
`anyone: true` marks a link drop (sent only in reply to `drop-claim`).

### `drop-claim` (C→S)

```json
{"v":1,"type":"drop-claim","data":{"id":"7f3a…"}}
```

Asks for the `drop-waiting` of a link drop (or of a drop addressed to the
caller). Rate-limited with the room-join bucket. Unknown, consumed, or
otherwise-addressed ids return `bad-drop`.

### `drop-accept` (C→S)

```json
{"v":1,"type":"drop-accept","data":{"id":"7f3a…","offset":0}}
```

Starts the pickup: the server pushes stored frames to the caller with the
credit-on-write pacing of a relay and finishes with `transfer-complete`.
Only the addressee (or anyone, for a link drop) may accept; a drop being
picked up by someone else returns `bad-drop`. `offset` resumes a pickup
that broke off (chunk-aligned, below `size`). A pickup that fails leaves
the drop held; a completed one deletes it.

### `drop-cancel` (C→S)

```json
{"v":1,"type":"drop-cancel","data":{"id":"7f3a…"}}
```

Deletes the drop. Allowed for the creator, the addressee (a decline), and
anyone for a link drop. A pickup in progress fails with `canceled`.

### `drop-gone` (S→C)

```json
{"v":1,"type":"drop-gone","data":{"id":"7f3a…","reason":"picked-up"}}
```

Sent to the creator and the addressee when connected. Reasons:
`picked-up`, `expired`, `canceled`, `sender-disconnected`, `protocol violation`.

## Snippets

### `snippet` (C→S, forwarded S→C)

```json
{"v":1,"type":"snippet","data":{"to":"a1b2…","text":"https://example.com/doc"}}
```

Text 1..8192 bytes, target in the same room. Forwarded with `from` (peer
object) and `to` scrubbed.

## Errors

### `error` (S→C)

```json
{"v":1,"type":"error","data":{"code":"bad-code","message":"unknown or expired code"}}
```

| code | meaning |
|---|---|
| `bad-message` | malformed envelope/payload, cap exceeded, unexpected type |
| `bad-code` | unknown or expired room code |
| `rate-limited` | too many room-join attempts from this IP |
| `unknown-peer` | target peer missing or not in your room |
| `bad-transfer` | unknown transfer ID or illegal state transition |
| `too-many-connections` | the per-IP connection cap is reached; the server closes the socket after sending this, so the client must not reconnect |
| `replaced` | the same device id connected again; this older socket is closed and the client must not reconnect |
| `bad-drop` | drops disabled, over a cap, unknown or consumed drop, or not its addressee; the message says which |

## Timing

| what | value |
|---|---|
| server ping interval | 30s |
| client read deadline (pong) | 60s |
| socket write deadline | 10s |
| offer timeout | 30s |
| room-code idle TTL | 10 min |
| drop pickup window | 10 min (`-drop-ttl`), restarted when the upload completes |
