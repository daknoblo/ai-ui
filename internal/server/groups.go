package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/daknoblo/ai-ui/internal/storage"
)

type chatGroupView struct {
	storage.ChatGroup
	Chats []storage.Chat
}

type chatNavRow struct {
	storage.Chat
	CurrentChat *storage.Chat
}

type sidebarData struct {
	Title       string
	Chats       []storage.Chat
	CurrentChat *storage.Chat
	Groups      []chatGroupView
	Ungrouped   []storage.Chat
}

func (s *Server) buildSidebarData(ctx context.Context, currentID int64) (sidebarData, error) {
	var data sidebarData
	chats, err := s.store.ListChats(ctx)
	if err != nil {
		return data, err
	}
	groups, err := s.store.ListChatGroups(ctx)
	if err != nil {
		return data, err
	}
	data.Chats = chats
	positions := make(map[int64]int, len(groups))
	for _, group := range groups {
		positions[group.ID] = len(data.Groups)
		data.Groups = append(data.Groups, chatGroupView{ChatGroup: group})
	}
	for _, chat := range chats {
		if chat.ID == currentID {
			data.CurrentChat = &chat
			data.Title = chat.Title
		}
		if index, ok := positions[chat.GroupID]; ok {
			data.Groups[index].Chats = append(data.Groups[index].Chats, chat)
		} else {
			data.Ungrouped = append(data.Ungrouped, chat)
		}
	}
	return data, nil
}

type groupDialogData struct {
	Group  storage.ChatGroup
	Chat   *storage.Chat
	Groups []storage.ChatGroup
	Colors []string
}

func (s *Server) groupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		http.Error(w, s.t("group.not_found"), http.StatusNotFound)
	case errors.Is(err, storage.ErrInvalidChatGroup):
		http.Error(w, s.t("group.invalid"), http.StatusBadRequest)
	default:
		s.httpError(w, err)
	}
}

func groupRequestID(r *http.Request) (int64, error) {
	id, err := parseID(r)
	if err != nil || id <= 0 {
		return 0, storage.ErrInvalidChatGroup
	}
	return id, nil
}

func parseGroupForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		return storage.ErrInvalidChatGroup
	}
	// net/http parses DELETE query values, but not its optional form body.
	if r.Method == http.MethodDelete {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return storage.ErrInvalidChatGroup
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return storage.ErrInvalidChatGroup
		}
		for key, values := range values {
			r.Form[key] = values
		}
	}
	if value := r.Form.Get("current_chat"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id < 0 {
			return storage.ErrInvalidChatGroup
		}
	}
	return nil
}

func (s *Server) renderGroupSidebar(w http.ResponseWriter, r *http.Request) {
	// current_chat selects the active row only; it never changes conversation
	// state or starts/stops the independent generation worker.
	currentID, _ := strconv.ParseInt(r.Form.Get("current_chat"), 10, 64)
	data, err := s.buildSidebarData(r.Context(), currentID)
	if err != nil {
		s.groupError(w, err)
		return
	}
	s.render(w, "chatnav", data)
}

func (s *Server) handleNewChatGroup(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "group-dialog", groupDialogData{Colors: storage.ChatGroupColors()})
}

func (s *Server) handleEditChatGroup(w http.ResponseWriter, r *http.Request) {
	id, err := groupRequestID(r)
	if err != nil {
		s.groupError(w, err)
		return
	}
	group, err := s.store.GetChatGroup(r.Context(), id)
	if err != nil {
		s.groupError(w, err)
		return
	}
	s.render(w, "group-dialog", groupDialogData{Group: group, Colors: storage.ChatGroupColors()})
}

func (s *Server) handleCreateChatGroup(w http.ResponseWriter, r *http.Request) {
	if err := parseGroupForm(w, r); err != nil {
		s.groupError(w, err)
		return
	}
	if _, err := s.store.CreateChatGroup(r.Context(), r.PostForm.Get("title"), r.PostForm.Get("color")); err != nil {
		s.groupError(w, err)
		return
	}
	s.renderGroupSidebar(w, r)
}

func (s *Server) handleUpdateChatGroup(w http.ResponseWriter, r *http.Request) {
	id, err := groupRequestID(r)
	if err == nil {
		err = parseGroupForm(w, r)
	}
	if err == nil {
		err = s.store.UpdateChatGroup(r.Context(), id, r.PostForm.Get("title"), r.PostForm.Get("color"))
	}
	if err != nil {
		s.groupError(w, err)
		return
	}
	s.renderGroupSidebar(w, r)
}

func (s *Server) handleCollapseChatGroup(w http.ResponseWriter, r *http.Request) {
	id, err := groupRequestID(r)
	if err == nil {
		err = parseGroupForm(w, r)
	}
	collapsed := r.PostForm.Get("collapsed")
	if err == nil && collapsed != "true" && collapsed != "false" {
		err = storage.ErrInvalidChatGroup
	}
	if err == nil {
		err = s.store.SetChatGroupCollapsed(r.Context(), id, collapsed == "true")
	}
	if err != nil {
		s.groupError(w, err)
		return
	}
	s.renderGroupSidebar(w, r)
}

func (s *Server) handleDeleteChatGroup(w http.ResponseWriter, r *http.Request) {
	id, err := groupRequestID(r)
	if err == nil {
		err = parseGroupForm(w, r)
	}
	if err == nil {
		err = s.store.DeleteChatGroup(r.Context(), id)
	}
	if err != nil {
		s.groupError(w, err)
		return
	}
	s.renderGroupSidebar(w, r)
}

func (s *Server) handleMoveChatDialog(w http.ResponseWriter, r *http.Request) {
	id, err := groupRequestID(r)
	if err != nil {
		s.groupError(w, err)
		return
	}
	chat, err := s.store.GetChat(r.Context(), id)
	if err != nil {
		s.groupError(w, err)
		return
	}
	groups, err := s.store.ListChatGroups(r.Context())
	if err != nil {
		s.groupError(w, err)
		return
	}
	s.render(w, "group-dialog", groupDialogData{Chat: &chat, Groups: groups})
}

func (s *Server) handleMoveChatGroup(w http.ResponseWriter, r *http.Request) {
	id, err := groupRequestID(r)
	if err == nil {
		err = parseGroupForm(w, r)
	}
	var groupID int64
	if err == nil {
		groupID, err = strconv.ParseInt(r.PostForm.Get("group_id"), 10, 64)
		if err != nil || groupID < 0 {
			err = storage.ErrInvalidChatGroup
		}
	}
	if err == nil {
		err = s.store.MoveChatToGroup(r.Context(), id, groupID)
	}
	if err != nil {
		s.groupError(w, err)
		return
	}
	s.renderGroupSidebar(w, r)
}
