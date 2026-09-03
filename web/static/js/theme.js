(function () {
  var t = "dark";
  try { t = localStorage.getItem("beam:theme") || (matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark"); } catch (e) {}
  document.documentElement.dataset.theme = t;
})();
