import { state, on, emit, isDone, peerName } from "./state.js";
import { $, el, icon, fmtSize, fmtRate, fmtLeft } from "./util.js";
import { rateOf, etaOf, cancel, saveBlob } from "./transfers.js";

const mobile = matchMedia("(max-width: 640px)");
const rows = new Map();
let panel, toggle, sum, bar, list, collapsed;

export function initPanel() {
  panel = $("panel"); toggle = $("panelToggle"); sum = $("panelSum"); bar = $("panelBar"); list = $("panelList");
  setCollapsed(mobile.matches);
  toggle.addEventListener("click", () => setCollapsed(!collapsed));
  mobile.addEventListener("change", () => setCollapsed(collapsed));

  on("transfer:add", addRow);
  on("transfer:progress", (t) => { updateRow(t); overall(); });
  on("transfer:state", (t) => { updateRow(t); overall(); });
  on("transfer:remove", removeRow);
}

function setCollapsed(v) {
  collapsed = v;
  panel.classList.toggle("is-collapsed", v);
  toggle.setAttribute("aria-expanded", String(!v));
  document.body.classList.toggle("panel-open", !v && mobile.matches);
  overall();
  emit("layout");
}

function addRow(t) {
  const li = $("rowT").content.firstElementChild.cloneNode(true);
  li.querySelector(".tr-icon").append(icon(t.kind));
  li.querySelector(".tr-name").textContent = t.name;
  li.querySelector(".tr-name").title = t.name;
  rows.set(t.id, li);
  list.prepend(li);
  if (panel.hidden) {
    panel.hidden = false;
    document.body.classList.add("has-panel");
    emit("layout");
  }
  updateRow(t);
  overall();
}

function updateRow(t) {
  const li = rows.get(t.id);
  if (!li) return;
  const who = peerName(t.peerId);
  const meta = li.querySelector(".tr-meta");
  const act = li.querySelector(".tr-act");
  const fill = li.querySelector(".tr-bar i");
  li.className = `tr is-${t.state}`;
  act.replaceChildren();
  li.querySelector(".tr-tags").textContent = [t.sas && `🔒 ${t.sas}`, t.link && t.link.p2p && "direct"].filter(Boolean).join(" · ");
  graph(li.querySelector(".tr-graph"), t.hist);

  switch (t.state) {
    case "offered":
      meta.textContent = `Waiting for ${who} to accept`;
      act.append(cancelBtn(t));
      break;
    case "active": {
      const rate = fmtRate(rateOf(t)), left = fmtLeft(etaOf(t));
      const parts = [`${t.dir === "send" ? "To" : "From"} ${who}`, `${fmtSize(t.bytes)} of ${fmtSize(t.size)}`];
      if (rate) parts.push(rate);
      if (left) parts.push(left);
      meta.textContent = parts.join(" · ");
      fill.style.width = `${t.size ? (t.bytes / t.size) * 100 : 0}%`;
      act.append(cancelBtn(t));
      break;
    }
    case "paused":
      meta.textContent = `Paused · ${fmtSize(t.bytes)} of ${fmtSize(t.size)} · resuming…`;
      fill.style.width = `${t.size ? (t.bytes / t.size) * 100 : 0}%`;
      act.append(cancelBtn(t));
      break;
    case "done":
      meta.textContent = t.dir === "send" ? `Sent to ${who}` : t.saved ? "Saved" : "Saved to your downloads";
      if (t.blobUrl) act.append(el("button", { class: "btn btn-ghost", type: "button", onclick: () => saveBlob(t) }, icon("save"), "Save again"));
      break;
    default:
      meta.textContent = t.note ? t.note[0].toUpperCase() + t.note.slice(1) : "Failed";
  }
}

function graph(svg, hist) {
  svg.toggleAttribute("hidden", hist.length < 2);
  if (hist.length < 2) return;
  const max = Math.max(1, ...hist);
  const pts = hist.map((v, i) => `${((i / (hist.length - 1)) * 100).toFixed(1)},${(23 - (v / max) * 21).toFixed(1)}`).join(" ");
  svg.querySelector("polyline").setAttribute("points", pts);
  svg.querySelector("polygon").setAttribute("points", `0,24 ${pts} 100,24`);
}

function cancelBtn(t) {
  return el("button", { class: "btn btn-ghost btn-icon", type: "button", "aria-label": "Cancel", title: "Cancel", onclick: () => cancel(t) }, icon("x"));
}

function removeRow(t) {
  const li = rows.get(t.id);
  if (!li) return;
  rows.delete(t.id);
  li.classList.add("is-leaving");
  setTimeout(() => {
    li.remove();
    if (!rows.size) {
      panel.hidden = true;
      document.body.classList.remove("has-panel", "panel-open");
      emit("layout");
    }
    overall();
  }, 240);
}

function overall() {
  let live = 0, total = 0, done = 0, current = null;
  for (const t of state.transfers.values()) {
    if (isDone(t)) continue;
    live++;
    total += t.size;
    done += t.bytes;
    if (t.state === "active") current = t;
  }
  if (!live) {
    bar.style.width = "0";
    sum.textContent = rows.size ? "Finished" : "";
    return;
  }
  const pct = total ? Math.floor((done / total) * 100) : 0;
  bar.style.width = `${pct}%`;
  if (collapsed && current) {
    sum.textContent = `${current.dir === "send" ? "Sending" : "Receiving"} ${current.name} · ${pct}%`;
  } else {
    sum.textContent = `${live} active · ${pct}%`;
  }
}
