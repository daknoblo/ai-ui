// Copy only the sanitized response body, not model tags, tool notices or usage.
(function () {
	"use strict";

	if (window.AIUI_COPY_RESPONSE) return;
	window.AIUI_COPY_RESPONSE = true;
	var allowed = new Set(["P", "DIV", "SPAN", "H1", "H2", "H3", "H4", "H5", "H6",
		"STRONG", "B", "EM", "I", "U", "DEL", "S", "SUP", "SUB", "UL", "OL", "LI",
		"BLOCKQUOTE", "PRE", "CODE", "TABLE", "THEAD", "TBODY", "TFOOT", "TR", "TH", "TD",
		"A", "BR", "HR"]);

	function copyNode(node) {
		if (node.nodeType === Node.TEXT_NODE) return document.createTextNode(node.textContent);
		if (node.nodeType !== Node.ELEMENT_NODE) return document.createDocumentFragment();
		if (node.tagName === "IMG") return document.createTextNode(node.getAttribute("alt") || "");
		if (!allowed.has(node.tagName)) return document.createDocumentFragment();
		var result = document.createElement(node.tagName.toLowerCase());
		if (node.tagName === "A") {
			try {
				var url = new URL(node.getAttribute("href") || "", document.baseURI);
				if (["http:", "https:", "mailto:"].includes(url.protocol)) result.setAttribute("href", url.href);
			} catch {
				// Keep link text without an invalid destination.
			}
		}
		for (var attribute of ["start", "value", "colspan", "rowspan"]) {
			var value = node.getAttribute(attribute);
			if (value && /^-?\d{1,4}$/.test(value)) result.setAttribute(attribute, value);
		}
		if (node.tagName === "TABLE") result.style.borderCollapse = "collapse";
		if (node.tagName === "TH" || node.tagName === "TD") {
			result.style.border = "1px solid #888";
			result.style.padding = "4px 8px";
		}
		if (node.tagName === "PRE") {
			result.style.whiteSpace = "pre-wrap";
			result.style.fontFamily = "monospace";
		}
		if (node.tagName === "CODE") result.style.fontFamily = "monospace";
		for (var child of node.childNodes) result.append(copyNode(child));
		return result;
	}

	function plainText(node, depth) {
		if (node.nodeType === Node.TEXT_NODE) return node.textContent;
		var tag = node.tagName;
		if (tag === "BR") return "\n";
		if (tag === "HR") return "\n---\n";
		if (tag === "PRE") return "\n" + node.textContent + "\n\n";
		if (tag === "UL" || tag === "OL") {
			var number = Number(node.getAttribute("start") || 1);
			return "\n" + Array.from(node.children).map(function (item) {
				if (item.tagName !== "LI") return plainText(item, depth);
				if (item.hasAttribute("value")) number = Number(item.getAttribute("value"));
				var marker = tag === "OL" ? String(number++) + ". " : "- ";
				return "  ".repeat(depth) + marker +
					Array.from(item.childNodes).map(function (child) { return plainText(child, depth + 1); }).join("").trim() + "\n";
			}).join("") + "\n";
		}
		if (tag === "TR") {
			return Array.from(node.children).map(function (cell) {
				return plainText(cell, depth).trim();
			}).join("\t") + "\n";
		}
		var content = Array.from(node.childNodes).map(function (child) { return plainText(child, depth); }).join("");
		if (["P", "DIV", "H1", "H2", "H3", "H4", "H5", "H6", "BLOCKQUOTE", "TABLE"].includes(tag)) {
			return "\n" + content.replace(/^\n+|\n+$/g, "") + "\n\n";
		}
		return content;
	}

	function report(button, key, state) {
		var status = button.parentElement.querySelector(".response-copy-status");
		status.dataset.state = state;
		status.textContent = button.dataset[key];
	}

	// HTTP-only local installations may not expose navigator.clipboard.
	// A user-initiated copy event still supports HTML in many such browsers.
	function copyWithEvent(html, text) {
		var previousFocus = document.activeElement;
		var selection = window.getSelection();
		var ranges = [];
		if (selection) {
			for (var i = 0; i < selection.rangeCount; i++) ranges.push(selection.getRangeAt(i).cloneRange());
		}
		var helper = document.createElement("textarea");
		helper.value = text;
		helper.readOnly = true;
		helper.tabIndex = -1;
		helper.setAttribute("aria-hidden", "true");
		helper.style.cssText = "position:fixed;left:-9999px;top:0;width:1px;height:1px;opacity:0";
		var supplied = false;
		function onCopy(event) {
			if (!event.clipboardData) return;
			event.clipboardData.setData("text/html", html);
			event.clipboardData.setData("text/plain", text);
			event.preventDefault();
			supplied = true;
		}
		document.addEventListener("copy", onCopy);
		document.body.append(helper);
		try {
			helper.focus({ preventScroll: true });
			helper.select();
			return document.execCommand("copy") && supplied;
		} finally {
			document.removeEventListener("copy", onCopy);
			helper.remove();
			if (previousFocus && previousFocus.isConnected) previousFocus.focus({ preventScroll: true });
			if (selection) {
				selection.removeAllRanges();
				for (var range of ranges) selection.addRange(range);
			}
		}
	}

	async function copyResponse(button) {
		var message = button.closest(".msg");
		if (!message || (message.hasAttribute("data-stream-state") && message.dataset.streamState !== "finished")) return;
		var status = button.parentElement.querySelector(".response-copy-status");
		status.textContent = "";
		delete status.dataset.state;
		var bubble = message.querySelector(".bubble");
		if (!bubble) {
			console.warn("Response copy failed: response body is missing");
			report(button, "copyFailed", "error");
			return;
		}
		var content = copyNode(bubble);
		var text = plainText(content, 0).replace(/^\n+|\n+$/g, "");
		if (!text.trim()) {
			report(button, "copyEmpty", "error");
			return;
		}
		var html = content.innerHTML;
		button.disabled = true;
		try {
			if (navigator.clipboard && navigator.clipboard.write && window.ClipboardItem) {
				try {
					await navigator.clipboard.write([new ClipboardItem({
						"text/html": new Blob([html], { type: "text/html" }),
						"text/plain": new Blob([text], { type: "text/plain" })
					})]);
					return;
				} catch (error) {
					console.warn("Formatted clipboard API unavailable; trying the copy-event fallback", error.name);
				}
			}
			try {
				if (copyWithEvent(html, text)) {
					return;
				}
			} catch (error) {
				console.warn("Copy-event fallback failed", error.name);
			}
			if (navigator.clipboard && navigator.clipboard.writeText) {
				await navigator.clipboard.writeText(text);
				report(button, "copyPlain", "success");
				return;
			}
			throw new Error("ClipboardUnavailable");
		} catch (error) {
			console.warn("Response copy failed", error.name);
			report(button, "copyFailed", "error");
		} finally {
			button.disabled = false;
		}
	}

	document.addEventListener("click", function (event) {
		var button = event.target instanceof Element && event.target.closest(".response-copy");
		if (button && !button.disabled) void copyResponse(button);
	});
})();
