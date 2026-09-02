import { state, on } from "./state.js";
import * as transfers from "./transfers.js";

const ICE = [{ urls: "stun:stun.l.google.com:19302" }];
const HIGH = 4 << 20;
const LOW = 1 << 20;
const MIN_MESSAGE = 16 + 64 * 1024 + 16;
const links = new Map();
let socket;

export function initRTC(s) {
  socket = s;
  on("peers", sync);
}

export function reset() {
  for (const id of [...links.keys()]) drop(id);
}

export function linkFor(peerId) {
  const l = links.get(peerId);
  return l && l.dc && l.dc.readyState === "open" && l.big ? l : null;
}

function sync() {
  for (const id of [...links.keys()]) if (!state.peers.has(id)) drop(id);
  if (!state.self || typeof RTCPeerConnection === "undefined") return;
  for (const id of state.peers.keys()) ensure(id);
}

function drop(id) {
  const l = links.get(id);
  if (!l) return;
  links.delete(id);
  transfers.dropLink(l, "direct connection lost");
  try { l.pc.close(); } catch {}
}

function signal(peerId, payload) {
  socket.send("rtc", { to: peerId, signal: payload });
}

function ensure(peerId) {
  if (links.has(peerId)) return links.get(peerId);
  const pc = new RTCPeerConnection({ iceServers: ICE });
  const l = {
    p2p: true, peerId, pc, dc: null, big: false,
    polite: state.self.id < peerId, makingOffer: false, ignoreOffer: false,
    send(type, data) { this.dc.send(JSON.stringify({ v: 1, type, data })); },
    sendRaw(buf) { this.dc.send(buf); },
    ready() { return this.dc && this.dc.readyState === "open" && this.dc.bufferedAmount < HIGH; },
    sent() {},
  };
  links.set(peerId, l);
  pc.onnegotiationneeded = async () => {
    try {
      l.makingOffer = true;
      await pc.setLocalDescription();
      signal(peerId, { description: pc.localDescription });
    } catch {} finally {
      l.makingOffer = false;
    }
  };
  pc.onicecandidate = ({ candidate }) => { if (candidate) signal(peerId, { candidate }); };
  pc.ondatachannel = ({ channel }) => attach(l, channel);
  pc.onconnectionstatechange = () => {
    if (pc.connectionState === "failed" || pc.connectionState === "closed") drop(peerId);
  };
  if (!l.polite) attach(l, pc.createDataChannel("beam"));
  return l;
}

function attach(l, dc) {
  l.dc = dc;
  dc.binaryType = "arraybuffer";
  dc.bufferedAmountLowThreshold = LOW;
  dc.onopen = () => {
    const max = l.pc.sctp && l.pc.sctp.maxMessageSize;
    l.big = !max || max >= MIN_MESSAGE;
  };
  dc.onbufferedamountlow = () => transfers.resume(l);
  dc.onclose = () => drop(l.peerId);
  dc.onmessage = (ev) => {
    if (typeof ev.data !== "string") return transfers.onChunk(ev.data, l);
    let env;
    try { env = JSON.parse(ev.data); } catch { return; }
    if (env.v !== 1 || !env.type) return;
    const d = env.data || {};
    if (env.type === "transfer-offer") d.from = state.peers.get(l.peerId);
    const h = transfers.handlers[env.type];
    if (h) h(d, l);
  };
}

export async function onSignal(d) {
  if (!state.self || !state.peers.has(d.from) || typeof RTCPeerConnection === "undefined") return;
  const l = ensure(d.from);
  const { pc } = l;
  const s = d.signal || {};
  try {
    if (s.description) {
      const collision = s.description.type === "offer" && (l.makingOffer || pc.signalingState !== "stable");
      l.ignoreOffer = !l.polite && collision;
      if (l.ignoreOffer) return;
      await pc.setRemoteDescription(s.description);
      if (s.description.type === "offer") {
        await pc.setLocalDescription();
        signal(d.from, { description: pc.localDescription });
      }
    } else if (s.candidate) {
      try { await pc.addIceCandidate(s.candidate); } catch (e) { if (!l.ignoreOffer) throw e; }
    }
  } catch {}
}
