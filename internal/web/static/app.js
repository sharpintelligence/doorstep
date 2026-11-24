(function () {
  "use strict";

  function localiseTimes(root) {
    root.querySelectorAll("time[datetime]").forEach(function (el) {
      var when = new Date(el.getAttribute("datetime"));
      if (!isNaN(when.getTime())) {
        el.title = when.toLocaleString();
      }
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    localiseTimes(document);
  });

  document.addEventListener("htmx:afterSettle", function () {
    localiseTimes(document);
  });

  document.addEventListener("htmx:responseError", function (event) {
    var xhr = event.detail && event.detail.xhr;
    if (xhr && xhr.status === 401) {
      window.location.reload();
    }
  });
})();
