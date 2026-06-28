(function() {
  var STORAGE_KEY = "alto.sidebar.width";
  var DEFAULT_WIDTH = 280;
  var MIN_WIDTH = 220;
  var MAX_WIDTH = 560;
  var KEYBOARD_STEP = 16;

  function initSidebarResize() {
    var shell = document.querySelector(".app-shell");
    var handle = document.querySelector(".app-sidebar-resizer");
    if (!shell || !handle) {
      return;
    }

    var currentWidth = DEFAULT_WIDTH;
    var activePointerId = null;

    function readStoredWidth() {
      try {
        var raw = localStorage.getItem(STORAGE_KEY);
        if (!raw) {
          return null;
        }
        var value = parseInt(raw, 10);
        return isNaN(value) ? null : value;
      } catch (_) {
        return null;
      }
    }

    function saveWidth(width) {
      try {
        localStorage.setItem(STORAGE_KEY, String(Math.round(width)));
      } catch (_) {}
    }

    function computeMaxWidth() {
      var shellWidth = Math.round(shell.getBoundingClientRect().width);
      var gutterWidth = Math.round(handle.getBoundingClientRect().width) || 10;
      return Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, shellWidth - gutterWidth - 320));
    }

    function clampWidth(width) {
      return Math.max(MIN_WIDTH, Math.min(computeMaxWidth(), Math.round(width)));
    }

    function syncAria(width) {
      handle.setAttribute("aria-valuemin", String(MIN_WIDTH));
      handle.setAttribute("aria-valuemax", String(computeMaxWidth()));
      handle.setAttribute("aria-valuenow", String(width));
      handle.setAttribute("aria-valuetext", width + " pixels");
    }

    function applyWidth(width, persist) {
      currentWidth = clampWidth(width);
      shell.style.setProperty("--sidebar-width", currentWidth + "px");
      syncAria(currentWidth);
      if (persist) {
        saveWidth(currentWidth);
      }
    }

    function cleanupDrag() {
      if (activePointerId === null) {
        return;
      }
      if (handle.hasPointerCapture && handle.hasPointerCapture(activePointerId)) {
        handle.releasePointerCapture(activePointerId);
      }
      activePointerId = null;
      handle.classList.remove("dragging");
      document.body.classList.remove("sidebar-resizing");
      applyWidth(currentWidth, true);
    }

    function widthFromPointer(event) {
      return event.clientX - shell.getBoundingClientRect().left;
    }

    handle.addEventListener("pointerdown", function(event) {
      if (event.button !== undefined && event.button !== 0) {
        return;
      }
      activePointerId = event.pointerId;
      if (handle.setPointerCapture) {
        handle.setPointerCapture(activePointerId);
      }
      handle.classList.add("dragging");
      document.body.classList.add("sidebar-resizing");
      handle.focus();
      event.preventDefault();
    });

    handle.addEventListener("pointermove", function(event) {
      if (event.pointerId !== activePointerId) {
        return;
      }
      applyWidth(widthFromPointer(event), false);
    });

    handle.addEventListener("pointerup", function(event) {
      if (event.pointerId !== activePointerId) {
        return;
      }
      cleanupDrag();
    });

    handle.addEventListener("pointercancel", function(event) {
      if (event.pointerId !== activePointerId) {
        return;
      }
      cleanupDrag();
    });

    handle.addEventListener("dblclick", function() {
      applyWidth(DEFAULT_WIDTH, true);
    });

    handle.addEventListener("keydown", function(event) {
      var step = event.shiftKey ? KEYBOARD_STEP * 2 : KEYBOARD_STEP;

      if (event.key === "ArrowLeft") {
        applyWidth(currentWidth - step, true);
        event.preventDefault();
        return;
      }

      if (event.key === "ArrowRight") {
        applyWidth(currentWidth + step, true);
        event.preventDefault();
        return;
      }

      if (event.key === "Home") {
        applyWidth(MIN_WIDTH, true);
        event.preventDefault();
        return;
      }

      if (event.key === "End") {
        applyWidth(computeMaxWidth(), true);
        event.preventDefault();
      }
    });

    window.addEventListener("resize", function() {
      applyWidth(currentWidth, false);
    });

    window.addEventListener("blur", cleanupDrag);

    applyWidth(readStoredWidth() || DEFAULT_WIDTH, false);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initSidebarResize);
    return;
  }

  initSidebarResize();
})();
