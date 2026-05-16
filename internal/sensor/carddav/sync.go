package carddav

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav/carddav"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/store"
)

// Sensor implements internal/sensor.Sensor for CardDAV. Each Sync
// fetches the full address book from Provider, transforms each vCard
// into a CardDAVContact row, upserts them, and soft-deletes any rows
// whose vCard disappeared from the server (deduplication by Href).
type Sensor struct {
	provider Provider
	repo     *store.CardDAVRepo
	log      *logrus.Entry
	now      func() time.Time
}

// New constructs the sensor.
func New(p Provider, repo *store.CardDAVRepo, log *logrus.Entry) *Sensor {
	return &Sensor{provider: p, repo: repo, log: log, now: time.Now}
}

// WithNow overrides the clock (tests).
func (s *Sensor) WithNow(now func() time.Time) *Sensor {
	s.now = now
	return s
}

// Name identifies the sensor in scheduler output.
func (s *Sensor) Name() string { return "carddav" }

// Sync runs one poll cycle.
func (s *Sensor) Sync(ctx context.Context) error {
	if s == nil {
		return errors.New("carddav: nil sensor")
	}
	if s.provider == nil || s.repo == nil {
		return errors.New("carddav: not initialized")
	}

	objs, err := s.provider.ListAll(ctx)
	if err != nil {
		return err
	}

	now := s.now()
	upserts := make([]store.CardDAVContact, 0, len(objs))
	seenHrefs := make(map[string]struct{}, len(objs))

	for _, obj := range objs {
		c := vcardToContact(obj, now)
		if c.UID == "" {
			// Skip cards without a UID — we can't dedupe them.
			if s.log != nil {
				s.log.WithField("href", obj.Path).Debug("carddav: skipping vCard with empty UID")
			}
			continue
		}
		upserts = append(upserts, c)
		if c.Href != "" {
			seenHrefs[c.Href] = struct{}{}
		}
	}

	if err := s.repo.UpsertBatch(ctx, upserts); err != nil {
		return err
	}

	// Soft-delete rows whose Href is no longer present on the server.
	existing, err := s.repo.ListAll(ctx)
	if err != nil {
		return err
	}
	deleted := 0
	for _, e := range existing {
		if e.Href == "" {
			continue
		}
		if _, ok := seenHrefs[e.Href]; !ok {
			if err := s.repo.SoftDeleteByHref(ctx, e.Href); err != nil {
				if s.log != nil {
					s.log.WithError(err).WithField("href", e.Href).Warn("carddav: soft-delete failed")
				}
				continue
			}
			deleted++
		}
	}

	if s.log != nil {
		s.log.WithFields(logrus.Fields{
			"upserted": len(upserts),
			"deleted":  deleted,
		}).Info("carddav: sync complete")
	}
	return nil
}

// vcardToContact transforms one AddressObject into a row. Pure (no I/O,
// no clock except `now`), so tests cover it directly without needing
// a Sensor or repo.
func vcardToContact(obj carddav.AddressObject, now time.Time) store.CardDAVContact {
	card := obj.Card
	if card == nil {
		card = vcard.Card{}
	}

	uid := strings.TrimSpace(card.Value(vcard.FieldUID))
	displayName := strings.TrimSpace(card.Value(vcard.FieldFormattedName))

	var given, family string
	if n := card.Name(); n != nil {
		given = strings.TrimSpace(n.GivenName)
		family = strings.TrimSpace(n.FamilyName)
	}

	if displayName == "" {
		// Fall back to "Given Family" if FN is missing.
		fallback := strings.TrimSpace(given + " " + family)
		displayName = fallback
	}

	nicknames := extractNicknames(card)
	phones := extractPhones(card)
	emails := extractEmails(card)

	return store.CardDAVContact{
		UID:         uid,
		Href:        obj.Path,
		DisplayName: displayName,
		GivenName:   given,
		FamilyName:  family,
		Nicknames:   store.EncodeStringList(nicknames),
		Phones:      store.EncodePhones(phones),
		Emails:      store.EncodeEmails(emails),
		ETag:        obj.ETag,
		LastSyncAt:  now,
	}
}

// extractNicknames pulls every NICKNAME value, splitting on comma when
// the vCard packs multiple nicknames into one field.
func extractNicknames(card vcard.Card) []string {
	var out []string
	for _, f := range card[vcard.FieldNickname] {
		if f == nil {
			continue
		}
		for _, v := range strings.Split(f.Value, ",") {
			if vv := strings.TrimSpace(v); vv != "" {
				out = append(out, vv)
			}
		}
	}
	return out
}

// extractPhones pulls every TEL value, capturing TYPE flags and PREF.
func extractPhones(card vcard.Card) []store.Phone {
	var out []store.Phone
	for _, f := range card[vcard.FieldTelephone] {
		if f == nil {
			continue
		}
		val := strings.TrimSpace(f.Value)
		if val == "" {
			continue
		}
		out = append(out, store.Phone{
			Value: val,
			Types: f.Params.Types(),
			Pref:  parsePref(f.Params.Get(vcard.ParamPreferred)),
		})
	}
	return out
}

// extractEmails pulls every EMAIL value with TYPE flags.
func extractEmails(card vcard.Card) []store.Email {
	var out []store.Email
	for _, f := range card[vcard.FieldEmail] {
		if f == nil {
			continue
		}
		val := strings.TrimSpace(f.Value)
		if val == "" {
			continue
		}
		out = append(out, store.Email{
			Value: val,
			Types: f.Params.Types(),
		})
	}
	return out
}

// parsePref turns the PREF param string into an int. RFC 6350 PREF is
// 1–100 (1 = highest); legacy vCard 3.0 used the literal "TYPE=PREF" or
// no value. Empty / unparseable defaults to 0 (no preference).
func parsePref(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
