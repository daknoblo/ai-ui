(function () {
	"use strict";
	if (window.AIUI_RETRY_RESPONSE) return;
	window.AIUI_RETRY_RESPONSE = true;
	var pendingRequest = false;
	function update() {
		var busy = pendingRequest || !!document.querySelector('#messages [sse-connect]:not([data-stream-state="finished"])');
		document.querySelectorAll(".response-retry").forEach(function (button) {
			button.disabled = busy || button.dataset.retryUnavailable === "1";
		});
	}
	function source(event) {
		return event.detail.requestConfig?.elt || event.detail.elt;
	}
	document.addEventListener("htmx:beforeRequest", function (event) {
		if (!source(event)?.matches(".response-retry")) return;
		if (pendingRequest || document.querySelector('#messages [sse-connect]:not([data-stream-state="finished"])')) {
			event.preventDefault();
			return;
		}
		pendingRequest = true;
		update();
	});
	document.addEventListener("htmx:afterRequest", function (event) {
		if (!source(event)?.matches(".response-retry")) return;
		pendingRequest = false;
		var notice = source(event).closest(".response-actions")?.querySelector(".response-retry-status");
		if (notice) {
			notice.textContent = event.detail.successful ? "" : (event.detail.xhr.responseText || source(event).dataset.retryError);
		}
		update();
	});
	document.addEventListener("htmx:afterSwap", update);
	document.addEventListener("htmx:load", update);
	document.addEventListener("htmx:sseClose", function () { queueMicrotask(update); });
	update();
})();
