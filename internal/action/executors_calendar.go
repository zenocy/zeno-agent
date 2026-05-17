package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	logp "github.com/zenocy/zeno-v2/internal/log"
	caldavsensor "github.com/zenocy/zeno-v2/internal/sensor/caldav"
)

// CalendarDeps bundles the dependencies the calendar Executors share.
// Constructed once at boot.
type CalendarDeps struct {
	Provider caldavsensor.Provider
	UserMail string // mailto target for RSVP attendee matching; usually CalDAV username if it's an email
	Logger   *logrus.Entry
}

// resolveStartEnd takes a Target map and resolves the start/end times
// into absolute time.Time values in the user's timezone.
//
// Accepted target keys (in priority order):
//   - "start_iso" / "end_iso": full RFC3339 timestamps; used as-is.
//   - "start" / "end": permissive — accepts "HH:MM" wall-clock combined
//     with target.date (or ec.Today), OR a full datetime string the LLM
//     may have inlined here instead of routing to *_iso. Datetime-shaped
//     values are tried as RFC3339 first, then naive
//     "2006-01-02T15:04:05" (parsed in the user's tz).
//   - "title" / "description" / "location": pass-through to the spec.
//
// The permissive `start`/`end` parsing exists because LLMs frequently
// inline an RFC3339-ish value into `start` even when prompted to use
// HH:MM. Pre-fix, that produced "2026-05-10 2026-05-10T19:00:00" via
// blind concatenation with `date` and a confusing parser error at
// click time. Now any T-bearing value is recognized as a full
// datetime and used directly.
//
// Returns a non-nil error when the inputs are unparseable.
func resolveStartEnd(target map[string]any, today string, tz *time.Location) (time.Time, time.Time, error) {
	if tz == nil {
		tz = time.UTC
	}
	if v := stringFromTarget(target, "start_iso"); v != "" {
		start, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("start_iso: %w", err)
		}
		endStr := stringFromTarget(target, "end_iso")
		if endStr == "" {
			return time.Time{}, time.Time{}, errors.New("end_iso required when start_iso is set")
		}
		end, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end_iso: %w", err)
		}
		return start, end, nil
	}

	startStr := stringFromTarget(target, "start")
	endStr := stringFromTarget(target, "end")
	if startStr == "" || endStr == "" {
		return time.Time{}, time.Time{}, errors.New("target.start and target.end are required (HH:MM or RFC3339)")
	}
	dateStr := stringFromTarget(target, "date")
	if dateStr == "" {
		dateStr = today
	}
	if dateStr == "" {
		dateStr = time.Now().In(tz).Format("2006-01-02")
	}
	start, err := parseTargetTime(startStr, dateStr, tz)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse start: %w", err)
	}
	end, err := parseTargetTime(endStr, dateStr, tz)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse end: %w", err)
	}
	if !end.After(start) {
		// Allow a same-time pair to mean "1h duration".
		end = start.Add(time.Hour)
	}
	return start, end, nil
}

// parseTargetTime accepts either an HH:MM wall-clock value (combined
// with dateStr in tz) or an LLM-inlined full datetime ("2026-05-10T19:00:00",
// "2026-05-10T19:00:00+03:00", "2026-05-10T19:00:00Z"). The datetime
// detection is just "contains a T" — narrower checks miss real-world
// LLM output variations and the formats below are tried in order so
// false positives still produce a real datetime.
func parseTargetTime(value, dateStr string, tz *time.Location) (time.Time, error) {
	if strings.Contains(value, "T") {
		// Try RFC3339 first (carries explicit tz).
		if t, err := time.Parse(time.RFC3339, value); err == nil {
			return t, nil
		}
		// Fall back to naive datetime (no tz) anchored in the user's tz.
		if t, err := time.ParseInLocation("2006-01-02T15:04:05", value, tz); err == nil {
			return t, nil
		}
		if t, err := time.ParseInLocation("2006-01-02T15:04", value, tz); err == nil {
			return t, nil
		}
		return time.Time{}, fmt.Errorf("unrecognized datetime %q (tried RFC3339 and naive forms)", value)
	}
	// Wall-clock: HH:MM combined with dateStr in tz.
	t, err := time.ParseInLocation("2006-01-02 15:04", dateStr+" "+value, tz)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// ----------------------------------------------------------------------
// AddEventExec — preflighted creation of a new calendar event.
// ----------------------------------------------------------------------

type AddEventExec struct {
	Deps CalendarDeps
}

func (e *AddEventExec) Mode() Mode { return ModePreflight }

func (e *AddEventExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Deps.Provider == nil {
		return Result{OK: false, Toast: "CalDAV not configured."}, nil
	}
	title := stringFromTarget(ec.Target, "title")
	if title == "" {
		return Result{OK: false, Toast: "target.title is required."}, nil
	}
	start, end, err := resolveStartEnd(ec.Target, ec.Today, ec.TZ)
	if err != nil {
		return Result{OK: false, Toast: "Could not parse times: " + err.Error()}, nil
	}

	spec := caldavsensor.EventSpec{
		Title:       title,
		Start:       start,
		End:         end,
		Location:    stringFromTarget(ec.Target, "location"),
		Description: stringFromTarget(ec.Target, "description"),
	}

	if !ec.Confirm {
		return Result{
			OK:           true,
			NeedsConfirm: true,
			Preview: map[string]any{
				"title":       spec.Title,
				"start":       start.Format(time.RFC3339),
				"end":         end.Format(time.RFC3339),
				"location":    spec.Location,
				"description": spec.Description,
			},
		}, nil
	}

	ics, uid, err := caldavsensor.BuildEventICS(spec)
	if err != nil {
		return Result{OK: false, Toast: "Could not build event."}, err
	}
	etag, path, err := e.Deps.Provider.PutEvent(ctx, uid, ics)
	if err != nil {
		return Result{OK: false, Toast: "CalDAV PUT failed."}, err
	}
	return Result{
		OK:        true,
		EventKind: logp.KindCalEventCreated,
		EventPayload: map[string]any{
			"uid": uid, "path": path, "etag": etag,
			"title": title, "start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339),
		},
		Toast: fmt.Sprintf("Added: %s", title),
	}, nil
}

// ----------------------------------------------------------------------
// BlockCalendarExec — 1-click block on the user's primary calendar.
// ----------------------------------------------------------------------

type BlockCalendarExec struct {
	Deps CalendarDeps
}

func (e *BlockCalendarExec) Mode() Mode { return Mode1Click }

func (e *BlockCalendarExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Deps.Provider == nil {
		return Result{OK: false, Toast: "CalDAV not configured."}, nil
	}
	title := stringFromTarget(ec.Target, "title")
	if title == "" {
		title = "Block"
	}
	start, end, err := resolveStartEnd(ec.Target, ec.Today, ec.TZ)
	if err != nil {
		return Result{OK: false, Toast: "Could not parse times: " + err.Error()}, nil
	}
	spec := caldavsensor.EventSpec{
		Title:      title,
		Start:      start,
		End:        end,
		Categories: []string{"personal"}, // surfaces as src=personal next morning
	}
	ics, uid, err := caldavsensor.BuildEventICS(spec)
	if err != nil {
		return Result{OK: false, Toast: "Could not build block."}, err
	}
	etag, path, err := e.Deps.Provider.PutEvent(ctx, uid, ics)
	if err != nil {
		return Result{OK: false, Toast: "CalDAV PUT failed."}, err
	}
	return Result{
		OK:        true,
		EventKind: logp.KindCalEventBlocked,
		EventPayload: map[string]any{
			"uid": uid, "path": path, "etag": etag,
			"title": title, "start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339),
		},
		Toast: fmt.Sprintf("Blocked %s–%s.", start.Format("15:04"), end.Format("15:04")),
	}, nil
}

// ----------------------------------------------------------------------
// RSVP Executors — set ATTENDEE PARTSTAT on an existing event.
// ----------------------------------------------------------------------

type rsvpExec struct {
	Deps   CalendarDeps
	status caldavsensor.PartStat
	verb   string // "yes" / "no" / "maybe" — used in toast text
}

func (e *rsvpExec) Mode() Mode { return Mode1Click }

func (e *rsvpExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Deps.Provider == nil {
		return Result{OK: false, Toast: "CalDAV not configured."}, nil
	}
	uid := stringFromTarget(ec.Target, "uid")
	if uid == "" {
		uid = stringFromTarget(ec.Target, "event_uid")
	}
	if uid == "" {
		return Result{OK: false, Toast: "RSVP needs target.uid."}, nil
	}

	src, err := e.Deps.Provider.GetEvent(ctx, uid)
	if err != nil {
		return Result{OK: false, Toast: "Could not read source event."}, err
	}
	if src == nil {
		return Result{OK: false, Toast: "Event not found."}, nil
	}

	updated, err := caldavsensor.SetAttendeePartStat(src.ICS, e.Deps.UserMail, e.status)
	if err != nil {
		return Result{OK: false, Toast: "Could not update attendee status."}, err
	}

	newETag, err := e.Deps.Provider.UpdateEvent(ctx, src.Path, updated, src.ETag)
	if err != nil {
		if errors.Is(err, caldavsensor.ErrPreconditionFailed) {
			return Result{OK: false, Toast: "Event was changed elsewhere — open it to retry."}, err
		}
		return Result{OK: false, Toast: "CalDAV PUT failed."}, err
	}

	title, _, _, _ := caldavsensor.EventSummary(updated)
	if title == "" {
		title = uid
	}

	return Result{
		OK:        true,
		EventKind: logp.KindCalRSVPSent,
		EventPayload: map[string]any{
			"uid": uid, "path": src.Path, "etag": newETag,
			"partstat": string(e.status), "title": title,
		},
		Toast: fmt.Sprintf("RSVP %s: %s", e.verb, title),
	}, nil
}

// RsvpYesExec / RsvpNoExec / RsvpMaybeExec are thin wrappers so the
// Registry can hold typed Executor values rather than a configured
// rsvpExec each time. Keeps the boot-time Register calls readable.

type RsvpYesExec struct{ Deps CalendarDeps }

func (e *RsvpYesExec) Mode() Mode { return Mode1Click }
func (e *RsvpYesExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	return (&rsvpExec{Deps: e.Deps, status: caldavsensor.PartStatAccepted, verb: "yes"}).Execute(ctx, ec)
}

type RsvpNoExec struct{ Deps CalendarDeps }

func (e *RsvpNoExec) Mode() Mode { return Mode1Click }
func (e *RsvpNoExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	return (&rsvpExec{Deps: e.Deps, status: caldavsensor.PartStatDeclined, verb: "no"}).Execute(ctx, ec)
}

type RsvpMaybeExec struct{ Deps CalendarDeps }

func (e *RsvpMaybeExec) Mode() Mode { return Mode1Click }
func (e *RsvpMaybeExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	return (&rsvpExec{Deps: e.Deps, status: caldavsensor.PartStatTentative, verb: "maybe"}).Execute(ctx, ec)
}

// ----------------------------------------------------------------------
// RescheduleEventExec — change DTSTART/DTEND on an existing event.
// Preflight: the user verifies the new time before the PUT commits.
// ----------------------------------------------------------------------

type RescheduleEventExec struct {
	Deps CalendarDeps
}

func (e *RescheduleEventExec) Mode() Mode { return ModePreflight }

func (e *RescheduleEventExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Deps.Provider == nil {
		return Result{OK: false, Toast: "CalDAV not configured."}, nil
	}
	uid := stringFromTarget(ec.Target, "uid")
	if uid == "" {
		uid = stringFromTarget(ec.Target, "event_uid")
	}
	if uid == "" {
		return Result{OK: false, Toast: "Reschedule needs target.uid."}, nil
	}
	newStart, newEnd, err := resolveStartEnd(ec.Target, ec.Today, ec.TZ)
	if err != nil {
		return Result{OK: false, Toast: "Could not parse new times: " + err.Error()}, nil
	}
	src, err := e.Deps.Provider.GetEvent(ctx, uid)
	if err != nil {
		return Result{OK: false, Toast: "Could not read source event."}, err
	}
	if src == nil {
		return Result{OK: false, Toast: "Event not found."}, nil
	}

	if !ec.Confirm {
		title, _, _, _ := caldavsensor.EventSummary(src.ICS)
		return Result{
			OK:           true,
			NeedsConfirm: true,
			Preview: map[string]any{
				"title":     title,
				"new_start": newStart.Format(time.RFC3339),
				"new_end":   newEnd.Format(time.RFC3339),
				"uid":       uid,
			},
		}, nil
	}

	updated, err := caldavsensor.MutateEventTimes(src.ICS, newStart, newEnd)
	if err != nil {
		return Result{OK: false, Toast: "Could not build updated event."}, err
	}
	newETag, err := e.Deps.Provider.UpdateEvent(ctx, src.Path, updated, src.ETag)
	if err != nil {
		if errors.Is(err, caldavsensor.ErrPreconditionFailed) {
			return Result{OK: false, Toast: "Event was changed elsewhere — open it to retry."}, err
		}
		return Result{OK: false, Toast: "CalDAV PUT failed."}, err
	}
	title, _, _, _ := caldavsensor.EventSummary(updated)
	return Result{
		OK:        true,
		EventKind: logp.KindCalEventRescheduled,
		EventPayload: map[string]any{
			"uid": uid, "path": src.Path, "etag": newETag,
			"title": title,
			"start": newStart.Format(time.RFC3339), "end": newEnd.Format(time.RFC3339),
		},
		Toast: fmt.Sprintf("Rescheduled: %s", title),
	}, nil
}

// ----------------------------------------------------------------------
// CancelEventExec — DELETE an existing event. ModeConfirm: the modal
// collects confirmation; there is no preview body.
// ----------------------------------------------------------------------

type CancelEventExec struct {
	Deps CalendarDeps
}

func (e *CancelEventExec) Mode() Mode { return ModeConfirm }

func (e *CancelEventExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Deps.Provider == nil {
		return Result{OK: false, Toast: "CalDAV not configured."}, nil
	}
	if !ec.Confirm {
		return Result{OK: false, Toast: "Confirmation required."}, nil
	}
	uid := stringFromTarget(ec.Target, "uid")
	if uid == "" {
		uid = stringFromTarget(ec.Target, "event_uid")
	}
	if uid == "" {
		return Result{OK: false, Toast: "Cancel needs target.uid."}, nil
	}
	src, err := e.Deps.Provider.GetEvent(ctx, uid)
	if err != nil {
		return Result{OK: false, Toast: "Could not read source event."}, err
	}
	if src == nil {
		// Already gone — treat as success (idempotent).
		return Result{
			OK:           true,
			EventKind:    logp.KindCalEventCancelled,
			EventPayload: map[string]any{"uid": uid, "already_gone": true},
			Toast:        "Event was already gone.",
		}, nil
	}
	title, _, _, _ := caldavsensor.EventSummary(src.ICS)
	if err := e.Deps.Provider.DeleteEvent(ctx, src.Path, src.ETag); err != nil {
		if errors.Is(err, caldavsensor.ErrPreconditionFailed) {
			return Result{OK: false, Toast: "Event was changed elsewhere — open it to retry."}, err
		}
		return Result{OK: false, Toast: "CalDAV DELETE failed."}, err
	}
	return Result{
		OK:        true,
		EventKind: logp.KindCalEventCancelled,
		EventPayload: map[string]any{
			"uid": uid, "path": src.Path, "title": title,
		},
		Toast: fmt.Sprintf("Cancelled: %s", title),
	}, nil
}

// userMailtoFromConfig is a convenience to assemble the mailto: form
// the action handler injects into RSVP Executors. CalDAV usernames
// are usually the user's primary email; pass that or override at the
// Settings UI level.
func UserMailtoFromConfig(username string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return ""
	}
	if strings.Contains(username, "@") {
		return "mailto:" + username
	}
	return ""
}
