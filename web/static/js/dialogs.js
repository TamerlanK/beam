import { state, on, emit, toast } from "./state.js";
import { $, icon, fmtSize, asURL, copyText } from "./util.js";
import { fileKind } from "./icons.js";
import { answerOffer, OFFER_TTL } from "./transfers.js";

const CODE_RE = /^[2-9A-HJ-NP-Z]{4}$/;
let socket;

export function initDialogs(s) {
  socket = s;
  initOffer();
  initConnect();
  return { openNote: initNotes() };
}

function initOffer() {
  const modal = $("offerModal"), timer = $("offerTimer"), left = $("offerLeft"), queue = $("offerQueue");
  let current = null, tick = null;

  function stop() { clearInterval(tick); tick = null; }

  function show() {
    const d = state.offers[0];
    if (!d) {
      current = null; stop();
      if (modal.open) modal.close("drained");
      return;
    }
    queue.hidden = state.offers.length < 2;
    queue.textContent = `${state.offers.length - 1} more waiting behind this one`;
    if (current === d) return;
    current = d;
    $("offerSender").textContent = d.from ? d.from.name : "Someone";
    $("offerEmoji").textContent = d.from ? d.from.emoji : "📦";
    $("offerName").textContent = d.name;
    $("offerSize").textContent = fmtSize(d.size);
    $("offerIcon").replaceChildren(icon(fileKind(d.name, d.mime || "")));
    if (!modal.open) modal.showModal();
    $("offerAccept").focus();
    stop();
    const paint = () => {
      const ms = Math.max(0, OFFER_TTL - (performance.now() - d.receivedAt));
      timer.style.setProperty("--left", (ms / OFFER_TTL).toFixed(3));
      left.textContent = `${Math.ceil(ms / 1000)}s`;
    };
    paint();
    tick = setInterval(paint, 250);
  }

  on("offers", show);
  $("offerAccept").addEventListener("click", () => answerOffer(true));
  $("offerDecline").addEventListener("click", () => answerOffer(false));
  modal.addEventListener("cancel", (e) => { e.preventDefault(); answerOffer(false); });
}

function initConnect() {
  const modal = $("netModal"), code = $("roomCode"), qr = $("qr"), input = $("joinInput"), pill = $("codePill");
  let joining = null;

  function render() {
    const c = state.code;
    code.classList.toggle("is-loading", !c);
    [...code.children].forEach((b, i) => { b.textContent = c ? c[i] : ""; });
    qr.replaceChildren();
    pill.hidden = !c;
    if (!c) return;
    $("codePillText").textContent = c;
    try {
      const q = qrcode(0, "M");
      q.addData(joinURL(c));
      q.make();
      qr.innerHTML = q.createSvgTag({ cellSize: 4, margin: 0 });
    } catch {
      qr.textContent = joinURL(c);
    }
  }

  function open() {
    render();
    socket.send("room-create", {});
    input.value = "";
    if (!modal.open) modal.showModal();
  }

  on("code", render);
  on("joined", (c) => {
    if (joining !== c) return;
    joining = null;
    if (modal.open) modal.close();
    toast(`Joined with code ${c}`, "ok");
  });
  on("join-failed", () => {
    joining = null;
    input.classList.remove("is-bad");
    void input.offsetWidth;
    input.classList.add("is-bad");
    input.focus();
  });

  for (const b of [$("netBtn"), $("emptyNetBtn"), pill]) b.addEventListener("click", open);
  $("netClose").addEventListener("click", () => modal.close());
  $("copyLink").addEventListener("click", async () => {
    if (!state.code) return;
    await copyText(joinURL(state.code));
    toast("Link copied", "ok");
  });

  input.addEventListener("input", () => {
    let v = input.value.toUpperCase();
    const m = v.match(/#([2-9A-HJ-NP-Z]{4})/);
    if (m) v = m[1];
    input.value = v.replace(/[^2-9A-HJ-NP-Z]/g, "").slice(0, 4);
    input.classList.remove("is-bad");
  });
  $("joinForm").addEventListener("submit", (e) => {
    e.preventDefault();
    const c = input.value.trim().toUpperCase();
    if (!CODE_RE.test(c)) {
      toast("Codes are 4 letters or digits", "bad");
      emit("join-failed");
      return;
    }
    if (c === state.code) { toast("That's your own code", "bad"); return; }
    joining = c;
    emit("joining", c);
    socket.send("room-join", { code: c });
  });
}

const joinURL = (c) => `${location.origin}${location.pathname}#${c}`;

function initNotes() {
  const send = $("snipModal"), text = $("snipText"), count = $("snipCount");
  const recv = $("snipRecvModal"), recvText = $("snipRecvText"), openLink = $("snipOpen");
  let target = null;

  function openSend(p) {
    target = p.id;
    $("snipTo").textContent = p.name;
    text.value = "";
    count.textContent = "0";
    send.showModal();
    text.focus();
  }

  function submit() {
    const body = text.value.trim();
    if (body && target && state.peers.has(target)) {
      socket.send("snippet", { to: target, text: body });
      toast("Note sent", "ok");
    }
    send.close();
  }

  text.addEventListener("input", () => { count.textContent = String(text.value.length); });
  text.addEventListener("keydown", (e) => { if ((e.metaKey || e.ctrlKey) && e.key === "Enter") submit(); });
  $("snipCancel").addEventListener("click", () => send.close());
  $("snipSend").addEventListener("click", submit);

  on("snippet", (d) => {
    $("snipFrom").textContent = d.from ? d.from.name : "someone";
    recvText.textContent = d.text;
    const url = asURL(d.text);
    openLink.hidden = !url;
    if (url) openLink.href = url;
    if (!recv.open) recv.showModal();
  });
  $("snipRecvClose").addEventListener("click", () => recv.close());
  $("snipCopy").addEventListener("click", async () => {
    await copyText(recvText.textContent);
    toast("Copied", "ok");
    recv.close();
  });

  return openSend;
}
