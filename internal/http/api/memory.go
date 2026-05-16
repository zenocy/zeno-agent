package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/embeddings"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// MemoryHandler answers GET / POST /api/memory and PATCH / DELETE
// /api/memory/:id. Mirrors the patterns at AskHandler / CardsHandler:
// dependency fields, Register attaches all routes, error responses are
// `{"error": "..."}` with conventional HTTP statuses.
type MemoryHandler struct {
	Memory         *store.MemoryRepo
	EmbeddingStore *embeddings.Store       // optional; nil → no vector cache update
	EmbeddingIndex *embeddings.MemoryIndex // optional; nil → no vector cache update
	EventLog       logp.Writer
	Bus            *eventbus.Bus
	TZ             func() *time.Location
	Now            func() time.Time
	Log            *logrus.Entry
}

// publishMemoryList re-reads the top facts and broadcasts the full list
// over SSE so subscribed UI hooks can update their cache without a
// follow-up fetch. Best-effort: failures log and return.
func (h *MemoryHandler) publishMemoryList(ctx context.Context) {
	if h.Bus == nil {
		return
	}
	rows, err := h.Memory.ListTop(ctx, memoryListLimit)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).Warn("memory: list for SSE publish failed")
		}
		return
	}
	out := listResponse{Facts: make([]memoryFactDTO, 0, len(rows))}
	for _, r := range rows {
		out.Facts = append(out.Facts, toMemoryDTO(r))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).Warn("memory: marshal for SSE publish failed")
		}
		return
	}
	h.Bus.Publish(eventbus.MemoryChangedEvent{Memory: raw})
}

// updateEmbedding is the manual-write counterpart to the consolidator's
// post-write hook. Best-effort: any failure logs and returns; the persisted
// fact is the source of truth and the next warmup will repair the index.
func (h *MemoryHandler) updateEmbedding(c echo.Context, id, factText string) {
	if h.EmbeddingStore == nil || h.EmbeddingIndex == nil || id == "" || factText == "" {
		return
	}
	ctx := c.Request().Context()
	start := time.Now()
	if err := h.EmbeddingIndex.Upsert(ctx, id, factText); err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("id", id).Warn("embed: index upsert failed")
		}
		return
	}
	vec, ok := h.EmbeddingIndex.GetVector(id)
	if !ok || len(vec) == 0 {
		return
	}
	blob, err := embeddings.EncodeVector(vec)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("id", id).Warn("embed: encode failed")
		}
		return
	}
	row := embeddings.MemoryEmbedding{
		ID:          id,
		ContentHash: embeddings.HashContent(factText),
		ModelID:     h.EmbeddingIndex.Embedder().ModelID(),
		Dims:        len(vec),
		Vector:      blob,
		UpdatedAt:   time.Now().UTC(),
	}
	if err := h.EmbeddingStore.Upsert(ctx, row); err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("id", id).Warn("embed: store upsert failed")
		}
		return
	}
	if h.Log != nil {
		h.Log.WithFields(logrus.Fields{
			"id":         id,
			"fact_len":   len(factText),
			"dims":       len(vec),
			"latency_ms": time.Since(start).Milliseconds(),
		}).Info("embed: manual fact indexed")
	}
}

// removeEmbedding deletes the cached vector for a soft-deleted fact.
// Best-effort.
func (h *MemoryHandler) removeEmbedding(c echo.Context, id string) {
	if h.EmbeddingStore == nil || h.EmbeddingIndex == nil || id == "" {
		return
	}
	h.EmbeddingIndex.Remove(id)
	if err := h.EmbeddingStore.Delete(c.Request().Context(), id); err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("id", id).Warn("embed: store delete failed")
		}
		return
	}
	if h.Log != nil {
		h.Log.WithField("id", id).Info("embed: manual fact removed")
	}
}

// memoryListLimit caps how many facts the GET endpoint returns. The total-
// fact cap (V2.2.0 plan: 50) is consolidator-side; the API exposes more
// headroom so the UI surfaces orphaned-but-not-yet-evicted rows too.
const memoryListLimit = 200

// allowedCategories pins the enum the manual-add UI emits. The synth
// consolidator currently inserts at "misc"; the user-facing categories are
// the spec-defined set. Submitting outside this list returns 400.
var allowedCategories = map[string]struct{}{
	"identity":     {},
	"relationship": {},
	"preference":   {},
	"routine":      {},
	"context":      {},
	"misc":         {},
}

// Register attaches the four memory routes to the Echo instance.
func (h *MemoryHandler) Register(e *echo.Echo) {
	e.GET("/api/memory", h.list)
	e.POST("/api/memory", h.create)
	e.PATCH("/api/memory/:id", h.patch)
	e.DELETE("/api/memory/:id", h.del)
}

// memoryFactDTO is the JSON wire shape returned to the UI.
type memoryFactDTO struct {
	ID             string    `json:"id"`
	Subject        string    `json:"subject"`
	Fact           string    `json:"fact"`
	Category       string    `json:"category"`
	Confidence     string    `json:"confidence"`
	Source         string    `json:"source"`
	EvidenceCount  int       `json:"evidence_count"`
	FirstSeen      time.Time `json:"first_seen"`
	LastReinforced time.Time `json:"last_reinforced"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type listResponse struct {
	Facts []memoryFactDTO `json:"facts"`
}

type createRequest struct {
	Subject  string `json:"subject"`
	Fact     string `json:"fact"`
	Category string `json:"category"`
}

type patchRequest struct {
	Fact     *string `json:"fact,omitempty"`
	Category *string `json:"category,omitempty"`
}

func (h *MemoryHandler) list(c echo.Context) error {
	rows, err := h.Memory.ListTop(c.Request().Context(), memoryListLimit)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).Error("memory list failed")
		}
		return Internal(c, err)
	}
	out := listResponse{Facts: make([]memoryFactDTO, 0, len(rows))}
	for _, r := range rows {
		out.Facts = append(out.Facts, toMemoryDTO(r))
	}
	return c.JSON(http.StatusOK, out)
}

func (h *MemoryHandler) create(c echo.Context) error {
	var req createRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	subject := strings.ToLower(strings.TrimSpace(req.Subject))
	fact := strings.TrimSpace(req.Fact)
	category := strings.ToLower(strings.TrimSpace(req.Category))

	if subject == "" {
		return BadRequest(c, "subject is required")
	}
	if strings.ContainsAny(subject, " \t\n") {
		return BadRequest(c, "subject must be a single token (no whitespace)")
	}
	if len(subject) > 64 {
		return BadRequest(c, "subject is too long (max 64 chars)")
	}
	if fact == "" {
		return BadRequest(c, "fact is required")
	}
	if len(fact) > 280 {
		return BadRequest(c, "fact is too long (max 280 chars)")
	}
	if category == "" {
		category = "misc"
	}
	if _, ok := allowedCategories[category]; !ok {
		return BadRequest(c, "category is not in the allowed set")
	}

	// Conflict: if a visible fact already has this subject the UI should
	// PATCH it instead. Soft-deleted (denylisted) subjects are also a
	// conflict — the user previously deleted that subject; re-adding via
	// POST silently un-denylists is too magical. UI surfaces a 409 and
	// asks the user to confirm.
	existing, err := h.Memory.GetBySubject(c.Request().Context(), subject, true)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("subject", subject).Error("memory create lookup failed")
		}
		return Internal(c, err)
	}
	if existing != nil {
		return c.JSON(http.StatusConflict, map[string]string{
			"error":   "subject already exists",
			"id":      existing.ID,
			"subject": existing.Subject,
		})
	}

	now := h.now()
	fact_row := store.MemoryFact{
		ID:             memoryFactID(subject, fact),
		Subject:        subject,
		Fact:           fact,
		Category:       category,
		Confidence:     "high",
		Source:         "user",
		EvidenceCount:  1,
		FirstSeen:      now,
		LastReinforced: now,
	}
	if err := h.Memory.Insert(c.Request().Context(), fact_row); err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("subject", subject).Error("memory create failed")
		}
		return Internal(c, err)
	}

	h.updateEmbedding(c, fact_row.ID, fact_row.Fact)

	if h.EventLog != nil {
		_, _ = h.EventLog.Append(c.Request().Context(), logp.KindMemoryAdded, "ui", map[string]any{
			"id":      fact_row.ID,
			"subject": subject,
			"source":  "user",
		})
	}
	h.publishMemoryList(c.Request().Context())

	return c.JSON(http.StatusCreated, toMemoryDTO(fact_row))
}

func (h *MemoryHandler) patch(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	var req patchRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if req.Fact == nil && req.Category == nil {
		return BadRequest(c, "at least one of fact or category must be provided")
	}

	existing, err := h.Memory.GetByID(c.Request().Context(), id)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("id", id).Error("memory patch lookup failed")
		}
		return Internal(c, err)
	}
	if existing == nil {
		return NotFound(c, "fact not found")
	}

	fields := []string{}
	if req.Fact != nil {
		fact := strings.TrimSpace(*req.Fact)
		if fact == "" {
			return BadRequest(c, "fact must be non-empty")
		}
		if len(fact) > 280 {
			return BadRequest(c, "fact is too long (max 280 chars)")
		}
		if err := h.Memory.UpdateFact(c.Request().Context(), id, fact); err != nil {
			if h.Log != nil {
				h.Log.WithError(err).WithField("id", id).Error("memory patch fact failed")
			}
			return Internal(c, err)
		}
		fields = append(fields, "fact")
	}
	if req.Category != nil {
		category := strings.ToLower(strings.TrimSpace(*req.Category))
		if _, ok := allowedCategories[category]; !ok {
			return BadRequest(c, "category is not in the allowed set")
		}
		if err := h.Memory.UpdateCategory(c.Request().Context(), id, category); err != nil {
			if h.Log != nil {
				h.Log.WithError(err).WithField("id", id).Error("memory patch category failed")
			}
			return Internal(c, err)
		}
		fields = append(fields, "category")
	}

	updated, err := h.Memory.GetByID(c.Request().Context(), id)
	if err != nil || updated == nil {
		// Failure on the readback isn't fatal — return what we know.
		updated = existing
	}

	// Re-embed only when fact text changed; a category-only edit doesn't
	// touch the vector. The hash short-circuit in MemoryIndex.Upsert makes
	// a redundant call cheap, but skipping it avoids an unnecessary embed.
	if req.Fact != nil {
		h.updateEmbedding(c, id, updated.Fact)
	}

	if h.EventLog != nil {
		_, _ = h.EventLog.Append(c.Request().Context(), logp.KindMemoryEdited, "ui", map[string]any{
			"id":     id,
			"fields": fields,
		})
	}
	h.publishMemoryList(c.Request().Context())

	return c.JSON(http.StatusOK, toMemoryDTO(*updated))
}

func (h *MemoryHandler) del(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	// Look up the row up-front so the audit event carries the subject. A
	// soft-deleted row still surfaces here via Unscoped because GetByID is
	// scoped — so for a second DELETE we won't find anything; that's fine,
	// the response is still 204.
	existing, _ := h.Memory.GetByID(c.Request().Context(), id)
	if err := h.Memory.SoftDelete(c.Request().Context(), id); err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("id", id).Error("memory delete failed")
		}
		return Internal(c, err)
	}
	h.removeEmbedding(c, id)
	if h.EventLog != nil {
		payload := map[string]any{"id": id}
		if existing != nil {
			payload["subject"] = existing.Subject
		}
		_, _ = h.EventLog.Append(c.Request().Context(), logp.KindMemoryDeleted, "ui", payload)
	}
	h.publishMemoryList(c.Request().Context())
	return c.NoContent(http.StatusNoContent)
}

func (h *MemoryHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// toMemoryDTO converts a persisted MemoryFact to the JSON wire shape.
func toMemoryDTO(m store.MemoryFact) memoryFactDTO {
	return memoryFactDTO{
		ID:             m.ID,
		Subject:        m.Subject,
		Fact:           m.Fact,
		Category:       m.Category,
		Confidence:     m.Confidence,
		Source:         m.Source,
		EvidenceCount:  m.EvidenceCount,
		FirstSeen:      m.FirstSeen,
		LastReinforced: m.LastReinforced,
		UpdatedAt:      m.UpdatedAt,
	}
}

// memoryFactID derives a stable ID from subject + fact text. Same scheme
// used by cmd/zeno/replay.go's seed loader, eval/store.go, and
// internal/synth/consolidate.go so manual + synth-derived rows live in the
// same ID space.
func memoryFactID(subject, fact string) string {
	subj := strings.ToLower(strings.TrimSpace(subject))
	sum := sha256.Sum256([]byte(fact))
	return subj + "-" + hex.EncodeToString(sum[:4])
}
