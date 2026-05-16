package action

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// DismissExec hides the card permanently. Reactive cards (Card==nil)
// log the action and succeed without a DB hit — same posture as the
// pre-V2.8 handler so the UI never sees a dismiss fail.
type DismissExec struct {
	Cards *store.CardRepo
}

func (e *DismissExec) Mode() Mode { return Mode1Click }

func (e *DismissExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if ec.Card != nil && e.Cards != nil {
		if err := e.Cards.SetDismissed(ctx, ec.Card.ID); err != nil {
			return Result{OK: false, Toast: "Could not dismiss card."}, err
		}
	}
	return Result{OK: true, OptimisticHide: true}, nil
}

// SnoozeExec hides the card for the rest of today. ExecCtx.Today
// supplies the date in the user's timezone.
type SnoozeExec struct {
	Cards *store.CardRepo
}

func (e *SnoozeExec) Mode() Mode { return Mode1Click }

func (e *SnoozeExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if ec.Card != nil && e.Cards != nil {
		if err := e.Cards.SetSnoozed(ctx, ec.Card.ID, ec.Today); err != nil {
			return Result{OK: false, Toast: "Could not snooze card."}, err
		}
	}
	return Result{OK: true, OptimisticHide: true}, nil
}

// PinCardExec marks the card as pinned so it survives across day
// boundaries; UnpinCardExec is the inverse. Both are 1-click and
// no-op safely when the card is reactive (Card==nil).
type PinCardExec struct{ Cards *store.CardRepo }

func (e *PinCardExec) Mode() Mode { return Mode1Click }
func (e *PinCardExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if ec.Card == nil || e.Cards == nil {
		return Result{OK: true}, nil // reactive — no DB row to pin
	}
	if err := e.Cards.SetPinned(ctx, ec.Card.ID, true); err != nil {
		return Result{OK: false, Toast: "Could not pin card."}, err
	}
	return Result{
		OK:           true,
		EventKind:    logp.KindCardPinned,
		EventPayload: map[string]any{"card_id": ec.Card.ID, "title": ec.Card.Title},
		Toast:        "Pinned.",
	}, nil
}

type UnpinCardExec struct{ Cards *store.CardRepo }

func (e *UnpinCardExec) Mode() Mode { return Mode1Click }
func (e *UnpinCardExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if ec.Card == nil || e.Cards == nil {
		return Result{OK: true}, nil
	}
	if err := e.Cards.SetPinned(ctx, ec.Card.ID, false); err != nil {
		return Result{OK: false, Toast: "Could not unpin card."}, err
	}
	return Result{
		OK:           true,
		EventKind:    logp.KindCardUnpinned,
		EventPayload: map[string]any{"card_id": ec.Card.ID, "title": ec.Card.Title},
		Toast:        "Unpinned.",
	}, nil
}

// SetReminderExec sets a fire_at on a task — V2.11 unification of the
// V2.8.1 reminders surface. Two modes, picked by what's in target:
//
//   - target.task_uid present → UPDATE that task's fire_at (the
//     "attach an alarm to an existing todo" flow used by the
//     /api/tasks/:uid/reminder route).
//   - otherwise → INSERT a new task with fire_at set + source_card_id
//     populated from the card context (the "remind me about this card"
//     flow used by briefing-card actions).
//
// Target keys:
//   - when: RFC3339 timestamp OR relative offset like "+30m", "+2h", "+1d"
//   - title (optional, defaults to source card title; ignored on update)
//   - body  (optional, defaults to source card sub)
//   - task_uid (optional; switches to update mode)
type SetReminderExec struct {
	Tasks *store.TaskRepo
	TZ    func() *time.Location
}

func (e *SetReminderExec) Mode() Mode { return Mode1Click }

func (e *SetReminderExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Tasks == nil {
		return Result{OK: false, Toast: "Tasks store not configured."}, nil
	}
	whenStr := stringFromTarget(ec.Target, "when")
	if whenStr == "" {
		whenStr = stringFromTarget(ec.Target, "at")
	}
	if whenStr == "" {
		return Result{OK: false, Toast: "target.when is required."}, nil
	}

	now := ec.Now
	if now.IsZero() {
		now = time.Now()
	}
	tz := ec.TZ
	if tz == nil && e.TZ != nil {
		tz = e.TZ()
	}
	if tz == nil {
		tz = time.UTC
	}

	due, err := parseWhen(whenStr, now, tz)
	if err != nil {
		return Result{OK: false, Toast: "Could not parse when: " + err.Error()}, nil
	}
	if !due.After(now) {
		return Result{OK: false, Toast: "Reminder time must be in the future."}, nil
	}

	if taskUID := strings.TrimSpace(stringFromTarget(ec.Target, "task_uid")); taskUID != "" {
		// Update mode — attach the alarm to an existing task.
		cur, err := e.Tasks.Get(ctx, taskUID)
		if err != nil {
			return Result{OK: false, Toast: "Could not look up task."}, err
		}
		if cur == nil {
			return Result{OK: false, Toast: "Task not found."}, nil
		}
		if err := e.Tasks.SetFireAt(ctx, taskUID, &due); err != nil {
			return Result{OK: false, Toast: "Could not set reminder."}, err
		}
		return Result{
			OK:        true,
			EventKind: logp.KindReminderSet,
			EventPayload: map[string]any{
				"task_uid":    taskUID,
				"fire_at":     due.Format(time.RFC3339),
				"title":       cur.Title,
				"source_card": cur.SourceCardID,
			},
			Toast: fmt.Sprintf("Will remind you at %s.", due.In(tz).Format("Mon 15:04")),
		}, nil
	}

	// Insert mode — create a new task carrying the alarm.
	title := stringFromTarget(ec.Target, "title")
	body := stringFromTarget(ec.Target, "body")
	var sourceID string
	if ec.Card != nil {
		sourceID = ec.Card.ID
		if title == "" {
			title = ec.Card.Title
		}
		if body == "" {
			body = ec.Card.Sub
		}
	}
	if title == "" {
		title = "Reminder"
	}

	row := store.Task{
		ID:           uuid.NewString(),
		Title:        title,
		Body:         body,
		FireAt:       &due,
		SourceCardID: sourceID,
		CreatedAt:    now,
	}
	if err := e.Tasks.Insert(ctx, row); err != nil {
		return Result{OK: false, Toast: "Could not save reminder."}, err
	}
	return Result{
		OK:        true,
		EventKind: logp.KindReminderSet,
		EventPayload: map[string]any{
			"task_uid":    row.ID,
			"fire_at":     row.FireAt.Format(time.RFC3339),
			"title":       row.Title,
			"source_card": row.SourceCardID,
		},
		Toast: fmt.Sprintf("Will remind you at %s.", due.In(tz).Format("Mon 15:04")),
	}, nil
}

// parseWhen accepts an RFC3339 timestamp or a relative offset like
// "+30m" / "+2h" / "+1d" and returns the absolute due time.
func parseWhen(s string, now time.Time, tz *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "+") {
		d, err := store.ParseRelative(s)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d), nil
	}
	// RFC3339 first; if that fails fall back to "YYYY-MM-DD HH:MM" in TZ.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, tz); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized format %q (expected RFC3339, +Xh/+Xd, or YYYY-MM-DD HH:MM)", s)
}

// OpenURLExec is a sentinel executor: the URL opening happens in the
// browser tab, server-side this is a no-op that records the click. The
// UI is responsible for window.open(target.url). Server validates the
// URL is non-empty so a misemitted button doesn't open about:blank.
type OpenURLExec struct{}

func (e *OpenURLExec) Mode() Mode { return Mode1Click }

func (e *OpenURLExec) Execute(_ context.Context, ec ExecCtx) (Result, error) {
	url, _ := ec.Target["url"].(string)
	if url == "" {
		return Result{OK: false, Toast: "No URL on this action."}, nil
	}
	return Result{OK: true}, nil
}
