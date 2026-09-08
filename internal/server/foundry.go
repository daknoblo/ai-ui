package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/rag"
	"github.com/daknoblo/ai-ui/internal/storage"
)

var errRefreshRunning = errors.New("deployment refresh is already running")

type deploymentChoice struct {
	Name  string
	Label string
}

type deploymentRow struct {
	Name, Model, Version, State, Support string
}

type deploymentField struct {
	Name, LabelKey, Current, Env, EmptyKey string
	Choices                                []deploymentChoice
	Locked, CurrentAvailable               bool
}

type foundryView struct {
	config.FoundryStatus
	ChatChoices, EmbeddingChoices, ImageChoices, VisionChoices []deploymentChoice
	Deployments                                                []deploymentRow
	Fields                                                     []deploymentField
	UpdatedAt                                                  string
	Index                                                      embeddingIndexView
	ChatFilterActive, ImageFilterActive                        bool
}

type embeddingIndexView struct {
	Enabled, NeedsReindex, CanReindex, Running bool
	Selected, Active, Error, Status, Target    string
	Documents, Chunks, Completed               int
}

func (s *Server) refreshFoundry(ctx context.Context) error {
	if !s.refreshMu.TryLock() {
		return errRefreshRunning
	}
	defer s.refreshMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	snapshot, err := s.cfg.Discover(ctx)
	if err == nil {
		s.configMu.Lock()
		defer s.configMu.Unlock()
	}
	if err == nil && (!strings.EqualFold(snapshot.ResourceID, s.cfg.FoundryStatus().ResourceID) ||
		!foundry.IsV1Endpoint(snapshot.Endpoint)) {
		err = fmt.Errorf("discovery returned an invalid resource or endpoint")
	}
	changed := false
	if err == nil {
		previous, cfg := s.cfg.FoundryStatus().Catalog, s.cfg.Get()
		changed = previous.Endpoint != snapshot.Endpoint
		names := []string{cfg.ChatDeployment, cfg.ChatModel, cfg.EmbeddingDeployment, cfg.ImageDeployment, cfg.VisionDeployment}
		active, known, profileErr := s.store.ActiveEmbeddingProfile(ctx)
		if profileErr != nil {
			err = profileErr
		} else if known {
			names = append(names, active.Deployment)
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			before, had := previous.Find(name)
			after, has := snapshot.Find(name)
			if had != has || !reflect.DeepEqual(before, after) {
				changed = true
			}
		}
	}
	if err == nil {
		err = s.store.SaveCatalog(ctx, snapshot)
	}
	if err == nil {
		err = s.cfg.SetCatalog(snapshot)
	}
	if err != nil {
		s.cfg.SetDiscoveryError(err)
		slog.Warn("deployment refresh failed", "err", err)
		return err
	}
	if changed {
		s.ready.invalidate()
	}
	slog.Info("deployment catalog refreshed", "deployments", len(snapshot.Deployments))
	return nil
}

func (s *Server) handleDeploymentRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.FoundryStatus().Enabled {
		http.Error(w, s.t("foundry.disabled"), http.StatusBadRequest)
		return
	}
	if err := s.refreshFoundry(r.Context()); err != nil {
		s.renderConfigNotice(w, s.t("foundry.refresh_failed", err.Error()), true)
		return
	}
	s.renderConfigNotice(w, s.t("foundry.refreshed"), false)
}

func (s *Server) foundryData(ctx context.Context) foundryView {
	status := s.cfg.FoundryStatus()
	view := foundryView{FoundryStatus: status}
	if !status.Enabled {
		return view
	}
	view.UpdatedAt = formatCheckTime(status.Catalog.RefreshedAt)
	choices := func(op foundry.Operation) []deploymentChoice {
		var choices []deploymentChoice
		for _, name := range status.Catalog.Names(op) {
			deployment, err := s.cfg.ResolveDeployment(op, name)
			if err != nil {
				continue // An explicit environment list may exclude this deployment.
			}
			choices = append(choices, deploymentChoice{
				Name: name, Label: name + " (" + deployment.ModelName + ")",
			})
		}
		return choices
	}
	view.ChatChoices = choices(foundry.Chat)
	view.EmbeddingChoices = choices(foundry.Embeddings)
	view.ImageChoices = choices(foundry.Images)
	view.VisionChoices = choices(foundry.Vision)
	view.ChatFilterActive = len(view.ChatChoices) < len(status.Catalog.Names(foundry.Chat))
	view.ImageFilterActive = len(view.ImageChoices) < len(status.Catalog.Names(foundry.Images))
	cfg, locks := s.cfg.Get(), s.cfg.Locks()
	view.Fields = []deploymentField{
		{Name: "chat_deployment", LabelKey: "foundry.chat", Current: cfg.ChatDeployment, Env: "AZURE_DEPLOYMENT", EmptyKey: "foundry.not_configured", Choices: view.ChatChoices, Locked: locks.ChatDeployment},
		{Name: "embedding_deployment", LabelKey: "foundry.embeddings", Current: cfg.EmbeddingDeployment, Env: "AZURE_EMBEDDING_DEPLOYMENT", EmptyKey: "foundry.not_configured", Choices: view.EmbeddingChoices, Locked: locks.EmbeddingDeployment},
		{Name: "image_deployment", LabelKey: "foundry.images", Current: cfg.ImageDeployment, Env: "AZURE_IMAGE_DEPLOYMENT", EmptyKey: "foundry.not_configured", Choices: view.ImageChoices, Locked: locks.ImageDeployment},
		{Name: "vision_deployment", LabelKey: "foundry.vision", Current: cfg.VisionDeployment, EmptyKey: "foundry.use_chat", Choices: view.VisionChoices},
	}
	for i := range view.Fields {
		for _, choice := range view.Fields[i].Choices {
			if choice.Name == view.Fields[i].Current {
				view.Fields[i].CurrentAvailable = true
			}
		}
	}
	for _, deployment := range status.Catalog.Deployments {
		var support []string
		for _, capability := range []struct {
			op  foundry.Operation
			key string
		}{
			{foundry.Chat, "foundry.chat"}, {foundry.Embeddings, "foundry.embeddings"},
			{foundry.Images, "foundry.images"}, {foundry.ImageEdits, "foundry.image_edits"},
			{foundry.Vision, "foundry.vision"},
		} {
			if deployment.Supports(capability.op) {
				support = append(support, s.t(capability.key))
			}
		}
		supportText := strings.Join(support, ", ")
		if supportText == "" {
			supportText = s.t("foundry.unsupported_detail", deployment.UnsupportedReason(foundry.Chat))
		}
		view.Deployments = append(view.Deployments, deploymentRow{
			Name: deployment.Name, Model: deployment.ModelName, Version: deployment.ModelVersion,
			State: deployment.ProvisioningState, Support: supportText,
		})
	}
	view.Index = s.embeddingIndexData(ctx)
	return view
}

func (s *Server) embeddingIndexData(ctx context.Context) embeddingIndexView {
	view := embeddingIndexView{Enabled: s.cfg.FoundryStatus().Enabled, Selected: s.cfg.Get().EmbeddingDeployment}
	if !view.Enabled {
		return view
	}
	var err error
	view.Documents, view.Chunks, err = s.store.EmbeddingCounts(ctx)
	if err != nil {
		view.Error = s.t("foundry.index_error", err.Error())
		return view
	}
	profile, known, err := s.store.ActiveEmbeddingProfile(ctx)
	if err != nil {
		view.Error = s.t("foundry.index_error", err.Error())
		return view
	}
	if known {
		view.Active = profile.Deployment
	} else {
		view.Active = s.t("foundry.unknown_index")
	}
	if view.Selected != "" {
		target, err := s.llm.ConfiguredEmbeddingProfile()
		if err != nil {
			view.Error = s.t("foundry.index_error", err.Error())
		} else {
			view.Target = embeddingTarget(target)
			view.CanReindex = view.Chunks > 0
			view.NeedsReindex = view.Chunks > 0 && (!known || !profile.SameIdentity(target))
		}
	}
	job, exists, err := s.store.LatestReindex(ctx)
	if err != nil {
		view.Error = s.t("foundry.index_error", err.Error())
		return view
	}
	if exists {
		view.Running = job.Status == "running"
		view.Status = job.Status
		view.Completed = job.CompletedChunks
		if view.Running {
			view.Chunks = job.TotalChunks
		}
		if job.Status == "failed" {
			view.Error = s.t("foundry.reindex_failed", job.Error)
		}
	}
	return view
}

func (s *Server) handleReindexStatus(w http.ResponseWriter, r *http.Request) {
	s.render(w, "embedding-index", s.embeddingIndexData(r.Context()))
}

func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.httpError(w, err)
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.closing || s.ctx.Err() != nil {
		s.httpError(w, context.Canceled)
		return
	}
	if !s.cfg.FoundryStatus().Enabled || r.FormValue("confirm_reindex") != "yes" {
		slog.Info("embedding reindex requires explicit confirmation")
		http.Error(w, s.t("foundry.confirm_required"), http.StatusBadRequest)
		return
	}
	target, err := s.llm.ConfiguredEmbeddingProfile()
	if err != nil {
		s.renderConfigNotice(w, s.t("foundry.index_error", err.Error()), true)
		return
	}
	if r.FormValue("reindex_target") != embeddingTarget(target) {
		s.renderConfigNotice(w, s.t("foundry.selection_changed"), true)
		return
	}
	job, err := s.store.StartReindex(r.Context(), target)
	if err != nil {
		s.renderConfigNotice(w, s.t("foundry.index_error", err.Error()), true)
		return
	}
	s.ready.invalidate()
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		if err := rag.Reindex(s.ctx, s.store, job, s.llm.EmbedProfile); err != nil {
			slog.Warn("embedding reindex failed", "err", err)
		} else {
			slog.Info("embedding reindex completed", "deployment", target.Deployment)
		}
		s.ready.invalidate()
		if s.ctx.Err() == nil {
			s.runChecks(s.ctx, false)
		}
	}()
	s.renderConfigNotice(w, s.t("foundry.reindex_started"), false)
}

func embeddingTarget(profile storage.EmbeddingProfile) string {
	value := strings.Join([]string{profile.ResourceID, profile.Endpoint, profile.Deployment,
		profile.ModelName, profile.ModelVersion}, "\x00")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

// Only a verified empty corpus can acquire its first profile without reindexing.
func (s *Server) verifyActiveEmbedding(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !s.cfg.FoundryStatus().Enabled {
		return s.llm.VerifyEmbedding(ctx)
	}
	target, err := s.llm.ConfiguredEmbeddingProfile()
	if err != nil {
		return err
	}
	active, known, err := s.store.ActiveEmbeddingProfile(ctx)
	if err != nil {
		return err
	}
	_, chunks, err := s.store.EmbeddingCounts(ctx)
	if err != nil {
		return err
	}
	if chunks == 0 && (!known || !active.SameIdentity(target) || active.Dimensions == 0) {
		vectors, err := s.llm.EmbedProfile(ctx, target, []string{"ping"})
		if err != nil {
			return err
		}
		target.Dimensions = len(vectors[0])
		s.configMu.Lock()
		defer s.configMu.Unlock()
		current, err := s.llm.ConfiguredEmbeddingProfile()
		if err != nil {
			return err
		}
		if !current.SameIdentity(target) {
			return fmt.Errorf("embedding selection changed during verification")
		}
		if err := s.store.SetInitialEmbeddingProfile(ctx, target); err != nil {
			return err
		}
		return nil
	}
	if !known {
		return storage.ErrReindexRequired
	}
	return s.store.WithEmbeddingProfile(ctx, func(profile storage.EmbeddingProfile, known bool) error {
		if !known {
			return storage.ErrReindexRequired
		}
		_, err := s.llm.EmbedProfile(ctx, profile, []string{"ping"})
		return err
	})
}
