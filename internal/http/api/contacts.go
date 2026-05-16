package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// ContactsHandler answers the V2.12 /api/contacts surface used by the
// Settings UI.
//
// Mental model after the V2.12 contacts UX refinement:
//
//   - The CardDAV book is the address directory. Every imported vCard
//     surfaces here with its full phone/email/nickname detail.
//   - "Aliases" are nickname → CardDAV-contact mappings the user opts
//     into ("my wife" → Sam Carter). They live in `memory_facts`
//     (category=contact_whatsapp) joined to `memory_contact_links`. A
//     CardDAV contact may have 0..N aliases attached.
//   - "Groups" are stand-alone memory-fact entries with a literal JID,
//     since group JIDs are not present in any address book.
//   - The receive allowlist (`whatsapp_config.allowed_dm_jids`) is
//     independent from this panel — it gates incoming DMs, not sends.
//
// Read-side endpoints:
//
//	GET  /api/contacts              — full directory + aliases + groups
//	GET  /api/contacts/carddav?q=…  — CardDAV cache search (still useful
//	                                  for a tag-search input over very
//	                                  large books).
//
// Mutation:
//
//	POST   /api/contacts            — create an alias (CardDAV-linked)
//	                                  OR a group (jid-set)
//	DELETE /api/contacts/:id        — soft-delete an alias / group
//	PATCH  /api/contacts/:id        — update fact text or preferred phone
type ContactsHandler struct {
	Memory   *store.MemoryRepo
	Link     *store.ContactLinkRepo
	CardDAV  *store.CardDAVRepo // optional; nil disables CardDAV listing
	EventLog logp.Writer
	Now      func() time.Time
	Log      *logrus.Entry
}

// Register attaches the routes.
func (h *ContactsHandler) Register(e *echo.Echo) {
	e.GET("/api/contacts", h.list)
	e.POST("/api/contacts", h.create)
	e.DELETE("/api/contacts/:id", h.del)
	e.PATCH("/api/contacts/:id", h.patch)
	if h.CardDAV != nil {
		e.GET("/api/contacts/carddav", h.searchCardDAV)
	}
}

// aliasDTO is one user-curated alias attached to a CardDAV contact.
type aliasDTO struct {
	ID             string `json:"id"`
	Subject        string `json:"subject"`
	Fact           string `json:"fact,omitempty"`
	PreferredPhone string `json:"preferred_phone,omitempty"`
}

// directoryContactDTO is one row in the unified directory: a CardDAV
// contact plus any aliases the user attached. Phones / Emails carry the
// full vCard detail so the Settings UI can show the user every reachable
// number / address.
type directoryContactDTO struct {
	UID         string     `json:"uid"`
	DisplayName string     `json:"display_name"`
	GivenName   string     `json:"given_name,omitempty"`
	FamilyName  string     `json:"family_name,omitempty"`
	Nicknames   []string   `json:"nicknames,omitempty"`
	Phones      []phoneDTO `json:"phones,omitempty"`
	Emails      []emailDTO `json:"emails,omitempty"`
	Aliases     []aliasDTO `json:"aliases,omitempty"`
}

// groupDTO is one labelled group JID.
type groupDTO struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Fact    string `json:"fact,omitempty"`
	JID     string `json:"jid"`
}

// phoneDTO is one TEL value with type tags + the RFC 6350 PREF flag.
type phoneDTO struct {
	Value string   `json:"value"`
	Types []string `json:"types,omitempty"`
	Pref  int      `json:"pref,omitempty"`
}

// emailDTO is one EMAIL value with type tags.
type emailDTO struct {
	Value string   `json:"value"`
	Types []string `json:"types,omitempty"`
}

// cardDAVDTO is the wire shape for a CardDAV cache row returned by the
// q-string search endpoint. Subset of directoryContactDTO without
// aliases (which are joined only by the unified list endpoint).
type cardDAVDTO struct {
	UID         string     `json:"uid"`
	DisplayName string     `json:"display_name"`
	Nicknames   []string   `json:"nicknames,omitempty"`
	Phones      []phoneDTO `json:"phones,omitempty"`
}

// contactsListResponse is the unified response from GET /api/contacts.
//
// CardDAVEnabled tells the UI whether to render the empty-state hint
// pointing at the CardDAV config block (vs assuming the user has just
// not synced yet). LastSyncAt is the freshest LastSyncAt across all
// cached vCards — zero when the cache is empty.
type contactsListResponse struct {
	CardDAVEnabled bool                  `json:"carddav_enabled"`
	LastSyncAt     time.Time             `json:"last_sync_at,omitzero"`
	Contacts       []directoryContactDTO `json:"contacts"`
	Groups         []groupDTO            `json:"groups"`
}

// cardDAVListResponse is the wire shape for the q-string search
// endpoint. Kept separate from contactsListResponse so the UI's search
// fallback consumes a flat list.
type cardDAVListResponse struct {
	Contacts []cardDAVDTO `json:"contacts"`
}

// createContactRequest is the POST body. Exactly one of CardDAVUID or
// JID is required (matching the link-table invariant).
type createContactRequest struct {
	Subject        string `json:"subject"`
	Fact           string `json:"fact"`
	CardDAVUID     string `json:"carddav_uid,omitempty"`
	PreferredPhone string `json:"preferred_phone,omitempty"`
	JID            string `json:"jid,omitempty"`
}

// patchContactRequest is the PATCH body.
type patchContactRequest struct {
	Fact           *string `json:"fact,omitempty"`
	PreferredPhone *string `json:"preferred_phone,omitempty"`
}

func (h *ContactsHandler) list(c echo.Context) error {
	ctx := c.Request().Context()

	out := contactsListResponse{
		CardDAVEnabled: h.CardDAV != nil,
		Contacts:       make([]directoryContactDTO, 0),
		Groups:         make([]groupDTO, 0),
	}

	// 1. Pull every visible alias / group memory-fact row.
	rows, err := h.Memory.ListAllVisible(ctx)
	if err != nil {
		return Internal(c, err)
	}

	// aliasesByUID maps a CardDAV UID to the aliases attached to it.
	// `groups` accumulates JID-set rows. We iterate memory facts once
	// and dispatch into the right bucket.
	aliasesByUID := map[string][]aliasDTO{}
	for _, f := range rows {
		if f.Category != store.MemoryCategoryContactWhatsApp {
			continue
		}
		link, err := h.Link.GetByID(ctx, f.ID)
		if err != nil || link == nil {
			continue
		}
		switch {
		case link.JID != "" && link.IsGroup:
			out.Groups = append(out.Groups, groupDTO{
				ID:      f.ID,
				Subject: f.Subject,
				Fact:    f.Fact,
				JID:     link.JID,
			})
		case link.CardDAVUID != "":
			aliasesByUID[link.CardDAVUID] = append(aliasesByUID[link.CardDAVUID], aliasDTO{
				ID:             f.ID,
				Subject:        f.Subject,
				Fact:           f.Fact,
				PreferredPhone: link.PreferredPhone,
			})
		}
	}

	// 2. Walk the CardDAV cache to produce the directory rows. Each row
	// carries every phone/email/nickname plus the aliases (if any) the
	// user has attached.
	if h.CardDAV != nil {
		cards, err := h.CardDAV.ListAll(ctx)
		if err != nil {
			return Internal(c, err)
		}
		for _, card := range cards {
			row := directoryContactDTO{
				UID:         card.UID,
				DisplayName: card.DisplayName,
				GivenName:   card.GivenName,
				FamilyName:  card.FamilyName,
				Nicknames:   card.NicknameList(),
				Aliases:     aliasesByUID[card.UID],
			}
			for _, p := range card.PhoneList() {
				row.Phones = append(row.Phones, phoneDTO{
					Value: p.Value,
					Types: p.Types,
					Pref:  p.Pref,
				})
			}
			for _, e := range card.EmailList() {
				row.Emails = append(row.Emails, emailDTO{
					Value: e.Value,
					Types: e.Types,
				})
			}
			if card.LastSyncAt.After(out.LastSyncAt) {
				out.LastSyncAt = card.LastSyncAt
			}
			out.Contacts = append(out.Contacts, row)
		}
	}

	return c.JSON(http.StatusOK, out)
}

func (h *ContactsHandler) create(c echo.Context) error {
	ctx := c.Request().Context()
	var req createContactRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	subject := strings.ToLower(strings.TrimSpace(req.Subject))
	fact := strings.TrimSpace(req.Fact)
	cardDAVUID := strings.TrimSpace(req.CardDAVUID)
	jid := strings.TrimSpace(req.JID)

	if subject == "" {
		return BadRequest(c, "subject is required")
	}
	if len(subject) > 64 {
		return BadRequest(c, "subject is too long (max 64 chars)")
	}
	if cardDAVUID == "" && jid == "" {
		return BadRequest(c, "carddav_uid or jid is required")
	}
	if cardDAVUID != "" && jid != "" {
		return BadRequest(c, "only one of carddav_uid or jid may be set")
	}
	if cardDAVUID != "" && h.CardDAV != nil {
		card, err := h.CardDAV.GetByUID(ctx, cardDAVUID)
		if err != nil {
			return Internal(c, err)
		}
		if card == nil {
			return BadRequest(c, "carddav_uid not found in the contacts cache")
		}
	}

	existing, err := h.Memory.GetBySubject(ctx, subject, true)
	if err != nil {
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
	id := store.BuildContactID(subject, fact, cardDAVUID, jid)

	factRow := store.MemoryFact{
		ID:             id,
		Subject:        subject,
		Fact:           fact,
		Category:       store.MemoryCategoryContactWhatsApp,
		Confidence:     "high",
		Source:         "user",
		EvidenceCount:  1,
		FirstSeen:      now,
		LastReinforced: now,
	}
	if err := h.Memory.Insert(ctx, factRow); err != nil {
		return Internal(c, err)
	}

	link := store.MemoryContactLink{
		ID:             id,
		CardDAVUID:     cardDAVUID,
		JID:            jid,
		PreferredPhone: strings.TrimSpace(req.PreferredPhone),
	}
	if err := h.Link.Insert(ctx, link); err != nil {
		// Memory fact was inserted; roll back to avoid orphaned facts.
		_ = h.Memory.SoftDelete(ctx, id)
		return BadRequest(c, err.Error())
	}

	if h.EventLog != nil {
		_, _ = h.EventLog.Append(ctx, logp.KindMemoryAdded, "ui", map[string]any{
			"id":       id,
			"subject":  subject,
			"category": store.MemoryCategoryContactWhatsApp,
			"source":   "user",
		})
	}

	// Re-read to pick up IsGroup (derived in ValidateLink). Return a
	// shape-aligned response: aliases get an aliasDTO, groups get a
	// groupDTO. The UI uses the field set to dispatch.
	stored, _ := h.Link.GetByID(ctx, id)
	if stored != nil && stored.IsGroup {
		return c.JSON(http.StatusCreated, groupDTO{
			ID:      id,
			Subject: subject,
			Fact:    fact,
			JID:     stored.JID,
		})
	}
	return c.JSON(http.StatusCreated, aliasDTO{
		ID:             id,
		Subject:        subject,
		Fact:           fact,
		PreferredPhone: link.PreferredPhone,
	})
}

func (h *ContactsHandler) del(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	ctx := c.Request().Context()
	existing, err := h.Memory.GetByID(ctx, id)
	if err != nil {
		return Internal(c, err)
	}
	if existing == nil {
		return NotFound(c, "contact not found")
	}
	if err := h.Link.SoftDelete(ctx, id); err != nil {
		return Internal(c, err)
	}
	if err := h.Memory.SoftDelete(ctx, id); err != nil {
		return Internal(c, err)
	}
	if h.EventLog != nil {
		_, _ = h.EventLog.Append(ctx, logp.KindMemoryDeleted, "ui", map[string]any{
			"id":      id,
			"subject": existing.Subject,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *ContactsHandler) patch(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return BadRequest(c, "id is required")
	}
	var req patchContactRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if req.Fact == nil && req.PreferredPhone == nil {
		return BadRequest(c, "at least one of fact or preferred_phone must be provided")
	}

	ctx := c.Request().Context()
	existing, err := h.Memory.GetByID(ctx, id)
	if err != nil {
		return Internal(c, err)
	}
	if existing == nil {
		return NotFound(c, "contact not found")
	}

	if req.Fact != nil {
		f := strings.TrimSpace(*req.Fact)
		if f == "" {
			return BadRequest(c, "fact must be non-empty")
		}
		if err := h.Memory.UpdateFact(ctx, id, f); err != nil {
			return Internal(c, err)
		}
	}
	if req.PreferredPhone != nil {
		if err := h.Link.UpdatePreferredPhone(ctx, id, *req.PreferredPhone); err != nil {
			return Internal(c, err)
		}
	}

	link, _ := h.Link.GetByID(ctx, id)
	if link == nil {
		return NotFound(c, "contact link missing")
	}
	updated, _ := h.Memory.GetByID(ctx, id)
	if updated == nil {
		return NotFound(c, "contact missing")
	}
	if link.IsGroup {
		return c.JSON(http.StatusOK, groupDTO{
			ID:      id,
			Subject: updated.Subject,
			Fact:    updated.Fact,
			JID:     link.JID,
		})
	}
	return c.JSON(http.StatusOK, aliasDTO{
		ID:             id,
		Subject:        updated.Subject,
		Fact:           updated.Fact,
		PreferredPhone: link.PreferredPhone,
	})
}

func (h *ContactsHandler) searchCardDAV(c echo.Context) error {
	q := strings.TrimSpace(c.QueryParam("q"))
	hits, err := h.CardDAV.Search(c.Request().Context(), q, 25)
	if err != nil {
		return Internal(c, err)
	}
	out := cardDAVListResponse{Contacts: make([]cardDAVDTO, 0, len(hits))}
	for _, hit := range hits {
		dto := cardDAVDTO{
			UID:         hit.UID,
			DisplayName: hit.DisplayName,
			Nicknames:   hit.NicknameList(),
		}
		for _, p := range hit.PhoneList() {
			dto.Phones = append(dto.Phones, phoneDTO{
				Value: p.Value,
				Types: p.Types,
				Pref:  p.Pref,
			})
		}
		out.Contacts = append(out.Contacts, dto)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ContactsHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

