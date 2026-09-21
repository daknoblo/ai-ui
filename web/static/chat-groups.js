(function () {
	"use strict";
	if (window.aiuiChatGroups) return;
	window.aiuiChatGroups = true;

	let returnFocus = "new-group";
	let sidebarScroll = 0;
	let dragChat = "";
	const dragType = "application/x-ai-ui-chat";

	function source(event) {
		return event.detail.elt || event.detail.requestConfig?.elt;
	}
	function isGroupRequest(event) {
		return !!source(event)?.closest("[data-group-action]");
	}
	function currentChat() {
		return document.querySelector(".chat-groups")?.dataset.currentChat || "0";
	}
	function feedback(message, failed) {
		const box = document.getElementById(failed && document.getElementById("group-dialog") ? "group-dialog-error" : "group-feedback");
		if (!box) return;
		box.textContent = message || box.dataset[failed ? "error" : "success"];
		box.hidden = false;
	}
	function restoreFocus() {
		let element = document.getElementById(returnFocus);
		if (element && !element.getClientRects().length) {
			element = element.closest(".chat-group")?.querySelector(".group-toggle");
		}
		(element || document.getElementById("new-group"))?.focus({ preventScroll: true });
	}
	function closeDialog() {
		const dialog = document.getElementById("group-dialog");
		if (!dialog) return;
		dialog.close();
		dialog.remove();
		restoreFocus();
	}
	function initializeDialog() {
		const dialog = document.getElementById("group-dialog");
		if (!dialog || dialog.open) return;
		dialog.showModal();
		dialog.querySelector("[autofocus]")?.focus();
		dialog.addEventListener("cancel", function (event) {
			event.preventDefault();
			event.stopPropagation();
			closeDialog();
		});
	}
	function rememberSidebar() {
		const list = document.getElementById("chat-list");
		if (list) sidebarScroll = list.scrollTop;
	}
	function restoreSidebar() {
		const list = document.getElementById("chat-list");
		if (list) list.scrollTop = sidebarScroll;
	}
	document.addEventListener("click", function (event) {
		if (event.target.closest("[data-group-cancel]")) closeDialog();
	});
	// Keep Escape within the native modal instead of closing the mobile sidebar.
	document.addEventListener("keydown", function (event) {
		if (event.key === "Escape" && document.getElementById("group-dialog")?.open) {
			event.preventDefault();
			event.stopPropagation();
			closeDialog();
		}
	}, true);
	document.addEventListener("htmx:configRequest", function (event) {
		if (isGroupRequest(event)) event.detail.parameters.current_chat = currentChat();
	});
	document.addEventListener("htmx:beforeRequest", function (event) {
		if (!isGroupRequest(event)) return;
		rememberSidebar();
		const element = source(event);
		if (!element.closest("#group-dialog") && element.id) returnFocus = element.id;
	});
	document.addEventListener("htmx:beforeSwap", function (event) {
		if (event.detail.target?.id === "chat-list") rememberSidebar();
	});
	document.addEventListener("htmx:oobBeforeSwap", function (event) {
		if (event.detail.target?.id === "chat-list") rememberSidebar();
	});
	document.addEventListener("htmx:afterSwap", function (event) {
		if (event.detail.target?.id === "chat-list") restoreSidebar();
		initializeDialog();
	});
	document.addEventListener("htmx:oobAfterSwap", function (event) {
		if (event.detail.target?.id === "chat-list") restoreSidebar();
	});
	document.addEventListener("htmx:afterRequest", function (event) {
		if (!isGroupRequest(event)) return;
		if (!event.detail.successful) {
			feedback(event.detail.xhr.status === 400 || event.detail.xhr.status === 404 ? event.detail.xhr.responseText : "", true);
			return;
		}
		if (event.detail.requestConfig.verb.toLowerCase() === "get") return;
		closeDialog();
		restoreSidebar();
		restoreFocus();
		feedback("", false);
	});
	document.addEventListener("htmx:sendError", function (event) {
		if (isGroupRequest(event)) feedback("", true);
	});
	function clearDrag() {
		dragChat = "";
		document.querySelectorAll(".group-drop-target, .chat-dragging").forEach(function (element) {
			element.classList.remove("group-drop-target", "chat-dragging");
		});
	}
	document.addEventListener("dragstart", function (event) {
		const row = event.target.closest('.chat-item[draggable="true"][data-chat-id]');
		if (!row || !event.dataTransfer) return;
		dragChat = row.dataset.chatId;
		event.dataTransfer.setData(dragType, dragChat);
		event.dataTransfer.effectAllowed = "move";
		row.classList.add("chat-dragging");
	});
	document.addEventListener("dragover", function (event) {
		const target = event.target.closest("[data-group-id]");
		if (!target || !dragChat || !event.dataTransfer?.types.includes(dragType)) return;
		event.preventDefault();
		event.dataTransfer.dropEffect = "move";
		document.querySelectorAll(".group-drop-target").forEach(function (element) {
			if (element !== target) element.classList.remove("group-drop-target");
		});
		target.classList.add("group-drop-target");
	});
	document.addEventListener("dragleave", function (event) {
		const target = event.target.closest("[data-group-id]");
		if (target && !target.contains(event.relatedTarget)) target.classList.remove("group-drop-target");
	});
	document.addEventListener("drop", function (event) {
		const target = event.target.closest("[data-group-id]");
		if (!target || !dragChat || event.dataTransfer?.getData(dragType) !== dragChat) return;
		event.preventDefault();
		const chat = dragChat;
		clearDrag();
		htmx.ajax("POST", "/chats/" + chat + "/group", {
			source: document.getElementById("new-group"),
			target: "#chat-list",
			swap: "outerHTML",
			values: { group_id: target.dataset.groupId, current_chat: currentChat() }
		});
	});
	document.addEventListener("dragend", clearDrag);
}());
