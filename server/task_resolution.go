package server

import (
	"fmt"
	"net/http"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/subscription"
)

type taskLLMResolution struct {
	ProfileID *int64 `json:"profile_id,omitempty"`
	Name      string `json:"name"`
	Format    string `json:"format"`
	Model     string `json:"model"`
	Source    string `json:"source"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// nativeProfileSelectionError preserves the selected-profile boundary around
// providerForProfile's legacy bool result. A missing or unusable API-key profile
// keeps the historical fallback behavior; a selected OAuth profile must remain
// selected and surface a terminal native error instead.
func (s *Server) nativeProfileSelectionError(profileID int64) error {
	if s == nil || s.m == nil || s.m.pg == nil {
		return fmt.Errorf("load selected LLM profile #%d: database unavailable", profileID)
	}
	profile, err := s.m.pg.ProfileByID(profileID)
	if err != nil {
		return fmt.Errorf("load selected LLM profile #%d: %w", profileID, err)
	}
	if profile == nil || profile.AuthMode != "oauth" {
		return nil
	}
	return &subscription.Error{
		Code:    subscription.CodeAuthRequired,
		Message: fmt.Sprintf("selected native LLM profile #%d is unavailable", profileID),
	}
}

func (s *Server) resolutionFromProfile(p *db.LLMProfile, source string) taskLLMResolution {
	if p == nil {
		return taskLLMResolution{Source: source, Reason: "LLM profile does not exist"}
	}
	id := p.ID
	result := taskLLMResolution{
		ProfileID: &id,
		Name:      p.Name,
		Format:    p.Format,
		Model:     p.Model,
		Source:    source,
	}
	// OAuth and Ollama profiles are keyless by design. Legacy API profiles keep
	// the existing missing-key fallback semantics.
	if p.AuthMode != "oauth" && p.APIKey == "" && p.Format != "ollama" {
		result.Reason = "LLM profile has no API Key set"
		return result
	}
	if _, _, ok := s.providerForProfile(p.ID); !ok {
		result.Reason = "LLM profile format or parameters are invalid"
		return result
	}
	result.Available = true
	return result
}

// resolveTaskRoleLLM mirrors taskLLMRuntime.current without creating a provider:
// role Agent binding -> explicit task chain -> active/global environment config.
// Database failures are returned rather than represented as an unavailable model.
func (s *Server) resolveTaskRoleLLM(t *Task, agentKey string) (taskLLMResolution, error) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return taskLLMResolution{}, fmt.Errorf("database unavailable")
	}
	a, err := s.m.pg.GetAgentByKey(agentKey)
	if err != nil {
		return taskLLMResolution{}, fmt.Errorf("load agent %q: %w", agentKey, err)
	}
	if a != nil && a.LLMProfileID != nil {
		p, err := s.m.pg.ProfileByID(*a.LLMProfileID)
		if err != nil {
			return taskLLMResolution{}, fmt.Errorf("load agent LLM profile: %w", err)
		}
		if p != nil {
			resolved := s.resolutionFromProfile(p, "agent_binding")
			if resolved.Available {
				return resolved, nil
			}
			if selectionErr := s.nativeProfileSelectionError(p.ID); selectionErr != nil {
				return resolved, selectionErr
			}
		}
	}
	state := t.llmStateSnapshot()
	if len(state.ProfileIDs) > 0 {
		if state.ActiveID == nil {
			return taskLLMResolution{Source: "task_chain", Reason: "task LLM profile chain quota exhausted"}, nil
		}
		p, err := s.m.pg.ProfileByID(*state.ActiveID)
		if err != nil {
			return taskLLMResolution{}, fmt.Errorf("load task LLM profile: %w", err)
		}
		resolved := s.resolutionFromProfile(p, "task_chain")
		if selectionErr := s.nativeProfileSelectionError(*state.ActiveID); !resolved.Available && selectionErr != nil {
			return resolved, selectionErr
		}
		return resolved, nil
	}
	p, err := s.m.pg.ActiveProfile()
	if err != nil {
		return taskLLMResolution{}, fmt.Errorf("load global LLM profile: %w", err)
	}
	if p != nil {
		resolved := s.resolutionFromProfile(p, "global_profile")
		if resolved.Available {
			return resolved, nil
		}
		if selectionErr := s.nativeProfileSelectionError(p.ID); selectionErr != nil {
			return resolved, selectionErr
		}
	}
	s.cfgMu.Lock()
	on, name, cfg := s.llmOn, s.llmProf, s.llmCfg
	s.cfgMu.Unlock()
	if on {
		if name == "" {
			name = "global configuration"
		}
		return taskLLMResolution{
			Name: name, Format: cfg.Provider(), Model: cfg.Model,
			Source: "environment", Available: true,
		}, nil
	}
	return taskLLMResolution{Source: "global", Reason: "no available LLM profile"}, nil
}

func (s *Server) taskLLMResolutionHandler(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	roles := map[string]taskLLMResolution{}
	for responseKey, agentKey := range map[string]string{
		"mainagent": "mainagent",
		"planner":   "planner",
		"worker":    "worker",
	} {
		resolved, err := s.resolveTaskRoleLLM(t, agentKey)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		roles[responseKey] = resolved
	}
	writeJSON(w, http.StatusOK, roles)
}
