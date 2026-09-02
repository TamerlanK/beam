import { on } from "./state.js";
import { $, el, icon } from "./util.js";

const GLYPH = { ok: "check", bad: "x", info: "info" };

export function initToasts() {
  const host = $("toasts");
  on("toast", ({ msg, kind }) => {
    while (host.children.length >= 3) host.firstElementChild.remove();
    const t = el("div", { class: `toast ${kind}`, role: kind === "bad" ? "alert" : "status" },
      el("span", { class: "toast-ic" }, icon(GLYPH[kind] || "info")),
      el("span", { text: msg }));
    host.append(t);
    setTimeout(() => {
      t.classList.add("is-leaving");
      setTimeout(() => t.remove(), 260);
    }, kind === "bad" ? 5500 : 3800);
  });
}
