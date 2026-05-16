package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/llm"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// ----------------------------------------------------------------------
// AddConcernExec — promotes the card content to a new concern (active).
// ----------------------------------------------------------------------

type AddConcernExec struct {
	Concerns *store.ConcernRepo
	Logger   *logrus.Entry
	Now      func() time.Time
}

func (e *AddConcernExec) Mode() Mode { return Mode1Click }

func (e *AddConcernExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Concerns == nil {
		return Result{OK: false, Toast: "Concerns store not configured."}, nil
	}
	name := stringFromTarget(ec.Target, "name")
	if name == "" && ec.Card != nil {
		name = ec.Card.Title
	}
	if name == "" {
		return Result{OK: false, Toast: "target.name is required."}, nil
	}
	desc := stringFromTarget(ec.Target, "description")
	if desc == "" && ec.Card != nil {
		desc = ec.Card.Sub
	}

	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	if !ec.Now.IsZero() {
		now = ec.Now
	}

	norm := store.NormalizeConcernName(name)
	if existing, err := e.Concerns.GetByNormName(ctx, norm, false); err == nil && existing != nil {
		return Result{
			OK:    true,
			Toast: fmt.Sprintf("Already tracked: %s", existing.Name),
		}, nil
	}

	c := store.Concern{
		ID:           uuid.NewString(),
		Name:         strings.TrimSpace(name),
		NormName:     norm,
		Description:  strings.TrimSpace(desc),
		State:        store.ConcernStateActive,
		Source:       store.ConcernSourceUser,
		Confidence:   1.0,
		LastActiveAt: now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := e.Concerns.Insert(ctx, c); err != nil {
		return Result{OK: false, Toast: "Could not save concern."}, err
	}

	return Result{
		OK:        true,
		EventKind: logp.KindConcernAddedViaAction,
		EventPayload: map[string]any{
			"concern_id": c.ID,
			"name":       c.Name,
			"source":     c.Source,
		},
		Toast: fmt.Sprintf("Tracking: %s", c.Name),
	}, nil
}

// ----------------------------------------------------------------------
// AddMemoryExec — persists a derived-memory fact.
// ----------------------------------------------------------------------

// AddMemoryExec persists a derived-memory fact and, when the subject
// matches an existing CardDAV contact, atomically attaches a
// MemoryContactLink so the WhatsApp Resolver can find the fact.
//
// Link / CardDAV are nil-tolerant — leaving them nil degrades the
// executor to a plain memory insert (preserves the v1 behavior the
// existing tests + replay paths depend on).
type AddMemoryExec struct {
	Memory  *store.MemoryRepo
	Link    *store.ContactLinkRepo // optional; nil → skip link creation
	CardDAV *store.CardDAVRepo     // optional; nil → skip contact resolution
	Now     func() time.Time
}

func (e *AddMemoryExec) Mode() Mode { return Mode1Click }

func (e *AddMemoryExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Memory == nil {
		return Result{OK: false, Toast: "Memory store not configured."}, nil
	}
	subject := strings.ToLower(strings.TrimSpace(stringFromTarget(ec.Target, "subject")))
	if subject == "" {
		return Result{OK: false, Toast: "target.subject is required."}, nil
	}
	fact := stringFromTarget(ec.Target, "fact")
	if fact == "" && ec.Card != nil {
		fact = ec.Card.Sub
	}
	if fact == "" {
		return Result{OK: false, Toast: "target.fact is required."}, nil
	}
	category := stringFromTarget(ec.Target, "category")
	if category == "" {
		category = "general"
	}

	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	if !ec.Now.IsZero() {
		now = ec.Now
	}

	if existing, err := e.Memory.GetBySubject(ctx, subject, false); err == nil && existing != nil {
		return Result{
			OK:    true,
			Toast: fmt.Sprintf("Already remembered: %s", existing.Subject),
		}, nil
	}

	// Try to resolve the subject against the CardDAV directory. When
	// exactly one contact's display name (or nickname) matches the
	// subject case-insensitively, we create a MemoryContactLink so
	// the WhatsApp Resolver finds the fact AND we override the fact's
	// category to MemoryCategoryContactWhatsApp (the gate the resolver
	// filters on). The category override is intentional and
	// load-bearing — without it, even a perfectly-linked fact stays
	// invisible to resolve_contact. Audit-side note: the rewrite is
	// surfaced in the event log via carddav_uid below.
	contact := e.resolveExactCardDAVMatch(ctx, subject)
	fact = strings.TrimSpace(fact)

	if contact != nil && e.Link != nil {
		id := store.BuildContactID(subject, fact, contact.UID, "")
		linkedCategory := store.MemoryCategoryContactWhatsApp
		m := store.MemoryFact{
			ID:             id,
			Subject:        subject,
			Fact:           fact,
			Category:       linkedCategory,
			Confidence:     "high", // user-source facts skip the evidence ladder
			Source:         "user",
			EvidenceCount:  1,
			FirstSeen:      now,
			LastReinforced: now,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := e.Memory.Insert(ctx, m); err != nil {
			return Result{OK: false, Toast: "Could not save memory."}, err
		}
		link := store.MemoryContactLink{
			ID:         id,
			CardDAVUID: contact.UID,
		}
		if err := e.Link.Insert(ctx, link); err != nil {
			// Roll back the fact so we don't leave an orphan with the
			// rewritten category and no addressing data behind it.
			_ = e.Memory.SoftDelete(ctx, id)
			return Result{OK: false, Toast: "Could not link contact."}, err
		}
		return Result{
			OK:        true,
			EventKind: logp.KindMemoryAddedViaAction,
			EventPayload: map[string]any{
				"memory_id":   id,
				"subject":     subject,
				"category":    linkedCategory,
				"carddav_uid": contact.UID,
			},
			Toast: fmt.Sprintf("Remembered: %s (linked to %s)", subject, contact.DisplayName),
		}, nil
	}

	m := store.MemoryFact{
		ID:             uuid.NewString(),
		Subject:        subject,
		Fact:           fact,
		Category:       strings.ToLower(category),
		Confidence:     "high", // user-source facts skip the evidence ladder
		Source:         "user",
		EvidenceCount:  1,
		FirstSeen:      now,
		LastReinforced: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := e.Memory.Insert(ctx, m); err != nil {
		return Result{OK: false, Toast: "Could not save memory."}, err
	}

	return Result{
		OK:        true,
		EventKind: logp.KindMemoryAddedViaAction,
		EventPayload: map[string]any{
			"memory_id": m.ID,
			"subject":   m.Subject,
			"category":  m.Category,
		},
		Toast: fmt.Sprintf("Remembered: %s", m.Subject),
	}, nil
}

// resolveExactCardDAVMatch returns the single CardDAV contact whose
// display name or any nickname equals subject (case-insensitive), or
// nil when zero or multiple contacts match. This mirrors the "tighter
// pass" in internal/whatsapp/contacts.go::Resolver.matchCardDAV so the
// writer's matching rule stays aligned with the reader's. We require
// EXACTLY one match — multiple exact matches would force us to guess
// which contact the user meant, and the conservative skip is safer
// than an incorrect link.
//
// Returns nil silently on CardDAV errors — a directory failure
// shouldn't block the underlying memory write.
func (e *AddMemoryExec) resolveExactCardDAVMatch(ctx context.Context, subject string) *store.CardDAVContact {
	if e.CardDAV == nil {
		return nil
	}
	hits, err := e.CardDAV.Search(ctx, subject, 25)
	if err != nil || len(hits) == 0 {
		return nil
	}
	var exact []store.CardDAVContact
	for _, c := range hits {
		if strings.EqualFold(strings.TrimSpace(c.DisplayName), subject) {
			exact = append(exact, c)
			continue
		}
		for _, n := range c.NicknameList() {
			if strings.EqualFold(strings.TrimSpace(n), subject) {
				exact = append(exact, c)
				break
			}
		}
	}
	if len(exact) != 1 {
		return nil
	}
	return &exact[0]
}

// ----------------------------------------------------------------------
// AddTaskExec — V2.11: insert a row into the unified tasks table.
// Subsumes V2.6's Markdown-append path. Optional target keys: title
// (required), due (YYYY-MM-DD), priority (low|med|high), tags ([]string),
// fire_at (RFC3339 or "+30m"/"+2h"/"+1d"). source_card_id is filled
// automatically from the card context when the action originates from a
// briefing card.
// ----------------------------------------------------------------------

type AddTaskExec struct {
	Tasks *store.TaskRepo
	Now   func() time.Time
}

func (e *AddTaskExec) Mode() Mode { return Mode1Click }

func (e *AddTaskExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Tasks == nil {
		return Result{OK: false, Toast: "Tasks store not configured."}, nil
	}
	title := strings.TrimSpace(stringFromTarget(ec.Target, "title"))
	if title == "" && ec.Card != nil {
		title = ec.Card.Title
	}
	if title == "" {
		return Result{OK: false, Toast: "target.title is required."}, nil
	}

	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	if !ec.Now.IsZero() {
		now = ec.Now
	}

	row := store.Task{
		ID:       uuid.NewString(),
		Title:    title,
		Body:     stringFromTarget(ec.Target, "body"),
		DueDate:  strings.TrimSpace(stringFromTarget(ec.Target, "due")),
		Priority: strings.ToLower(strings.TrimSpace(stringFromTarget(ec.Target, "priority"))),
	}
	if ec.Card != nil {
		row.SourceCardID = ec.Card.ID
	}
	if id := strings.TrimSpace(stringFromTarget(ec.Target, "source_card_id")); id != "" {
		row.SourceCardID = id
	}

	if tags := stringSliceFromTarget(ec.Target, "tags"); len(tags) > 0 {
		clean := make([]string, 0, len(tags))
		for _, t := range tags {
			t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "#"))
			if t != "" {
				clean = append(clean, strings.ToLower(t))
			}
		}
		if len(clean) > 0 {
			b, err := json.Marshal(clean)
			if err == nil {
				row.Tags = b
			}
		}
	}

	if fireRaw := strings.TrimSpace(stringFromTarget(ec.Target, "fire_at")); fireRaw != "" {
		fa, err := parseFireAt(fireRaw, now)
		if err != nil {
			return Result{OK: false, Toast: "Could not parse fire_at: " + err.Error()}, nil
		}
		row.FireAt = &fa
	}

	if err := e.Tasks.Insert(ctx, row); err != nil {
		return Result{OK: false, Toast: "Could not save task."}, err
	}

	payload := map[string]any{
		"uid":   row.ID,
		"title": title,
	}
	if row.FireAt != nil {
		payload["fire_at"] = row.FireAt.Format(time.RFC3339)
	}
	if row.SourceCardID != "" {
		payload["source_card_id"] = row.SourceCardID
	}

	return Result{
		OK:           true,
		EventKind:    logp.KindTaskAddedViaAction,
		EventPayload: payload,
		Toast:        fmt.Sprintf("Added task: %s", title),
	}, nil
}

// parseFireAt accepts RFC3339 timestamps and the same "+<N>(m|h|d)"
// shorthand SetReminderExec already supports (via store.ParseRelative).
// The relative form is more ergonomic for both LLM and UI callers.
func parseFireAt(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "+") {
		d, err := store.ParseRelative(raw)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected RFC3339 or +<N>(m|h|d), got %q", raw)
	}
	return t, nil
}

// ----------------------------------------------------------------------
// AskFollowupExec — wraps reactive Ask, seeded with card context.
// ----------------------------------------------------------------------

// AskFn is the callback the Executor invokes to run reactive synthesis.
// Matches the shape AskHandler already wires in cmd/zeno/main.go so
// the boot path can pass the same closure to both.
type AskFn func(ctx context.Context, query string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error)

type AskFollowupExec struct {
	AskFn  AskFn
	Logger *logrus.Entry
}

func (e *AskFollowupExec) Mode() Mode { return Mode1Click }

func (e *AskFollowupExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.AskFn == nil {
		return Result{OK: false, Toast: "Reactive synth not configured."}, nil
	}
	seed := stringFromTarget(ec.Target, "seed")
	if seed == "" {
		seed = stringFromTarget(ec.Target, "query")
	}
	if seed == "" && ec.Card != nil {
		seed = "What should I do about: " + ec.Card.Title
	}
	if seed == "" {
		return Result{OK: false, Toast: "target.seed is required."}, nil
	}

	// Enrich seed with the source card's context so the model has
	// something concrete to reason against.
	query := seed
	if ec.Card != nil {
		query = fmt.Sprintf("%s\n\nContext from card \"%s\": %s", seed, ec.Card.Title, ec.Card.Sub)
	}

	card, _, _, err := e.AskFn(ctx, query)
	if err != nil {
		// AskFn returns a degraded card on most failures; surface
		// silently rather than surfacing the underlying error to the
		// user. Logger still records.
		if e.Logger != nil {
			e.Logger.WithError(err).Warn("ask_followup: AskFn errored")
		}
	}
	if card.ID == "" {
		card.ID = "ask-" + uuid.NewString()[:8]
	}

	return Result{
		OK:        true,
		EventKind: logp.KindAskFollowup,
		EventPayload: map[string]any{
			"seed":  seed,
			"card":  card.ID,
			"title": card.Title,
		},
		Followup: &card,
	}, nil
}
