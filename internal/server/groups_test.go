package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/storage"
)

func groupRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	return result
}

func TestChatGroupHandlers(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			s, handler := newTestServer(t, language)
			chat, err := s.store.CreateChat(t.Context(), "Active chat", "", "auto")
			if err != nil {
				t.Fatal(err)
			}
			turn, err := s.store.CreateGeneration(t.Context(), chat, "Question", "{}", time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			current := fmt.Sprintf("current_chat=%d", chat)
			checkSidebar := func(result *httptest.ResponseRecorder) {
				t.Helper()
				body := result.Body.String()
				if result.Code != http.StatusOK || !strings.Contains(body, `<nav id="chat-list"`) ||
					!strings.Contains(body, `chat-item active`) || strings.Contains(body, `id="messages"`) ||
					result.Header().Get("HX-Redirect") != "" {
					t.Fatalf("sidebar response = %d: %s", result.Code, body)
				}
			}
			checkSidebar(groupRequest(handler, http.MethodPost, "/groups", "title=Work&color=blue&"+current))
			for _, path := range []string{"/groups/new", "/groups/1/edit", fmt.Sprintf("/chats/%d/move", chat)} {
				result := groupRequest(handler, http.MethodGet, path, "")
				if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `<dialog id="group-dialog"`) {
					t.Fatalf("dialog %s = %d: %s", path, result.Code, result.Body.String())
				}
			}
			checkSidebar(groupRequest(handler, http.MethodPost, fmt.Sprintf("/chats/%d/group", chat), "group_id=1&"+current))
			checkSidebar(groupRequest(handler, http.MethodPost, "/groups/1", "title="+url.QueryEscape(`<script>alert("x")</script>`)+"&color=rose&"+current))
			collapsed := groupRequest(handler, http.MethodPost, "/groups/1/collapse", "collapsed=true&"+current)
			checkSidebar(collapsed)
			if !strings.Contains(collapsed.Body.String(), `aria-expanded="false"`) || strings.Contains(collapsed.Body.String(), `<script>alert`) {
				t.Fatalf("unsafe group or missing collapse: %s", collapsed.Body.String())
			}
			page := groupRequest(handler, http.MethodGet, fmt.Sprintf("/chat/%d", chat), "")
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `class="group-children" hidden`) {
				t.Fatalf("collapse not persisted: %d", page.Code)
			}
			checkSidebar(groupRequest(handler, http.MethodPost, "/groups/1/collapse", "collapsed=false&"+current))
			checkSidebar(groupRequest(handler, http.MethodDelete, "/groups/1", current))
			got, err := s.store.GetChat(t.Context(), chat)
			if err != nil || got.GroupID != 0 {
				t.Fatalf("chat removed with group: %+v, %v", got, err)
			}
			generation, err := s.store.GetGeneration(t.Context(), chat, turn.ID)
			if err != nil || generation.State != storage.GenerationPending {
				t.Fatalf("group action disturbed generation: %+v, %v", generation, err)
			}
		})
	}
}

func TestChatGroupHandlerErrors(t *testing.T) {
	_, handler := newTestServer(t, "en")
	cases := []struct {
		method, path, form string
		status             int
	}{
		{"POST", "/groups", "title=+&color=blue", 400},
		{"POST", "/groups", "title=" + strings.Repeat("x", 81), 400},
		{"POST", "/groups", "title=ok&color=chartreuse", 400},
		{"POST", "/groups", "title=ok&current_chat=-1", 400},
		{"POST", "/groups", "title=" + strings.Repeat("x", 5000), 400},
		{"POST", "/groups/nope", "title=ok", 400},
		{"POST", "/groups/0", "title=ok", 400},
		{"POST", "/groups/99", "title=ok", 404},
		{"GET", "/groups/99/edit", "", 404},
		{"DELETE", "/groups/99", "", 404},
		{"POST", "/groups/99/collapse", "collapsed=other", 400},
		{"POST", "/groups/99/collapse", "collapsed=true", 404},
		{"GET", "/chats/99/move", "", 404},
		{"POST", "/chats/99/group", "group_id=0", 404},
		{"POST", "/chats/99/group", "group_id=-1", 400},
		{"POST", "/chats/99/group", "group_id=nope", 400},
		{"POST", "/chats/99/group", "", 400},
	}
	for _, tc := range cases {
		result := groupRequest(handler, tc.method, tc.path, tc.form)
		if result.Code != tc.status {
			t.Errorf("%s %s %q = %d, want %d: %s", tc.method, tc.path, tc.form, result.Code, tc.status, result.Body.String())
		}
	}
}

func TestChatGroupSidebarTitleAndOrdering(t *testing.T) {
	s, handler := newTestServer(t, "en")
	ctx := t.Context()
	group, err := s.store.CreateChatGroup(ctx, "Project", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.store.CreateChat(ctx, "First", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.store.CreateChat(ctx, "Second", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	for _, chat := range []int64{first, second} {
		if err := s.store.MoveChatToGroup(ctx, chat, group); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.UpdateChatTitle(ctx, first, "Latest title"); err != nil {
		t.Fatal(err)
	}
	data, err := s.buildSidebarData(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	body := s.renderString("title-update", data)
	if !strings.Contains(body, "Latest title") || !strings.Contains(body, `data-group-id="1"`) ||
		!strings.Contains(body, `chat-item active`) || len(data.Groups) != 1 || len(data.Groups[0].Chats) != 2 {
		t.Fatalf("title sidebar: %s", body)
	}
	all, err := s.store.ListChats(ctx)
	if err != nil || data.Groups[0].Chats[0].ID != all[0].ID {
		t.Fatalf("group order differs from latest chat order: %v", err)
	}
	result := groupRequest(handler, http.MethodPost, "/chats", "")
	if result.Code != http.StatusOK {
		t.Fatal(result.Code)
	}
	newChat, err := s.store.GetChat(ctx, second+1)
	if err != nil || newChat.GroupID != 0 {
		t.Fatalf("new chat should be ungrouped: %+v, %v", newChat, err)
	}
}
