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

		uploadFiles(chatId, files);
	});

	// Alle abgelegten Dateien in einer Anfrage hochladen; der Server verarbeitet
	// sie und liefert die aktualisierte Dokumentliste samt Sammelmeldung zurück.
	function uploadFiles(chatId, fileList) {
		var form = new FormData();
		for (var i = 0; i < fileList.length; i++) {
			form.append("file", fileList[i]);
		}

		fetch("/chat/" + encodeURIComponent(chatId) + "/documents", {
			method: "POST",
			body: form,
		})
			.then(function (resp) {
				return resp.text();
			})
			.then(function (html) {
				replaceDocList(html);
			})
			.catch(function () {
				/* Fehler werden serverseitig als Notiz gerendert; hier nichts tun. */
			});
	}

	// Ersetzt die Dokumentliste in der Seitenleiste durch das Server-Fragment.
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
