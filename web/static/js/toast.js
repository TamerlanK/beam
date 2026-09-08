import { on, toast } from "./state.js";
import { $, el, icon, asURL, copyText } from "./util.js";

const GLYPH = { ok: "check", bad: "x", info: "info" };

export function initToasts() {
  const host = $("toasts");

  function dismiss(t) {
    t.classList.add("is-leaving");
    setTimeout(() => t.remove(), 260);
  }

  function push(t, ms) {
    while (host.children.length >= 3) {
      const old = host.querySelector(".toast:not(.note)");
      if (!old) break;
      old.remove();
    }
    host.append(t);
    try {
      if (host.matches(":popover-open")) host.hidePopover();
      host.showPopover();
    } catch {}
    if (ms) setTimeout(() => dismiss(t), ms);
  }

  on("toast", ({ msg, kind }) => {
    push(
      el(
        "div",
        { class: `toast ${kind}`, role: kind === "bad" ? "alert" : "status" },
        el("span", { class: "toast-ic" }, icon(GLYPH[kind] || "info")),
        el("span", { text: msg }),
      ),
      kind === "bad" ? 5500 : 3800,
    );
  });

  on("snippet", (d) => {
    const url = asURL(d.text);
    const t = el(
      "div",
      { class: "toast note", role: "status" },
      el("b", {
        class: "note-from",
        text: `Note from ${d.from ? d.from.name : "someone"}`,
      }),
      el("pre", { class: "snip-recv", text: d.text, tabindex: "0" }),
      el(
        "div",
        { class: "note-act" },
        url
          ? el(
              "a",
              {
                class: "btn btn-ghost",
                href: url,
                target: "_blank",
                rel: "noopener noreferrer",
              },
              "Open link",
            )
          : null,
        el(
          "button",
          {
            class: "btn btn-ghost",
            type: "button",
            onclick: async () => {
              await copyText(d.text);
              toast("Copied", "ok");
            },
          },
          "Copy",
        ),
        el(
          "button",
          { class: "btn btn-ghost", type: "button", onclick: () => dismiss(t) },
          "Dismiss",
        ),
      ),
    );
    push(t);
  });
}
