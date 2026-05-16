package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zenocy/zeno-v2/internal/store"
)

// ErrAmbiguous is returned by Resolver.Resolve when the query matches
// more than one candidate. Candidates carries the canonical name list
// (no JIDs, no phone numbers — those stay server-side).
type ErrAmbiguous struct {
	Query      string
	Candidates []string
}

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("contact %q is ambiguous (%d candidates)", e.Query, len(e.Candidates))
}

// ErrContactNotFound is returned when no candidate matches the query.
// Suggestions carries up to two close-but-not-quite candidate names so
// the LLM can ask the user to confirm.
type ErrContactNotFound struct {
	Query       string
	Suggestions []string
}

func (e *ErrContactNotFound) Error() string {
	return fmt.Sprintf("contact %q not found", e.Query)
}

// Contact is the resolved-recipient struct. JID is the addressable form
// (DM or group) and is for executor-internal use only — must not be
// returned by LLM-visible tools or rendered into prompts. Name + IsGroup
// are the only fields safe to surface upstream.
type Contact struct {
	Name       string // canonical (MemoryFact.Subject or CardDAV display name)
	JID        string // resolved; never leak via LLM tool output
	IsGroup    bool
	FactID     string // MemoryFact.ID; empty for direct CardDAV hits
	CardDAVUID string // when the resolved JID came from a vCard
}

// Resolver maps a free-form query ("my wife", "Sam Carter", a JID) to a
// Contact. It is the user's contact directory + alias map: aliases
// (memory facts in `category=contact_whatsapp`) are *disambiguators*,
// not *gates*. A query that uniquely identifies a CardDAV vCard by name
// or nickname resolves directly even when no alias is attached. The
// receive allowlist (`AllowedDMs` in runtime_config.go) is independent
// — it gates inbound DMs, not outbound sends.
type Resolver struct {
	Memory  *store.MemoryRepo
	Link    *store.ContactLinkRepo
	CardDAV *store.CardDAVRepo // optional; nil disables the CardDAV-direct fallback
}

// Resolve runs the lookup pipeline. See package comment for ordering.
func (r *Resolver) Resolve(ctx context.Context, query string) (Contact, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return Contact{}, errors.New("contact: empty query")
	}

	// 1. JID input → look up the link directly.
	if store.IsDMJID(q) || store.IsGroupJID(q) {
		if c, ok, err := r.lookupByJID(ctx, q); err != nil {
			return Contact{}, err
		} else if ok {
			return c, nil
		}
		// JID not on the allowlist — refuse rather than fall through.
		return Contact{}, &ErrContactNotFound{Query: q}
	}

	ql := strings.ToLower(q)

	// 2 & 3. Match memory-fact contacts.
	factMatches, err := r.matchMemoryFacts(ctx, ql)
	if err != nil {
		return Contact{}, err
	}

	// 4. CardDAV-direct: search the cache by name/nicknames.
	cardMatches, err := r.matchCardDAV(ctx, ql)
	if err != nil {
		return Contact{}, err
	}

	// Combine: if any memory-fact match exists, ignore CardDAV-direct
	// to keep user-curated allowlist authoritative. Memory facts win.
	if len(factMatches) > 0 {
		if len(factMatches) == 1 {
			return r.factToContact(ctx, factMatches[0])
		}
		return Contact{}, ambiguous(q, factsToNames(factMatches))
	}

	// No memory-fact hits — fall back to CardDAV-direct.
	if len(cardMatches) > 0 {
		if len(cardMatches) == 1 {
			return cardToContact(cardMatches[0])
		}
		return Contact{}, ambiguous(q, cardsToNames(cardMatches))
	}

	// Not found. Try a softer suggestion — top-2 substring hits over
	// memory facts even if none scored as a "match".
	return Contact{}, &ErrContactNotFound{Query: q}
}

// lookupByJID resolves a JID to its Contact via the link table. Returns
// ok=false when no link row carries this JID and the JID isn't a DM
// derived from a CardDAV-linked phone.
func (r *Resolver) lookupByJID(ctx context.Context, jid string) (Contact, bool, error) {
	if r.Link == nil {
		return Contact{}, false, nil
	}
	link, err := r.Link.GetByJID(ctx, jid)
	if err != nil {
		return Contact{}, false, err
	}
	if link != nil {
		// Group JID — name comes from the linked memory fact.
		name := jid // fallback if memory lookup fails
		if r.Memory != nil {
			fact, _ := r.Memory.GetByID(ctx, link.ID)
			if fact != nil {
				name = fact.Subject
			}
		}
		return Contact{Name: name, JID: jid, IsGroup: link.IsGroup, FactID: link.ID}, true, nil
	}

	// DM JID — try matching it against any CardDAV-linked phone.
	if store.IsDMJID(jid) && r.CardDAV != nil && r.Link != nil {
		// Walk all DM-style links (CardDAVUID set) and match.
		links, err := r.Link.ListAll(ctx)
		if err != nil {
			return Contact{}, false, err
		}
		for _, l := range links {
			if l.CardDAVUID == "" {
				continue
			}
			derivedJID, err := r.deriveJIDFromLink(ctx, l)
			if err != nil || derivedJID == "" {
				continue
			}
			if derivedJID == jid {
				name := jid
				if r.Memory != nil {
					if fact, _ := r.Memory.GetByID(ctx, l.ID); fact != nil {
						name = fact.Subject
					}
				}
				return Contact{Name: name, JID: jid, IsGroup: false, FactID: l.ID, CardDAVUID: l.CardDAVUID}, true, nil
			}
		}
	}

	return Contact{}, false, nil
}

// matchMemoryFacts returns memory-fact contact rows whose Subject or
// Fact prose matches the lowercased query (substring). Exact-subject
// matches sort first.
func (r *Resolver) matchMemoryFacts(ctx context.Context, ql string) ([]store.MemoryFact, error) {
	if r.Memory == nil {
		return nil, nil
	}
	all, err := r.Memory.ListAllVisible(ctx)
	if err != nil {
		return nil, err
	}
	var exact, substring []store.MemoryFact
	for _, f := range all {
		if f.Category != store.MemoryCategoryContactWhatsApp {
			continue
		}
		subj := strings.ToLower(f.Subject)
		if subj == ql {
			exact = append(exact, f)
			continue
		}
		hay := subj + " " + strings.ToLower(f.Fact)
		if strings.Contains(hay, ql) {
			substring = append(substring, f)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	return substring, nil
}

// matchCardDAV searches the local cache.
func (r *Resolver) matchCardDAV(ctx context.Context, ql string) ([]store.CardDAVContact, error) {
	if r.CardDAV == nil {
		return nil, nil
	}
	hits, err := r.CardDAV.Search(ctx, ql, 25)
	if err != nil {
		return nil, err
	}
	// Tighter pass: prefer exact display-name / nickname matches when present.
	var exact []store.CardDAVContact
	for _, c := range hits {
		if strings.EqualFold(strings.TrimSpace(c.DisplayName), ql) {
			exact = append(exact, c)
			continue
		}
		for _, n := range c.NicknameList() {
			if strings.EqualFold(strings.TrimSpace(n), ql) {
				exact = append(exact, c)
				break
			}
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	return hits, nil
}

// factToContact builds a Contact from a matched memory fact, following
// its link to either a CardDAV vCard (DM) or a literal JID (group).
func (r *Resolver) factToContact(ctx context.Context, f store.MemoryFact) (Contact, error) {
	if r.Link == nil {
		return Contact{}, fmt.Errorf("contact %q: link repo unavailable", f.Subject)
	}
	link, err := r.Link.GetByID(ctx, f.ID)
	if err != nil {
		return Contact{}, err
	}
	if link == nil {
		return Contact{}, fmt.Errorf("contact %q: no addressable link configured", f.Subject)
	}
	// Group: JID is set on the link.
	if link.JID != "" {
		return Contact{
			Name:    f.Subject,
			JID:     link.JID,
			IsGroup: link.IsGroup,
			FactID:  f.ID,
		}, nil
	}
	// DM: derive JID from CardDAV vCard.
	jid, err := r.deriveJIDFromLink(ctx, *link)
	if err != nil {
		return Contact{}, err
	}
	if jid == "" {
		return Contact{}, fmt.Errorf("contact %q: linked vCard has no usable phone", f.Subject)
	}
	return Contact{
		Name:       f.Subject,
		JID:        jid,
		IsGroup:    false,
		FactID:     f.ID,
		CardDAVUID: link.CardDAVUID,
	}, nil
}

// deriveJIDFromLink walks a CardDAV link to a phone → JID. Honors the
// link's PreferredPhone override; falls back to CardDAVContact's
// preference picker when unset.
func (r *Resolver) deriveJIDFromLink(ctx context.Context, link store.MemoryContactLink) (string, error) {
	if r.CardDAV == nil {
		return "", fmt.Errorf("contact: carddav repo unavailable")
	}
	c, err := r.CardDAV.GetByUID(ctx, link.CardDAVUID)
	if err != nil {
		return "", err
	}
	if c == nil {
		return "", fmt.Errorf("contact: linked vCard %q not found in cache", link.CardDAVUID)
	}
	phone := strings.TrimSpace(link.PreferredPhone)
	if phone == "" {
		phone = c.PreferredPhone()
	}
	if phone == "" {
		return "", nil
	}
	jid := store.PhoneToJID(phone)
	return jid, nil
}

// cardToContact builds a Contact from a CardDAV-direct hit. The vCard
// must have at least one phone; otherwise the resolver treats this as
// not-found (the user can't address a contact with no phone number).
func cardToContact(c store.CardDAVContact) (Contact, error) {
	phone := c.PreferredPhone()
	if phone == "" {
		return Contact{}, &ErrContactNotFound{Query: c.DisplayName}
	}
	jid := store.PhoneToJID(phone)
	if jid == "" {
		return Contact{}, &ErrContactNotFound{Query: c.DisplayName}
	}
	return Contact{
		Name:       c.DisplayName,
		JID:        jid,
		IsGroup:    false,
		CardDAVUID: c.UID,
	}, nil
}

func factsToNames(in []store.MemoryFact) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		out = append(out, f.Subject)
	}
	return out
}

func cardsToNames(in []store.CardDAVContact) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, c.DisplayName)
	}
	return out
}

func ambiguous(q string, names []string) error {
	if len(names) > 5 {
		names = names[:5]
	}
	return &ErrAmbiguous{Query: q, Candidates: names}
}

// ResolveByEmail maps a calendar-attendee email to a Contact. Server-side
// only — never invoked by LLM-visible callers. V2.13.0 added this for
// the assistant-mode "text X to confirm" proposal: when a calendar
// attendee carries an email address, the cards loop tries this before
// the name-based path.
//
// Order:
//  1. CardDAV exact email match → derive JID from preferred phone.
//  2. Email local-part as a name query → standard Resolve pipeline.
//
// Returns ErrContactNotFound when neither hits. Email collisions on
// CardDAV are surfaced as not-found rather than guessed (see plan §5
// risks) — the cards loop should skip the proposal in that case.
func (r *Resolver) ResolveByEmail(ctx context.Context, email string) (Contact, error) {
	addr := strings.ToLower(strings.TrimSpace(email))
	if addr == "" {
		return Contact{}, errors.New("contact: empty email")
	}

	if r.CardDAV != nil {
		card, err := r.CardDAV.FindByEmail(ctx, addr)
		if err != nil {
			return Contact{}, err
		}
		if card != nil {
			c, err := cardToContact(*card)
			if err == nil {
				return c, nil
			}
			// vCard with no usable phone — fall through to local-part lookup.
		}
	}

	// Fallback: query the resolver as if the local-part were a name.
	if i := strings.IndexByte(addr, '@'); i > 0 {
		local := strings.TrimSpace(addr[:i])
		if local != "" {
			c, err := r.Resolve(ctx, local)
			if err == nil {
				return c, nil
			}
		}
	}
	return Contact{}, &ErrContactNotFound{Query: email}
}

// ResolverList is the read-side surface for Settings UIs and `resolve_contact`
// candidate dropdowns. Returns the union of memory-fact contacts and
// CardDAV-direct contacts (without duplicates: facts win when their link
// references a CardDAV UID).
func (r *Resolver) List(ctx context.Context) ([]Contact, error) {
	out := make([]Contact, 0, 32)
	seenCardDAV := map[string]struct{}{}

	if r.Memory != nil && r.Link != nil {
		all, err := r.Memory.ListAllVisible(ctx)
		if err != nil {
			return nil, err
		}
		for _, f := range all {
			if f.Category != store.MemoryCategoryContactWhatsApp {
				continue
			}
			c, err := r.factToContact(ctx, f)
			if err != nil {
				continue
			}
			out = append(out, c)
			if c.CardDAVUID != "" {
				seenCardDAV[c.CardDAVUID] = struct{}{}
			}
		}
	}

	if r.CardDAV != nil {
		all, err := r.CardDAV.ListAll(ctx)
		if err != nil {
			return nil, err
		}
		for _, ce := range all {
			if _, ok := seenCardDAV[ce.UID]; ok {
				continue
			}
			c, err := cardToContact(ce)
			if err != nil {
				continue
			}
			out = append(out, c)
		}
	}

	return out, nil
}
