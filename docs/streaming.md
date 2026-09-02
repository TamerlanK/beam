# Streaming, flow control, and the memory model

The invariant everything below serves: **server memory stays flat no matter
how large or how slow a transfer is.** Relaying a 50GB file must cost the
same RAM as relaying 5MB.

## Chunking

The sending browser never loads the file: it reads 64KB slices lazily with
`file.slice(offset, end).arrayBuffer()` and sends each as one binary frame
`[16-byte transfer UUID][chunk]`. 64KB is small enough that a frame is an
instant, droppable unit of work, and large enough that per-frame overhead
(websocket framing, one channel hop, one syscall) stays negligible.

On the server, a chunk's whole life happens inside recycled buffers:

1. `readPump` takes a 64KB+16 buffer from a `sync.Pool` and reads the frame
   into it — the frame is *rejected* if it exceeds the buffer.
2. The buffer travels by reference: readPump → hub (ownership check,
   accounting) → receiver's send queue → `writePump` writes it to the
   receiver's socket.
3. `writePump` returns the buffer to the pool. Every early-exit path
   (validation failure, dead receiver, full queue) returns it instead.

Steady state allocates nothing per chunk: the pool holds roughly
(in-flight chunks across all transfers) buffers, bounded by the credit
windows. There is no per-transfer buffer, no file assembly, and nothing
ever touches disk.

## Credit-based flow control

The end-to-end problem: a fast sender and a slow receiver, with a server in
between that must not become the buffer.

- A sender may have at most **16 unacked chunks** (1MB) in flight per
  transfer. It starts with 16 credits on acceptance, spends one per chunk,
  and stops when it hits zero.
- The server grants `flow-credit {n:1}` only after the receiver's
  **socket write completes** — `writePump` reports each finished chunk
  write back to the hub, which converts it into a credit for the sender.
  Credits therefore measure what the receiver actually consumed, not what
  the server accepted.
- The final chunk's write-completion becomes `transfer-complete` instead
  of a credit.

Why 16: the window must cover the bandwidth-delay product of the
sender→server→receiver path to keep the pipe full. 1MB in flight saturates
a gigabit link up to ~8ms RTT and typical WAN links far beyond that, while
capping worst-case per-transfer server memory at 1MB.

## Backpressure failure policy

A receiver that stops draining must fail *its own transfer*, promptly and
cleanly — never grow memory, never stall the hub, never affect other pairs.
Three mechanisms compose:

1. **Bounded queues.** Each client's outbound queue holds 64 messages. The
   hub only ever does non-blocking sends, so the hub goroutine can never be
   blocked by any client.
2. **Write deadline.** `writePump` gives every socket write 10 seconds. A
   receiver that stays blocked past that deadline gets its connection
   closed, which fails its transfers with `peer-disconnected`. This is the
   "blocked past a deadline" guarantee: 10s of zero progress ends the
   transfer.
3. **Queue overflow fails the transfer.** Credits bound chunks in flight
   per transfer, so a receiver's queue can only fill if it genuinely
   stopped draining while several transfers converge on it. If the hub
   cannot queue a chunk, it fails that transfer with
   `receiver-backpressure` (and symmetrically, a sender too backed up to
   accept a flow credit fails with `sender-backpressure` — a lost credit
   would wedge the transfer forever).

Dropped *broadcast* control messages (peer-joined etc.) on a full queue are
logged and tolerated: a client 64 messages behind is effectively dead and
the ping/pong deadline reaps it within a minute.

## Server memory model

| consumer | bound |
|---|---|
| chunk buffers (`sync.Pool`) | ≈ in-flight chunks × 64KB, capped by credit windows (16/transfer) |
| per-client queues | 64 slots of headers; chunk payloads counted above |
| hub state | O(clients + rooms + live transfers) — a few hundred bytes each |
| websocket buffers | 2 × 4KB per client |

200 idle clients ≈ a few MB. 25 concurrent transfers ≈ 25MB of pooled
buffers worst-case. Nothing scales with *file size* — only with
*concurrency*. `make loadtest` measures this empirically.

## The receiver-side Blob tradeoff

The receiving browser accumulates chunks in memory and assembles a `Blob`
on completion, then triggers a download. This is a deliberate tradeoff:

- **Why:** it's the only zero-install, zero-permission path. The
  alternatives — the File System Access API (Chromium-only, permission
  prompt), a service-worker streaming shim (fragile across browsers), or
  OPFS staging (still bounded by quota) — all break "open a link and it
  works".
- **Cost:** a received file must fit comfortably in the receiving device's
  RAM. Multi-GB works on desktops; it is the practical ceiling on phones.
  Note the asymmetry: the *sender* streams with `file.slice()` and can send
  arbitrarily large files; the limit is receiver-side only, and the server
  is indifferent either way.
- **Exit path:** the protocol already streams; swapping the receiver's
  sink for a File System Access writer or service-worker stream requires
  no server or protocol change.

## Receiver sinks (2026-09-02)

The receiver now picks a sink when the user accepts:

- **File System Access** (`showSaveFilePicker`, Chromium): every decrypted
  chunk goes straight to the file's writable stream; memory stays flat
  regardless of file size.
- **In-memory Blob** (everything else, or when the picker is dismissed):
  the previous behaviour, with the RAM ceiling described above.

The sink is opened *before* the answer is sent, so no chunks are queued
while the picker is open.
