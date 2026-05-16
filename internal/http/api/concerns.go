package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/zenocy/zeno-v2/internal/idgen"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// ConcernsHandler answers the /api/concerns/* surface. Phase 1 ships CRUD,
// state transition, merge, split, and tag management. Phase 2 adds the
// /approve and /dismiss shortcuts (recognition pipeline) and routes
// user-declared concerns through synth.Ask. Phase 4 wraps these endpoints
// in the review surface UI.
type ConcernsHandler struct {
	Concerns     *store.ConcernRepo
	Observations *store.ConcernObservationRepo
	Bus          *eventbus.Bus
	EventLog     logp.Writer
	Now          func() time.Time
	Log          *logrus.Entry

	// Dispatcher is the V2.5.0 Phase 2 retrospective dispatcher. /approve
	// fires it after a successful proposed→active transition. nil → no
	// retrospective is dispatched (the concern is approved, but no
	// historical tagging will run); the DTO surfaces this so the UI can
	// reflect the difference.
	Dispatcher synth.RetrospectiveDispatcher

	// AutoRetireDays gates the DTO's `ready_to_retire` flag. Default 90
	// when 0 — kept aligned with projection.DefaultAutoRetireDays and
	// the recognition retirement survey threshold so all three paths
	// agree on what "inactive" means.
	AutoRetireDays int
}

// Limits and validation constants.
const (
	concernNameMaxLen        = 80
	concernDescriptionMaxLen = 600
	concernListLimit         = 200
	concernTagBatchLimit     = 256
)

// Register attaches every concern route to the Echo instance.
func (h *ConcernsHandler) Register(e *echo.Echo) {
	e.GET("/api/concerns", h.list)
	e.POST("/api/concerns", h.create)
	e.GET("/api/concerns/:id", h.get)
	e.PATCH("/api/concerns/:id", h.patch)
	e.POST("/api/concerns/:id/state", h.transition)
	e.POST("/api/concerns/:id/approve", h.approve)
	e.POST("/api/concerns/:id/dismiss", h.dismiss)
	e.POST("/api/concerns/:id/merge", h.merge)
	e.POST("/api/concerns/:id/split", h.split)
	e.GET("/api/concerns/:id/observations", h.listObservations)
	e.POST("/api/concerns/:id/tags", h.addTags)
	e.DELETE("/api/concerns/:id/tags/:event_id", h.removeTag)
}

// concernDTO is the JSON wire shape returned to clients.
type concernDTO struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Description      string     `json:"description"`
	State            string     `json:"state"`
	Source           string     `json:"source"`
	Confidence       float64    `json:"confidence"`
	MergedIntoID     *string    `json:"merged_into_id,omitempty"`
	SplitFromID      *string    `json:"split_from_id,omitempty"`
	LastActiveAt     time.Time  `json:"last_active_at"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	ObservationCount int64      `json:"observation_count"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	// ReadyToRetire is computed on the fly from LastActiveAt against the
	// handler's AutoRetireDays threshold. Only `active` concerns can be
	// "ready to retire". The UI surfaces a calm note ("quiet for N days
	// — ready to retire?") on the row; the user retires by ending the
	// concern through the existing /state endpoint.
	ReadyToRetire bool `json:"ready_to_retire,omitempty"`
}

type listResp struct {
	Concerns []concernDTO `json:"concerns"`
}

type createReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"` // "model" | "user"; defaults to "user" via API
}

type patchReq struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

type transitionReq struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type mergeReq struct {
	IntoID string `json:"into_id"`
}

type splitItem struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	ObservationIDs []string `json:"observation_ids"`
}

type splitReq struct {
	Splits []splitItem `json:"splits"`
}

type tagsReq struct {
	EventIDs []string `json:"event_ids"`
	Source   string   `json:"source,omitempty"` // defaults "user"
}

func (h *ConcernsHandler) list(c echo.Context) error {
	state := strings.ToLower(strings.TrimSpace(c.QueryParam("state")))
	ctx := c.Request().Context()

	var rows []store.Concern
	var err error
	if state == "" {
		rows, err = h.Concerns.ListAll(ctx)
	} else if !store.IsValidConcernState(state) {
		return BadRequest(c, "unknown state filter")
	} else {
		rows, err = h.Concerns.ListByState(ctx, state)
	}
	if err != nil {
		return h.serverErr(c, "concerns list failed", err)
	}
	if len(rows) > concernListLimit {
		rows = rows[:concernListLimit]
	}

	out := listResp{Concerns: make([]concernDTO, 0, len(rows))}
	for _, r := range rows {
		dto, err := h.toDTO(ctx, r)
		if err != nil {
			return h.serverErr(c, "concern dto failed", err)
		}
		out.Concerns = append(out.Concerns, dto)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ConcernsHandler) create(c echo.Context) error {
	var req createReq
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	name := strings.TrimSpace(req.Name)
	desc := strings.TrimSpace(req.Description)
	source := strings.ToLower(strings.TrimSpace(req.Source))
	if source == "" {
		source = store.ConcernSourceUser
	}
	if name == "" {
		return BadRequest(c, "name is required")
	}
	if len(name) > concernNameMaxLen {
		return BadRequest(c, "name is too long")
	}
	if desc == "" {
		return BadRequest(c, "description is required")
	}
	if len(desc) > concernDescriptionMaxLen {
		return BadRequest(c, "description is too long")
	}
	if source != store.ConcernSourceUser && source != store.ConcernSourceModel {
		return BadRequest(c, "source must be 'user' or 'model'")
	}

	ctx := c.Request().Context()
	norm := store.NormalizeConcernName(name)
	existing, err := h.Concerns.GetByNormName(ctx, norm, true)
	if err != nil {
		return h.serverErr(c, "concern lookup failed", err)
	}
	if existing != nil && !existing.DeletedAt.Valid {
		return c.JSON(http.StatusConflict, map[string]string{
			"error": "a concern with this name already exists",
			"id":    existing.ID,
		})
	}

	now := h.now()
	state := store.ConcernStateProposed
	confidence := 0.0
	if source == store.ConcernSourceUser {
		// User-declared auto-promotes to active. Phase 2 routes Ask declarations
		// through this same path. The Phase 1 test surface uses it to set up
		// fixtures.
		state = store.ConcernStateActive
		confidence = 1.0
	}
	row := store.Concern{
		ID:           idgen.New(),
		Name:         name,
		NormName:     norm,
		Description:  desc,
		State:        state,
		Source:       source,
		Confidence:   confidence,
		LastActiveAt: now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := h.Concerns.Insert(ctx, row); err != nil {
		return h.serverErr(c, "concern insert failed", err)
	}

	h.audit(ctx, logp.KindConcernCreated, map[string]any{
		"id":     row.ID,
		"name":   row.Name,
		"state":  row.State,
		"source": row.Source,
	})
	if h.Bus != nil && state == store.ConcernStateProposed {
		h.Bus.Publish(eventbus.ConcernProposedEvent{
			ConcernID:   row.ID,
			Name:        row.Name,
			Description: row.Description,
			Source:      row.Source,
			Confidence:  row.Confidence,
		})
	}

	dto, err := h.toDTO(ctx, row)
	if err != nil {
		return h.serverErr(c, "concern dto failed", err)
	}
	return c.JSON(http.StatusCreated, dto)
}

func (h *ConcernsHandler) get(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	row, err := h.Concerns.GetByID(c.Request().Context(), id)
	if err != nil {
		return h.serverErr(c, "concern get failed", err)
	}
	if row == nil {
		return NotFound(c, "concern not found")
	}
	dto, err := h.toDTO(c.Request().Context(), *row)
	if err != nil {
		return h.serverErr(c, "concern dto failed", err)
	}
	return c.JSON(http.StatusOK, dto)
}

func (h *ConcernsHandler) patch(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	var req patchReq
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if req.Name == nil && req.Description == nil {
		return BadRequest(c, "at least one of name or description must be provided")
	}
	ctx := c.Request().Context()
	row, err := h.Concerns.GetByID(ctx, id)
	if err != nil {
		return h.serverErr(c, "concern lookup failed", err)
	}
	if row == nil {
		return NotFound(c, "concern not found")
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return BadRequest(c, "name must be non-empty")
		}
		if len(name) > concernNameMaxLen {
			return BadRequest(c, "name is too long")
		}
		// Check the new norm doesn't collide with another visible concern.
		newNorm := store.NormalizeConcernName(name)
		if newNorm != row.NormName {
			collide, err := h.Concerns.GetByNormName(ctx, newNorm, false)
			if err != nil {
				return h.serverErr(c, "concern norm lookup failed", err)
			}
			if collide != nil && collide.ID != id {
				return c.JSON(http.StatusConflict, map[string]string{
					"error": "another concern already has this name",
					"id":    collide.ID,
				})
			}
		}
		if err := h.Concerns.UpdateName(ctx, id, name); err != nil {
			return h.serverErr(c, "concern rename failed", err)
		}
		h.audit(ctx, logp.KindConcernRenamed, map[string]any{
			"id":       id,
			"old_name": row.Name,
			"new_name": name,
		})
	}
	if req.Description != nil {
		desc := strings.TrimSpace(*req.Description)
		if desc == "" {
			return BadRequest(c, "description must be non-empty")
		}
		if len(desc) > concernDescriptionMaxLen {
			return BadRequest(c, "description is too long")
		}
		if err := h.Concerns.UpdateDescription(ctx, id, desc); err != nil {
			return h.serverErr(c, "concern description update failed", err)
		}
	}
	updated, err := h.Concerns.GetByID(ctx, id)
	if err != nil || updated == nil {
		return h.serverErr(c, "concern reread failed", err)
	}
	dto, err := h.toDTO(ctx, *updated)
	if err != nil {
		return h.serverErr(c, "concern dto failed", err)
	}
	return c.JSON(http.StatusOK, dto)
}

func (h *ConcernsHandler) transition(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	var req transitionReq
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	target := strings.ToLower(strings.TrimSpace(req.State))
	if !store.IsValidConcernState(target) {
		return BadRequest(c, "unknown target state")
	}
	if target == store.ConcernStateMerged {
		return BadRequest(c, "use POST /merge to transition to merged")
	}
	ctx := c.Request().Context()
	row, err := h.Concerns.GetByID(ctx, id)
	if err != nil {
		return h.serverErr(c, "concern lookup failed", err)
	}
	if row == nil {
		return NotFound(c, "concern not found")
	}
	prior := row.State
	if err := h.Concerns.Transition(ctx, id, target); err != nil {
		if errors.Is(err, store.ErrInvalidConcernTransition) {
			return Conflict(c, err.Error())
		}
		if errors.Is(err, store.ErrConcernNotFound) {
			return NotFound(c, "concern not found")
		}
		return h.serverErr(c, "concern transition failed", err)
	}
	h.audit(ctx, logp.KindConcernStateChanged, map[string]any{
		"id":          id,
		"prior_state": prior,
		"new_state":   target,
		"reason":      req.Reason,
	})
	if h.Bus != nil {
		h.Bus.Publish(eventbus.ConcernStateChangedEvent{
			ConcernID:  id,
			PriorState: prior,
			NewState:   target,
		})
	}
	updated, _ := h.Concerns.GetByID(ctx, id)
	if updated == nil {
		return c.NoContent(http.StatusOK)
	}
	dto, _ := h.toDTO(ctx, *updated)
	return c.JSON(http.StatusOK, dto)
}

// approve transitions a proposed concern to active and (when configured)
// dispatches retrospective tagging. Convenience over POST /state — the
// review surface and CLI use this so the action emits a stable
// `concern.approved` audit kind without callers having to know the
// state-machine wording.
//
// Errors:
//
//	400 — id missing
//	404 — no such concern
//	409 — concern is not in `proposed`; only proposed concerns can be approved
func (h *ConcernsHandler) approve(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	ctx := c.Request().Context()
	row, err := h.Concerns.GetByID(ctx, id)
	if err != nil {
		return h.serverErr(c, "concern lookup failed", err)
	}
	if row == nil {
		return NotFound(c, "concern not found")
	}
	if row.State != store.ConcernStateProposed {
		return c.JSON(http.StatusConflict, map[string]string{
			"error": "only proposed concerns can be approved",
			"state": row.State,
		})
	}
	if err := h.Concerns.Transition(ctx, id, store.ConcernStateActive); err != nil {
		if errors.Is(err, store.ErrInvalidConcernTransition) {
			return Conflict(c, err.Error())
		}
		return h.serverErr(c, "approve transition failed", err)
	}
	h.audit(ctx, logp.KindConcernApproved, map[string]any{
		"id":          id,
		"prior_state": store.ConcernStateProposed,
		"new_state":   store.ConcernStateActive,
	})
	if h.Bus != nil {
		h.Bus.Publish(eventbus.ConcernStateChangedEvent{
			ConcernID:  id,
			PriorState: store.ConcernStateProposed,
			NewState:   store.ConcernStateActive,
		})
	}
	retroDispatched := false
	if h.Dispatcher != nil {
		h.Dispatcher.Dispatch(id)
		retroDispatched = true
	}
	updated, _ := h.Concerns.GetByID(ctx, id)
	if updated == nil {
		return c.JSON(http.StatusOK, map[string]any{"approved": true, "retrospective_dispatched": retroDispatched})
	}
	dto, _ := h.toDTO(ctx, *updated)
	return c.JSON(http.StatusOK, map[string]any{
		"concern":                  dto,
		"retrospective_dispatched": retroDispatched,
	})
}

// dismiss soft-deletes a proposed concern. The soft-delete row keeps the
// audit trail and seeds the 90-day denylist that recognition consults
// before re-proposing the same normalized name. dismiss is the canonical
// action a user takes on a noisy proposal; the review surface uses
// this and so will the CLI.
//
// Errors:
//
//	400 — id missing
//	404 — no such concern
//	409 — concern is not in `proposed`; ended/active concerns must use /state to end
func (h *ConcernsHandler) dismiss(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	ctx := c.Request().Context()
	row, err := h.Concerns.GetByID(ctx, id)
	if err != nil {
		return h.serverErr(c, "concern lookup failed", err)
	}
	if row == nil {
		return NotFound(c, "concern not found")
	}
	if row.State != store.ConcernStateProposed {
		return c.JSON(http.StatusConflict, map[string]string{
			"error": "only proposed concerns can be dismissed",
			"state": row.State,
		})
	}
	if err := h.Concerns.SoftDelete(ctx, id); err != nil {
		return h.serverErr(c, "concern soft-delete failed", err)
	}
	h.audit(ctx, logp.KindConcernDismissed, map[string]any{
		"id":        id,
		"name":      row.Name,
		"norm_name": row.NormName,
	})
	return c.JSON(http.StatusOK, map[string]any{
		"dismissed": true,
		"id":        id,
		"name":      row.Name,
	})
}

func (h *ConcernsHandler) merge(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	var req mergeReq
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	tgt := strings.TrimSpace(req.IntoID)
	if tgt == "" {
		return BadRequest(c, "into_id is required")
	}
	ctx := c.Request().Context()
	if err := h.Concerns.Merge(ctx, id, tgt); err != nil {
		if errors.Is(err, store.ErrInvalidConcernTransition) {
			return Conflict(c, err.Error())
		}
		if errors.Is(err, store.ErrConcernNotFound) {
			return NotFound(c, err.Error())
		}
		return h.serverErr(c, "concern merge failed", err)
	}
	moved, err := h.Observations.ReassignToConcern(ctx, id, tgt)
	if err != nil {
		return h.serverErr(c, "concern observation reassign failed", err)
	}
	h.audit(ctx, logp.KindConcernMerged, map[string]any{
		"source_id":    id,
		"target_id":    tgt,
		"observations": moved,
	})
	if h.Bus != nil {
		t := tgt
		h.Bus.Publish(eventbus.ConcernStateChangedEvent{
			ConcernID:    id,
			PriorState:   store.ConcernStateActive,
			NewState:     store.ConcernStateMerged,
			MergedIntoID: &t,
		})
	}
	row, _ := h.Concerns.GetByID(ctx, id)
	if row == nil {
		return c.NoContent(http.StatusOK)
	}
	dto, _ := h.toDTO(ctx, *row)
	return c.JSON(http.StatusOK, dto)
}

func (h *ConcernsHandler) split(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	var req splitReq
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if len(req.Splits) < 2 {
		return BadRequest(c, "split requires at least two new concerns")
	}
	ctx := c.Request().Context()
	src, err := h.Concerns.GetByID(ctx, id)
	if err != nil {
		return h.serverErr(c, "concern lookup failed", err)
	}
	if src == nil {
		return NotFound(c, "concern not found")
	}
	if src.State != store.ConcernStateActive && src.State != store.ConcernStatePaused {
		return Conflict(c, "split requires active or paused source")
	}

	// Validate split items and pre-compute IDs + the assignment map.
	now := h.now()
	newRows := make([]store.Concern, 0, len(req.Splits))
	assignment := map[string]string{}
	for i, item := range req.Splits {
		name := strings.TrimSpace(item.Name)
		desc := strings.TrimSpace(item.Description)
		if name == "" || desc == "" {
			return BadRequest(c, "each split needs name and description")
		}
		if len(name) > concernNameMaxLen || len(desc) > concernDescriptionMaxLen {
			return BadRequest(c, "split field too long")
		}
		newID := idgen.New()
		newRows = append(newRows, store.Concern{
			ID:           newID,
			Name:         name,
			NormName:     store.NormalizeConcernName(name),
			Description:  desc,
			State:        store.ConcernStateActive,
			Source:       store.ConcernSourceUser,
			Confidence:   1.0,
			SplitFromID:  ptr(id),
			LastActiveAt: now,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
		for _, evID := range item.ObservationIDs {
			if evID == "" {
				continue
			}
			if _, exists := assignment[evID]; exists {
				return c.JSON(http.StatusBadRequest, map[string]string{
					"error":          "observation assigned to two splits",
					"event_id":       evID,
					"first_split":    assignment[evID],
					"conflict_split": newID,
					"item_index":     intToString(i),
				})
			}
			assignment[evID] = newID
		}
	}
	for _, n := range newRows {
		if err := h.Concerns.Insert(ctx, n); err != nil {
			return h.serverErr(c, "split concern insert failed", err)
		}
	}
	moved, err := h.Observations.PartitionToConcerns(ctx, id, assignment)
	if err != nil {
		return h.serverErr(c, "split observation partition failed", err)
	}
	if err := h.Concerns.Transition(ctx, id, store.ConcernStateEnded); err != nil {
		return h.serverErr(c, "split end-source failed", err)
	}
	h.audit(ctx, logp.KindConcernSplit, map[string]any{
		"source_id":  id,
		"new_ids":    idList(newRows),
		"moved":      moved,
		"split_size": len(newRows),
	})
	type splitResp struct {
		Source concernDTO   `json:"source"`
		New    []concernDTO `json:"new"`
	}
	srcAfter, _ := h.Concerns.GetByID(ctx, id)
	srcDTO, _ := h.toDTO(ctx, *srcAfter)
	out := splitResp{Source: srcDTO, New: make([]concernDTO, 0, len(newRows))}
	for _, n := range newRows {
		row, _ := h.Concerns.GetByID(ctx, n.ID)
		if row == nil {
			continue
		}
		dto, _ := h.toDTO(ctx, *row)
		out.New = append(out.New, dto)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ConcernsHandler) listObservations(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	rows, err := h.Observations.ListByConcern(c.Request().Context(), id, 0)
	if err != nil {
		return h.serverErr(c, "concern observations list failed", err)
	}
	type tagDTO struct {
		EventID    string    `json:"event_id"`
		Source     string    `json:"source"`
		Confidence float64   `json:"confidence"`
		TaggedAt   time.Time `json:"tagged_at"`
	}
	out := make([]tagDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, tagDTO{
			EventID:    r.EventID,
			Source:     r.Source,
			Confidence: r.Confidence,
			TaggedAt:   r.TaggedAt,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"observations": out})
}

func (h *ConcernsHandler) addTags(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	var req tagsReq
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if len(req.EventIDs) == 0 {
		return BadRequest(c, "event_ids is required")
	}
	if len(req.EventIDs) > concernTagBatchLimit {
		return BadRequest(c, "too many event_ids in one call")
	}
	source := strings.ToLower(strings.TrimSpace(req.Source))
	if source == "" {
		source = store.ConcernTagSourceUser
	}
	if source != store.ConcernTagSourceUser && source != store.ConcernTagSourceModel {
		return BadRequest(c, "source must be 'user' or 'model'")
	}
	ctx := c.Request().Context()
	row, err := h.Concerns.GetByID(ctx, id)
	if err != nil {
		return h.serverErr(c, "concern lookup failed", err)
	}
	if row == nil {
		return NotFound(c, "concern not found")
	}
	now := h.now()
	tags := make([]store.ConcernObservation, 0, len(req.EventIDs))
	for _, evID := range req.EventIDs {
		evID = strings.TrimSpace(evID)
		if evID == "" {
			continue
		}
		tags = append(tags, store.ConcernObservation{
			ConcernID:  id,
			EventID:    evID,
			Source:     source,
			Confidence: 1.0,
			TaggedAt:   now,
		})
	}
	if err := h.Observations.TagBatch(ctx, tags); err != nil {
		return h.serverErr(c, "concern tag batch failed", err)
	}
	if err := h.Concerns.BumpLastActive(ctx, id, now); err != nil {
		return h.serverErr(c, "concern bump last_active_at failed", err)
	}
	evIDs := make([]string, len(tags))
	for i, t := range tags {
		evIDs[i] = t.EventID
	}
	h.audit(ctx, logp.KindConcernTagged, map[string]any{
		"concern_id": id,
		"event_ids":  evIDs,
		"source":     source,
	})
	if h.Bus != nil {
		h.Bus.Publish(eventbus.ConcernTaggedEvent{
			ConcernID:   id,
			EventIDs:    evIDs,
			Source:      source,
			BatchOrigin: "user",
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"tagged": len(tags)})
}

func (h *ConcernsHandler) removeTag(c echo.Context) error {
	id := c.Param("id")
	evID := c.Param("event_id")
	if id == "" || evID == "" {
		return BadRequest(c, "id and event_id are required")
	}
	ctx := c.Request().Context()
	if err := h.Observations.Untag(ctx, id, evID); err != nil {
		return h.serverErr(c, "concern untag failed", err)
	}
	h.audit(ctx, logp.KindConcernUntagged, map[string]any{
		"concern_id": id,
		"event_id":   evID,
	})
	return c.NoContent(http.StatusNoContent)
}

// toDTO converts a stored concern to the wire shape and includes the
// visible-tag count via the observations repo.
func (h *ConcernsHandler) toDTO(ctx context.Context, c store.Concern) (concernDTO, error) {
	dto := concernDTO{
		ID:           c.ID,
		Name:         c.Name,
		Description:  c.Description,
		State:        c.State,
		Source:       c.Source,
		Confidence:   c.Confidence,
		MergedIntoID: c.MergedIntoID,
		SplitFromID:  c.SplitFromID,
		LastActiveAt: c.LastActiveAt,
		EndedAt:      c.EndedAt,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
	if h.Observations != nil {
		count, err := h.Observations.CountByConcern(ctx, c.ID)
		if err != nil {
			return dto, err
		}
		dto.ObservationCount = count
	}
	dto.ReadyToRetire = h.computeReadyToRetire(c)
	return dto, nil
}

// computeReadyToRetire derives the ready-to-retire flag from the row's
// LastActiveAt and the configured threshold. Only `active` concerns
// qualify; everything else stays false.
func (h *ConcernsHandler) computeReadyToRetire(c store.Concern) bool {
	if c.State != store.ConcernStateActive {
		return false
	}
	days := h.AutoRetireDays
	if days <= 0 {
		days = 90
	}
	cutoff := h.now().Add(-time.Duration(days) * 24 * time.Hour)
	return !c.LastActiveAt.After(cutoff)
}

// audit appends an event to the durable log if EventLog is configured.
// Best-effort — errors are swallowed (mirrors MemoryHandler).
func (h *ConcernsHandler) audit(ctx context.Context, kind string, payload map[string]any) {
	if h.EventLog == nil {
		return
	}
	_, _ = h.EventLog.Append(ctx, kind, "ui", payload)
}

func (h *ConcernsHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *ConcernsHandler) serverErr(c echo.Context, msg string, err error) error {
	if h.Log != nil {
		h.Log.WithError(err).Error(msg)
	}
	return Internal(c, err)
}

func ptr(s string) *string { return &s }

func idList(rows []store.Concern) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// intToString avoids importing strconv just for one error-message field.
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
