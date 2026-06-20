(function () {
  "use strict";

  var refreshAfterMs = 60000;
  var timer = null;

  function scheduleRefresh() {
    window.clearTimeout(timer);
    timer = window.setTimeout(function () {
      if (document.hidden) {
        scheduleRefresh();
        return;
      }
      window.location.reload();
    }, refreshAfterMs);
  }

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) {
      scheduleRefresh();
    }
  });

  scheduleRefresh();
})();
