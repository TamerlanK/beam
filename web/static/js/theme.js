(function () {
  var t = "dark";
  try {
    t =
      localStorage.getItem("beam:theme") ||
      (matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
  } catch (e) {}
  document.documentElement.dataset.theme = t;

  function report(msg) {
    var host = document.getElementById("toasts");
    if (!host) return;
    var d = document.createElement("div");
    d.className = "toast bad";
    d.setAttribute("role", "alert");
    d.textContent = "Something broke: " + msg;
    host.appendChild(d);
    try {
      host.showPopover();
    } catch (e) {}
  }
  addEventListener("error", function (e) {
    report(e.message || "script error");
  });
  addEventListener("unhandledrejection", function (e) {
    report((e.reason && e.reason.message) || String(e.reason));
  });
})();
