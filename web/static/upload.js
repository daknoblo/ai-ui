// Drag-&-Drop-Upload für das Chatfenster.
// Dateien, die über dem Chatbereich abgelegt werden, werden an den aktuellen
// Chat hochgeladen und die Dokumentliste in der Seitenleiste aktualisiert.
(function () {
	"use strict";

	var dragDepth = 0;

	function dropzone() {
		return document.getElementById("chat-dropzone");
	}

	// Uploads sind nur erlaubt, wenn die Bereitschaft serverseitig verifiziert
	// wurde. Das Attribut spiegelt diesen Zustand beim Seitenrendern wider.
	function uploadsReady() {
		var z = dropzone();
		return z && z.getAttribute("data-uploads-ready") === "1";
	}

	// Nur echte Datei-Drags berücksichtigen (keine internen Element-Drags).
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

		// Nicht verifiziert: Drop ignorieren (serverseitig ohnehin gesperrt).
		if (!uploadsReady()) {
			return;
		}

		var chatId = z.getAttribute("data-chat-id");
		if (!chatId) return;

		var files = e.dataTransfer.files;
		if (!files || files.length === 0) return;

		uploadFiles(chatId, Array.prototype.slice.call(files));
	});

	// handleAttach wird vom Datei-Input (📎) aufgerufen. Der Upload läuft
	// asynchron und unabhängig vom Chat-Eingabefeld, damit eine begonnene
	// Nachricht nicht verloren geht.
	window.handleAttach = function (input) {
		var chatId = input.getAttribute("data-chat-id");
		var files = input.files;
		if (!chatId || !files || files.length === 0) {
			input.value = "";
			return;
		}
		uploadFiles(chatId, Array.prototype.slice.call(files));
		input.value = ""; // erlaubt erneutes Wählen derselben Datei
	};

	// uploadFiles lädt mehrere Dateien nacheinander hoch und zeigt dabei den
	// Fortschritt an. Jede Datei wird einzeln verarbeitet, sodass der Nutzer den
	// Verlauf sieht und die Dokumentliste fortlaufend aktualisiert wird.
	function uploadFiles(chatId, files) {
		var total = files.length;
		var done = 0;
		var failed = 0;

		showProgress("Lade Dokumente hoch…", 0, total);

		function next(index) {
			if (index >= total) {
				var msg = done + " von " + total + " verarbeitet";
				if (failed > 0) msg += " (" + failed + " fehlgeschlagen)";
				finishProgress(msg, failed > 0);
				return;
			}

			var file = files[index];
			var form = new FormData();
			form.append("file", file);

			var xhr = new XMLHttpRequest();
			xhr.open("POST", "/chat/" + encodeURIComponent(chatId) + "/documents");

			// Netzwerk-Fortschritt der aktuellen Datei in die Gesamtanzeige mappen.
			xhr.upload.onprogress = function (evt) {
				var fileFrac = evt.lengthComputable ? evt.loaded / evt.total : 0;
				var overall = (index + fileFrac) / total;
				updateProgress(
					"Verarbeite " + (index + 1) + " von " + total + ": " + file.name,
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
					"Verarbeite " + (index + 1) + " von " + total + "…",
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

	// ---- Fortschrittsanzeige ----

	function progressEl() {
		return document.getElementById("upload-status");
	}

	function showProgress(label, current, total) {
		var el = progressEl();
		if (!el) return;
		el.hidden = false;
		el.innerHTML =
			'<div class="upload-progress-label">' +
			escapeHTML(label) +
			'</div><div class="upload-progress-bar"><div class="upload-progress-fill" style="width:0%"></div></div>';
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
		// Anzeige nach kurzer Zeit ausblenden.
		setTimeout(function () {
			el.hidden = true;
			el.classList.remove("error");
		}, 4000);
	}

	function escapeHTML(s) {
		var d = document.createElement("div");
		d.textContent = s;
		return d.innerHTML;
	}

	// Ersetzt die Dokumentliste durch das Server-Fragment.
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
	}
})();
