(function () {
	"use strict";
	if (window.AIUI_SIDEBAR_RESIZE) return;
	window.AIUI_SIDEBAR_RESIZE = true;

	var key = "ai-ui-sidebar-width";
	var mobile = window.matchMedia("(max-width: 720px)");
	var preferred = null;
	var drag = null;
	var storageFailed = false;
	try {
		var saved = localStorage.getItem(key);
		if (saved !== null) {
			var width = Number(saved);
			if (Number.isFinite(width) && width > 0) preferred = width;
			else console.warn("Ignoring invalid saved sidebar width");
		}
	} catch (error) {
		console.warn("Could not read sidebar width", error.name);
		storageFailed = true;
	}

	function bounds(sidebar) {
		var style = getComputedStyle(sidebar);
		var min = parseFloat(style.getPropertyValue("--sidebar-min"));
		return {
			min: min,
			max: Math.max(min, Math.min(parseFloat(style.getPropertyValue("--sidebar-max")),
				document.documentElement.clientWidth - parseFloat(style.getPropertyValue("--sidebar-main-min")))),
			defaultWidth: parseFloat(style.getPropertyValue("--sidebar-default"))
		};
	}

	function render() {
		var sidebar = document.getElementById("sidebar");
		var handle = document.getElementById("sidebar-resize");
		if (!sidebar || !handle) return;
		var limits = bounds(sidebar);
		var width = Math.round(Math.max(limits.min, Math.min(limits.max, preferred ?? limits.defaultWidth)));
		sidebar.style.setProperty("--sidebar-width", width + "px");
		handle.setAttribute("aria-valuemin", limits.min);
		handle.setAttribute("aria-valuemax", limits.max);
		handle.setAttribute("aria-valuenow", width);
		var notice = document.getElementById("sidebar-resize-status");
		if (notice) notice.hidden = !storageFailed;
	}

	function save() {
		try {
			if (preferred === null) localStorage.removeItem(key);
			else localStorage.setItem(key, String(preferred));
			storageFailed = false;
		} catch (error) {
			console.warn("Could not save sidebar width", error.name);
			storageFailed = true;
		}
		render();
	}

	function finish(commit) {
		if (!drag) return;
		var previous = drag;
		drag = null;
		document.body.classList.remove("sidebar-resizing");
		if (previous.handle.hasPointerCapture(previous.pointer)) previous.handle.releasePointerCapture(previous.pointer);
		if (commit) save();
		else {
			preferred = previous.preferred;
			render();
		}
	}

	document.addEventListener("pointerdown", function (event) {
		if (event.target.id !== "sidebar-resize" || event.button !== 0 || !event.isPrimary || mobile.matches || drag) return;
		event.preventDefault();
		var handle = event.target;
		drag = {
			handle: handle, pointer: event.pointerId, x: event.clientX,
			width: handle.parentElement.getBoundingClientRect().width, preferred: preferred
		};
		handle.focus({ preventScroll: true });
		handle.setPointerCapture(event.pointerId);
		document.body.classList.add("sidebar-resizing");
	});
	document.addEventListener("pointermove", function (event) {
		if (!drag || event.pointerId !== drag.pointer) return;
		var limits = bounds(drag.handle.parentElement);
		preferred = Math.round(Math.max(limits.min, Math.min(limits.max, drag.width + event.clientX - drag.x)));
		render();
	});
	document.addEventListener("pointerup", function (event) {
		if (drag && event.pointerId === drag.pointer) finish(true);
	});
	document.addEventListener("pointercancel", function (event) {
		if (drag && event.pointerId === drag.pointer) finish(false);
	});
	document.addEventListener("lostpointercapture", function (event) {
		if (drag && event.pointerId === drag.pointer) finish(false);
	});
	document.addEventListener("keydown", function (event) {
		if (drag && event.key === "Escape") {
			event.preventDefault();
			finish(false);
			return;
		}
		if (event.target.id !== "sidebar-resize" || mobile.matches || drag) return;
		var sidebar = event.target.parentElement;
		var limits = bounds(sidebar);
		var width = sidebar.getBoundingClientRect().width;
		switch (event.key) {
		case "ArrowLeft": width -= 10; break;
		case "ArrowRight": width += 10; break;
		case "Home": width = limits.min; break;
		case "End": width = limits.max; break;
		case "Enter": width = null; break;
		default: return;
		}
		event.preventDefault();
		preferred = width === null ? null : Math.round(Math.max(limits.min, Math.min(limits.max, width)));
		save();
	});
	document.addEventListener("dblclick", function (event) {
		if (event.target.id !== "sidebar-resize" || mobile.matches) return;
		preferred = null;
		save();
	});
	window.addEventListener("resize", function () { finish(false); render(); });
	window.addEventListener("blur", function () { finish(false); });
	document.addEventListener("htmx:beforeSwap", function (event) {
		if (drag && event.detail.target?.contains(drag.handle)) finish(false);
	});
	document.addEventListener("htmx:load", render);
	render();
})();
