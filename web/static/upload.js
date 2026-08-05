// Drag & drop upload for the chat window.
// Files dropped on the chat area are uploaded to the current chat and the
// document list is refreshed afterwards.
(function () {
	"use strict";

	var dragDepth = 0;

	// t looks up a localized string rendered by the server (see base.html) and
	// substitutes {placeholders}. Falls back to the key so a missing string is
	// visible but never breaks the upload.
	function t(key, params) {
		var dict = window.AIUI_I18N || {};
		var s = dict[key] || key;
		if (!params) return s;
		return s.replace(/\{(\w+)\}/g, function (match, name) {
			return Object.prototype.hasOwnProperty.call(params, name) ? params[name] : match;
		});
	}

	function dropzone() {
		return document.getElementById("chat-dropzone");
	}

	// Uploads are only allowed once readiness has been verified server side.
	// The attribute mirrors that state at render time.
	function uploadsReady() {
		var z = dropzone();
		return z && z.getAttribute("data-uploads-ready") === "1";
	}

	// Only react to real file drags (not to internal element drags).
	function hasFiles(e) {
		if (!e.dataTransfer) return false;
		var types = e.dataTransfer.types;
		if (!types) return false;
		for (var i = 0; i < types.length; i++) {
			if (types[i] === "Files") return true;
		}
		return false;
	}

	function showOverlay() {
		var z = dropzone();
		if (z) z.classList.add(uploadsReady() ? "dragover" : "dragover-blocked");
	}

	function hideOverlay() {
		var z = dropzone();
		if (z) z.classList.remove("dragover", "dragover-blocked");
	}

	document.addEventListener("dragenter", function (e) {
		if (!hasFiles(e) || !dropzone()) return;
		e.preventDefault();
		dragDepth++;
		showOverlay();
	});

	document.addEventListener("dragover", function (e) {
		if (!hasFiles(e) || !dropzone()) return;
		e.preventDefault();
		e.dataTransfer.dropEffect = uploadsReady() ? "copy" : "none";
	});

	document.addEventListener("dragleave", function (e) {
		if (!hasFiles(e) || !dropzone()) return;
		dragDepth--;
		if (dragDepth <= 0) {
			dragDepth = 0;
			hideOverlay();
		}
	});

	document.addEventListener("drop", function (e) {
		var z = dropzone();
		if (!hasFiles(e) || !z) return;
		e.preventDefault();
		dragDepth = 0;
		hideOverlay();

		// Not verified: ignore the drop (it is blocked server side anyway).
		if (!uploadsReady()) {
			return;
		}

		var chatId = z.getAttribute("data-chat-id");
		if (!chatId) return;

		var files = e.dataTransfer.files;
		if (!files || files.length === 0) return;

		uploadFiles(chatId, Array.prototype.slice.call(files));
	});

	// handleAttach is called by the file input (📎). The upload runs
	// asynchronously and independently of the chat input so a message that is
	// already being typed is not lost.
	window.handleAttach = function (input) {
		var chatId = input.getAttribute("data-chat-id");
		var files = input.files;
		if (!chatId || !files || files.length === 0) {
			input.value = "";
			return;
		}
		uploadFiles(chatId, Array.prototype.slice.call(files));
		input.value = ""; // allows selecting the same file again
	};

	// uploadFiles uploads several files one after another and shows the
	// progress. Each file is processed individually so the user sees the
	// progression and the document list is updated continuously.
	function uploadFiles(chatId, files) {
		var total = files.length;
		var done = 0;
		var failed = 0;

		showProgress(t("uploadStart"));

		function next(index) {
			if (index >= total) {
				var msg = t("summary", { done: done, total: total });
				if (failed > 0) msg += " " + t("failed", { failed: failed });
				finishProgress(msg, failed > 0);
				return;
			}

			var file = files[index];
			var form = new FormData();
			form.append("file", file);

			var xhr = new XMLHttpRequest();
			xhr.open("POST", "/chat/" + encodeURIComponent(chatId) + "/documents");

			// Map the network progress of the current file onto the overall bar.
			xhr.upload.onprogress = function (evt) {
				var fileFrac = evt.lengthComputable ? evt.loaded / evt.total : 0;
				var overall = (index + fileFrac) / total;
				updateProgress(
					t("processing", { n: index + 1, total: total, name: file.name }),
					overall
				);
			};

			xhr.onload = function () {
				if (xhr.status >= 200 && xhr.status < 300) {
					done++;
					replaceDocList(xhr.responseText);
				} else {
					failed++;
				}
				updateProgress(
					t("processingShort", { n: index + 1, total: total }),
					(index + 1) / total
				);
				next(index + 1);
			};

			xhr.onerror = function () {
				failed++;
				next(index + 1);
			};

			xhr.send(form);
		}

		next(0);
	}

	// ---- progress display ----

	function progressEl() {
		return document.getElementById("upload-status");
	}

	// showProgress builds the progress widget from DOM nodes instead of an HTML
	// string. That avoids both an HTML injection surface and inline style
	// attributes, which keeps the Content-Security-Policy strict.
	function showProgress(label) {
		var el = progressEl();
		if (!el) return;
		el.hidden = false;
		el.textContent = "";

		var labelEl = document.createElement("div");
		labelEl.className = "upload-progress-label";
		labelEl.textContent = label;

		var fillEl = document.createElement("div");
		fillEl.className = "upload-progress-fill";
		fillEl.style.width = "0%";

		var barEl = document.createElement("div");
		barEl.className = "upload-progress-bar";
		barEl.appendChild(fillEl);

		el.appendChild(labelEl);
		el.appendChild(barEl);
	}

	function updateProgress(label, frac) {
		var el = progressEl();
		if (!el) return;
		el.hidden = false;
		var pct = Math.max(0, Math.min(100, Math.round(frac * 100)));
		var labelEl = el.querySelector(".upload-progress-label");
		var fillEl = el.querySelector(".upload-progress-fill");
		if (labelEl) labelEl.textContent = label + " (" + pct + "%)";
		if (fillEl) fillEl.style.width = pct + "%";
	}

	function finishProgress(label, isError) {
		var el = progressEl();
		if (!el) return;
		var fillEl = el.querySelector(".upload-progress-fill");
		if (fillEl) fillEl.style.width = "100%";
		var labelEl = el.querySelector(".upload-progress-label");
		if (labelEl) labelEl.textContent = label;
		el.classList.toggle("error", !!isError);
		// Hide the display again after a short delay.
		setTimeout(function () {
			el.hidden = true;
			el.classList.remove("error");
		}, 4000);
	}

	// Replaces the document list with the server rendered fragment.
	function replaceDocList(html) {
		var current = document.getElementById("doc-list");
		if (!current) return;
		var tmp = document.createElement("div");
		tmp.innerHTML = html.trim();
		var fresh = tmp.querySelector("#doc-list");
		if (!fresh) return;
		current.replaceWith(fresh);
		if (window.htmx) {
			window.htmx.process(fresh);
		}
		// The chips decide whether image mode edits or generates.
		if (window.applyComposerState) {
			window.applyComposerState();
		}
	}
})();
