package caldav

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/zenocy/zeno-v2/internal/config"
)

// FastmailProvider is the Provider implementation for Fastmail's CalDAV
// endpoint. It uses HTTP basic auth (Fastmail accepts an app password).
// Calendar paths are discovered at construction time via the CalDAV
// principal/home-set protocol, so cfg.URL only needs to be the server root
// (e.g. https://caldav.fastmail.com/) rather than a UUID calendar path.
type FastmailProvider struct {
	cfg           config.CalDAVConfig
	client        *caldav.Client
	calendarPaths []string

	// httpClient is the auth-wrapped HTTP client kept around so the
	// V2.8 write methods can issue raw PUT/DELETE with If-Match /
	// If-None-Match headers — the high-level go-webdav API doesn't
	// expose those.
	httpClient webdav.HTTPClient
	// baseURL is the server root (e.g. https://caldav.fastmail.com)
	// used to assemble absolute URLs from calendar-relative paths.
	baseURL string
}

// NewFastmail builds a Fastmail Provider. It contacts the server to discover
// the user's calendar home set and enumerate all calendar collections.
func NewFastmail(ctx context.Context, cfg config.CalDAVConfig) (*FastmailProvider, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("caldav: url is required")
	}
	rawHTTP := &http.Client{Timeout: 30 * time.Second}
	httpC := webdav.HTTPClientWithBasicAuth(rawHTTP, cfg.Username, cfg.Password)
	c, err := caldav.NewClient(httpC, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("caldav client: %w", err)
	}

	// Fastmail's Cyrus server rejects PROPFIND on collection roots; PROPFIND is
	// only valid on user-specific resources. Construct the principal path directly
	// rather than using generic RFC 4791 discovery.
	principal := "/dav/principals/user/" + cfg.Username + "/"
	homeSet, err := c.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("caldav: find home set: %w", err)
	}

	calendars, err := c.FindCalendars(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("caldav: list calendars: %w", err)
	}
	if len(calendars) == 0 {
		return nil, fmt.Errorf("caldav: no calendars found under %s", homeSet)
	}

	paths := make([]string, 0, len(calendars))
	for _, cal := range calendars {
		// Skip collections that don't support VEVENT (e.g. task-only calendars).
		if supportsEvents(cal) {
			paths = append(paths, cal.Path)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("caldav: no VEVENT-capable calendars found")
	}

	return &FastmailProvider{
		cfg:           cfg,
		client:        c,
		calendarPaths: paths,
		httpClient:    httpC,
		baseURL:       strings.TrimRight(cfg.URL, "/"),
	}, nil
}

// ListEvents returns events whose start falls in [from, to], across all
// discovered calendars. The CalDAV server is asked to expand recurring
// events into individual occurrences within the window (RFC 4791 §9.6.5
// <C:expand>), so RRULE/EXDATE/RDATE/overrides are applied server-side
// rather than dropped on the floor by the client. Each expanded
// occurrence is emitted as its own RawEvent with a per-occurrence UID
// (`originalUID#startISO`) so downstream folds keyed on UID don't
// collapse multiple occurrences of the same series into one.
func (p *FastmailProvider) ListEvents(ctx context.Context, from, to time.Time) ([]RawEvent, error) {
	q := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:     ical.CompCalendar,
			AllProps: true,
			AllComps: true,
			Expand:   &caldav.CalendarExpandRequest{Start: from, End: to},
		},
		CompFilter: caldav.CompFilter{
			Name: ical.CompCalendar,
			Comps: []caldav.CompFilter{{
				Name:  ical.CompEvent,
				Start: from,
				End:   to,
			}},
		},
	}

	var out []RawEvent
	for _, path := range p.calendarPaths {
		objs, err := p.client.QueryCalendar(ctx, path, q)
		if err != nil {
			return nil, fmt.Errorf("query calendar: %w", err)
		}
		for _, o := range objs {
			occs, err := splitOccurrences(o.Data)
			if err != nil || len(occs) == 0 {
				continue
			}
			for _, occ := range occs {
				out = append(out, RawEvent{UID: occ.uid, ICS: occ.ics, ETag: o.ETag, Path: o.Path})
			}
		}
	}
	return out, nil
}

// occurrence is one expanded VEVENT plus its synthesized per-occurrence
// UID. For non-recurring events the UID equals the original VEVENT's UID;
// for an expanded occurrence we append `#startISO` so multiple occurrences
// of the same series get distinct keys downstream.
type occurrence struct {
	uid string
	ics string
}

// splitOccurrences turns a CalDAV response object (which after server-side
// <C:expand> may contain multiple VEVENT children — one per occurrence in
// the window) into one occurrence per VEVENT. VTIMEZONE blocks are
// preserved in each split so TZID-anchored times still resolve correctly.
func splitOccurrences(cal *ical.Calendar) ([]occurrence, error) {
	if cal == nil {
		return nil, fmt.Errorf("nil calendar")
	}

	var (
		vevents   []*ical.Component
		timezones []*ical.Component
	)
	for _, child := range cal.Children {
		switch child.Name {
		case ical.CompEvent:
			vevents = append(vevents, child)
		case "VTIMEZONE":
			timezones = append(timezones, child)
		}
	}
	if len(vevents) == 0 {
		return nil, nil
	}

	hasMultiple := len(vevents) > 1
	out := make([]occurrence, 0, len(vevents))
	for _, ve := range vevents {
		uid := veventText(ve, ical.PropUID)
		if uid == "" {
			continue
		}
		// Build a single-VEVENT calendar that preserves the parent's
		// header props and any VTIMEZONE definitions.
		single := ical.NewCalendar()
		single.Props = cal.Props
		for _, tz := range timezones {
			single.Children = append(single.Children, tz)
		}
		single.Children = append(single.Children, ve)

		ics, err := encodeCalendar(single)
		if err != nil {
			continue
		}

		// Per-occurrence UID: synthesize when this VEVENT is an
		// expanded occurrence (RECURRENCE-ID present), or when the
		// response carries multiple VEVENTs sharing one UID
		// (defensive — covers servers that omit RECURRENCE-ID after
		// expansion). Single non-recurring events keep their UID
		// unchanged so existing log entries remain stable.
		recID := veventText(ve, ical.PropRecurrenceID)
		if recID != "" || hasMultiple {
			start := veventText(ve, ical.PropDateTimeStart)
			if start == "" {
				start = recID
			}
			uid = uid + "#" + start
		}
		out = append(out, occurrence{uid: uid, ics: ics})
	}
	return out, nil
}

// veventText returns the raw text value of a VEVENT property, or "" when
// missing. Used for UID/RECURRENCE-ID/DTSTART lookups in
// splitOccurrences — we just need the string form to build a stable
// per-occurrence key, not a parsed time.
func veventText(ve *ical.Component, name string) string {
	if ve == nil {
		return ""
	}
	prop := ve.Props.Get(name)
	if prop == nil {
		return ""
	}
	return prop.Value
}

// GetEvent fetches a single event by UID across all configured
// calendars. Returns nil when not found. The returned RawEvent's Path
// is required for UpdateEvent/DeleteEvent.
func (p *FastmailProvider) GetEvent(ctx context.Context, uid string) (*RawEvent, error) {
	if uid == "" {
		return nil, fmt.Errorf("caldav: uid is required")
	}
	q := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:     ical.CompCalendar,
			AllProps: true,
			AllComps: true,
		},
		CompFilter: caldav.CompFilter{
			Name: ical.CompCalendar,
			Comps: []caldav.CompFilter{{
				Name: ical.CompEvent,
				Props: []caldav.PropFilter{{
					Name:      ical.PropUID,
					TextMatch: &caldav.TextMatch{Text: uid},
				}},
			}},
		},
	}
	for _, path := range p.calendarPaths {
		objs, err := p.client.QueryCalendar(ctx, path, q)
		if err != nil {
			continue
		}
		for _, o := range objs {
			ics, err := encodeCalendar(o.Data)
			if err != nil {
				continue
			}
			if primaryUID(o.Data) == uid {
				return &RawEvent{UID: uid, ICS: ics, ETag: o.ETag, Path: o.Path}, nil
			}
		}
	}
	return nil, nil
}

// PutEvent creates a new calendar object on the user's primary
// calendar (calendarPaths[0]). The path is generated from the UID.
// Uses If-None-Match: * to prevent silently overwriting an existing
// event with the same UID.
func (p *FastmailProvider) PutEvent(ctx context.Context, uid, ics string) (string, string, error) {
	if uid == "" || ics == "" {
		return "", "", fmt.Errorf("caldav: uid and ics are required")
	}
	if len(p.calendarPaths) == 0 {
		return "", "", fmt.Errorf("caldav: no calendar paths discovered")
	}
	objPath := strings.TrimRight(p.calendarPaths[0], "/") + "/" + url.PathEscape(uid) + ".ics"
	etag, err := p.rawPut(ctx, objPath, ics, "*", "")
	if err != nil {
		return "", "", err
	}
	return etag, objPath, nil
}

// UpdateEvent rewrites an existing calendar object using If-Match
// against the previous ETag. Returns ErrPreconditionFailed (412) when
// the server's ETag has moved on.
func (p *FastmailProvider) UpdateEvent(ctx context.Context, path, ics, ifMatchETag string) (string, error) {
	if path == "" || ics == "" {
		return "", fmt.Errorf("caldav: path and ics are required")
	}
	return p.rawPut(ctx, path, ics, "", ifMatchETag)
}

// DeleteEvent removes a calendar object. 404 is success (object is
// gone, which is the user's intent); 412 is ErrPreconditionFailed.
func (p *FastmailProvider) DeleteEvent(ctx context.Context, path, ifMatchETag string) error {
	if path == "" {
		return fmt.Errorf("caldav: path is required")
	}
	u, err := p.absoluteURL(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	if ifMatchETag != "" {
		req.Header.Set("If-Match", ifMatchETag)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return nil
	case resp.StatusCode == http.StatusPreconditionFailed:
		return ErrPreconditionFailed
	default:
		return fmt.Errorf("caldav DELETE %s: status %d", u, resp.StatusCode)
	}
}

// rawPut is the conditional-PUT helper. Either ifNoneMatch or
// ifMatchETag may be set (or both empty for an unconditional PUT).
// Returns the new ETag from the response, when present.
func (p *FastmailProvider) rawPut(ctx context.Context, objPath, ics, ifNoneMatch, ifMatchETag string) (string, error) {
	u, err := p.absoluteURL(objPath)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, strings.NewReader(ics))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", ical.MIMEType)
	req.ContentLength = int64(len(ics))
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if ifMatchETag != "" {
		req.Header.Set("If-Match", ifMatchETag)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.Header.Get("ETag"), nil
	case resp.StatusCode == http.StatusPreconditionFailed:
		return "", ErrPreconditionFailed
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("caldav PUT %s: status %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// absoluteURL turns a CalDAV path (possibly relative) into a full URL
// rooted at the configured server.
func (p *FastmailProvider) absoluteURL(objPath string) (string, error) {
	if objPath == "" {
		return "", fmt.Errorf("caldav: empty path")
	}
	if strings.HasPrefix(objPath, "http://") || strings.HasPrefix(objPath, "https://") {
		return objPath, nil
	}
	base, err := url.Parse(p.baseURL)
	if err != nil {
		return "", err
	}
	rel, err := url.Parse(objPath)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(rel).String(), nil
}

// supportsEvents returns true when a calendar's SupportedComponentSet includes
// VEVENT, or when the set is empty (server didn't advertise it — assume yes).
func supportsEvents(cal caldav.Calendar) bool {
	if len(cal.SupportedComponentSet) == 0 {
		return true
	}
	for _, c := range cal.SupportedComponentSet {
		if c == ical.CompEvent {
			return true
		}
	}
	return false
}

func encodeCalendar(cal *ical.Calendar) (string, error) {
	if cal == nil {
		return "", fmt.Errorf("nil calendar")
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func primaryUID(cal *ical.Calendar) string {
	if cal == nil {
		return ""
	}
	for _, e := range cal.Events() {
		if uid, _ := e.Props.Text(ical.PropUID); uid != "" {
			return uid
		}
	}
	return ""
}
