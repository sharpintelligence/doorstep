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

  function changed(field) {
    if (field.type === "checkbox" || field.type === "radio") {
      return field.checked !== field.defaultChecked;
    }
    return field.value !== "";
  }

  function inUse(region) {
    if (region.querySelector("details[open]")) {
      return true;
    }
    var fields = region.querySelectorAll("input:not([type=hidden]), textarea");
    for (var i = 0; i < fields.length; i++) {
      if (fields[i] === document.activeElement || changed(fields[i])) {
        return true;
      }
    }
    return false;
  }

  document.addEventListener("htmx:beforeRequest", function (event) {
    var region = event.detail && event.detail.elt;
    if (region && region.id === "live" && inUse(region)) {
      event.preventDefault();
    }
  });

  document.addEventListener("htmx:responseError", function (event) {
    var xhr = event.detail && event.detail.xhr;
    if (xhr && xhr.status === 401) {
      window.location.reload();
    }
  });
})();
