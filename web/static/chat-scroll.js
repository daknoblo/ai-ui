// Follow new output only while the reader remains at the end of the conversation.
(function () {
	"use strict";

	if (window.AIUI_CHAT_SCROLL) return;
	window.AIUI_CHAT_SCROLL = true;
	var current = null;
	var bottomThreshold = 24;

	function atBottom(state) {
		var box = state.box;
		return box.scrollHeight - box.clientHeight - box.scrollTop <= bottomThreshold;
	}

	function rememberPosition(state) {
		var box = state.box;
		state.top = box.scrollTop;
		var rect = box.getBoundingClientRect();
		var hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + Math.min(8, rect.height / 2));
		var message = hit && hit.closest(".msg");
		var bubble = message && message.querySelector(".bubble");
		state.anchor = bubble && box.contains(bubble) ? {
			element: bubble,
			offset: bubble.getBoundingClientRect().top - rect.top
		} : null;
	}

	function showStatus(state) {
		var streams = Array.from(state.box.querySelectorAll("[sse-connect]")).filter(function (stream) {
			return stream.dataset.streamState !== "finished";
		});
		var kind = "latest";
		if (streams.length) {
			kind = streams.every(function (stream) {
				return stream.dataset.streamState === "reconnecting";
			}) ? "reconnecting" : "streaming";
		} else if (state.finished) {
			kind = "finished";
		}
		state.box.dataset.following = state.following ? "1" : "0";
		state.status.hidden = state.following;
		state.status.dataset.state = kind;
		var text = state.button.dataset[kind];
		if (state.label.textContent !== text) state.label.textContent = text;
	}

	function onScroll(state) {
		var top = state.box.scrollTop;
		if (top < state.top - 1) {
			state.following = false;
		} else if (top > state.top + 1 && atBottom(state)) {
			state.following = true;
			state.finished = false;
		}
		rememberPosition(state);
		showStatus(state);
	}

	function pause(state) {
		if (state.box.scrollTop <= 0 &&
			!state.box.querySelector("[sse-connect]:not([data-stream-state='finished'])")) return;
		state.following = false;
		rememberPosition(state);
		showStatus(state);
	}

	function updatePosition(state) {
		if (current !== state || !state.box.isConnected) return;
		var box = state.box;
		var target;
		if (state.following) {
			target = box.scrollHeight - box.clientHeight;
			state.anchor = null;
		} else if (state.anchor && box.contains(state.anchor.element)) {
			// The bubble survives SSE innerHTML replacements. Anchoring it also
			// compensates for images or tool/model notices inserted above it.
			var offset = state.anchor.element.getBoundingClientRect().top - box.getBoundingClientRect().top;
			target = box.scrollTop + offset - state.anchor.offset;
		} else {
			target = state.top;
		}
		if (Math.abs(box.scrollTop - target) > 0.5) box.scrollTop = target;
		state.top = box.scrollTop;
		showStatus(state);
	}

	function jumpToLatest(state) {
		state.following = true;
		state.finished = false;
		updatePosition(state);
		state.box.focus({ preventScroll: true });
	}

	function initialize() {
		var box = document.getElementById("messages");
		if (current && current.box === box) return current;
		if (current) {
			current.resize.disconnect();
			current.mutations.disconnect();
			current = null;
		}
		if (!box) return null;
		var button = document.getElementById("scroll-to-latest");
		var status = document.getElementById("scroll-status");
		var label = document.getElementById("scroll-status-text");
		if (!button || !status || !label) {
			console.warn("Chat scroll controls are missing");
			return null;
		}
		var state = { box: box, button: button, status: status, label: label,
			following: true, finished: false, top: box.scrollTop, anchor: null };
		current = state;
		var observed = new Set();
		state.resize = new ResizeObserver(function () { updatePosition(state); });
		state.resize.observe(box);
		function observeMessages() {
			if (current !== state || !box.isConnected) return;
			observed.forEach(function (element) {
				if (element.parentElement !== box) {
					state.resize.unobserve(element);
					observed.delete(element);
				}
			});
			Array.from(box.children).forEach(function (element) {
				if (!observed.has(element)) {
					state.resize.observe(element);
					observed.add(element);
				}
			});
			updatePosition(state);
		}
		state.mutations = new MutationObserver(observeMessages);
		state.mutations.observe(box, { childList: true });
		box.addEventListener("scroll", function () { onScroll(state); }, { passive: true });
		box.addEventListener("wheel", function (event) {
			if (event.deltaY < 0) pause(state);
		}, { passive: true });
		var touchY = null;
		box.addEventListener("touchstart", function (event) {
			touchY = event.touches.length === 1 ? event.touches[0].clientY : null;
		}, { passive: true });
		box.addEventListener("touchmove", function (event) {
			if (event.touches.length !== 1) return;
			var nextY = event.touches[0].clientY;
			if (touchY !== null && nextY > touchY) pause(state);
			touchY = nextY;
		}, { passive: true });
		box.addEventListener("keydown", function (event) {
			if (event.target.closest("input, textarea, select, [contenteditable='true']")) return;
			if (["ArrowUp", "PageUp", "Home"].includes(event.key) || (event.key === " " && event.shiftKey)) {
				pause(state);
			}
		});
		button.addEventListener("click", function () { jumpToLatest(state); });
		observeMessages();
		return state;
	}

	function messageState(event) {
		var state = initialize();
		return state && event.target instanceof Element && state.box.contains(event.target) ? state : null;
	}

	document.addEventListener("htmx:beforeSwap", function (event) {
		var state = initialize();
		if (!state) return;
		var request = event.detail.requestConfig;
		if (event.detail.target === state.box && request && request.elt && request.elt.closest("#chat-form") &&
			event.detail.shouldSwap) {
			state.following = true;
			state.finished = false;
		}
		if (event.detail.target && state.box.contains(event.detail.target)) onScroll(state);
	});
	document.addEventListener("htmx:sseBeforeMessage", function (event) {
		var state = messageState(event);
		if (state) onScroll(state);
	});
	document.addEventListener("htmx:afterSwap", function (event) {
		var state = initialize();
		if (state && event.target instanceof Element && state.box.contains(event.target)) updatePosition(state);
	});
	document.addEventListener("htmx:sseMessage", function (event) {
		var state = messageState(event);
		if (state) updatePosition(state);
	});
	document.addEventListener("htmx:sseOpen", function (event) {
		var state = messageState(event);
		if (!state || event.target.dataset.streamState === "finished") return;
		event.target.dataset.streamState = "streaming";
		showStatus(state);
	});
	document.addEventListener("htmx:sseError", function (event) {
		var state = messageState(event);
		if (!state || event.target.dataset.streamState === "finished") return;
		event.target.dataset.streamState = "reconnecting";
		showStatus(state);
	});
	document.addEventListener("htmx:sseClose", function (event) {
		var state = messageState(event);
		if (!state || event.detail.type !== "message") return;
		event.target.dataset.streamState = "finished";
		if (!state.following) state.finished = true;
		updatePosition(state);
	});
	document.addEventListener("htmx:load", initialize);
	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", initialize, { once: true });
	} else {
		initialize();
	}
})();
