package schedule

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.11 — the sweeper now operates on tasks-with-fire_at instead of a
// dedicated reminders table. Functionally identical to V2.8: every
// `Tick` it queries due alarms (FireAt <= now AND FiredAt IS NULL),
// marks each fired (race-guarded), then dispatches via inject +
// optional WhatsApp.

// ReminderInjector is the subset of the scheduler the sweeper depends
// on — letting tests inject a stub without dragging in cron.
type ReminderInjector interface {
	RunInjectNowWithSignal(ctx context.Context, signal any) error
}

// WhatsAppSender is the seam the sweeper uses to dispatch a fired
// reminder over WhatsApp. The whatsapp.Service swaps clients across
// re-pair, so production wires this as a closure that resolves the
// live client at send time, not the one captured at boot.
type WhatsAppSender interface {
	SendText(ctx context.Context, to, text string) error
}

// ReminderSweeperDeps bundles the dependencies. Now is overridable
// for tests; production passes time.Now.
type ReminderSweeperDeps struct {
	Tasks       *store.TaskRepo
	Injector    ReminderInjector
	BuildSignal func(t store.Task, at time.Time) any // returns a *synth.InjectSignal-typed value (any to avoid the synth import cycle)
	Logger      *logrus.Entry
	Now         func() time.Time
	Tick        time.Duration // default 60s
	Burst       int           // max alarms fired per tick; default 10

	// V2.9: outbound dispatch.
	InjectEnabled  bool                  // when false, skip the inject pipeline call
	WhatsAppSender func() WhatsAppSender // closure so the live client is used after re-pair
	WhatsAppTo     string                // recipient JID; empty disables WA dispatch even when Sender is set
	EventLog       logp.Writer           // when non-nil, emits KindTaskAlarmFired with a dispatch summary
}

// RunReminderSweeper polls the tasks table on a fixed interval and
// fires due alarms into the inject pipeline. Process-lifetime
// goroutine — cancel via the passed context. The sweeper is a no-op
// when Tasks or Injector is nil so callers can wire conditionally.
func RunReminderSweeper(ctx context.Context, deps ReminderSweeperDeps) {
	if deps.Tasks == nil || deps.Injector == nil {
		return
	}
	tick := deps.Tick
	if tick <= 0 {
		tick = 60 * time.Second
	}
	burst := deps.Burst
	if burst <= 0 {
		burst = 10
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepReminders(ctx, deps, now(), burst)
		}
	}
}

// sweepReminders drains up to burst rows whose FireAt has passed and
// dispatches each via the configured channels. The MarkFired call uses
// a WHERE-on-fired_at guard so a clear-while-firing race resolves to
// "didn't fire" and the dispatch is skipped.
func sweepReminders(ctx context.Context, deps ReminderSweeperDeps, at time.Time, burst int) {
	repo := deps.Tasks
	logger := deps.Logger
	due, err := repo.DueBefore(ctx, at, burst)
	if err != nil {
		if logger != nil {
			logger.WithError(err).Warn("reminder_sweeper: query failed")
		}
		return
	}
	if len(due) == 0 {
		return
	}
	for _, r := range due {
		n, err := repo.MarkFired(ctx, r.ID, at)
		if err != nil {
			if logger != nil {
				logger.WithError(err).WithField("task_uid", r.ID).Warn("reminder_sweeper: mark fired failed")
			}
			continue
		}
		if n == 0 {
			// Race: another writer cleared fire_at or already fired
			// this row between the DueBefore query and MarkFired.
			// Drop the dispatch — the user explicitly cancelled.
			if logger != nil {
				logger.WithField("task_uid", r.ID).Debug("reminder_sweeper: alarm cleared mid-fire; skipping dispatch")
			}
			continue
		}
		dispatched := dispatchReminder(ctx, deps, r, at)
		emitTaskAlarmFired(ctx, deps, r, at, dispatched)
		if logger != nil {
			fa := ""
			if r.FireAt != nil {
				fa = r.FireAt.Format(time.RFC3339)
			}
			logger.WithFields(logrus.Fields{
				"task_uid": r.ID,
				"title":    r.Title,
				"fire_at":  fa,
				"dispatch": dispatched,
			}).Info("reminder_sweeper: alarm fired")
		}
	}
}

// dispatchReminder runs each configured outbound channel and returns
// the list of channels that succeeded. A channel is considered
// "configured" when its toggle is on AND its dependencies are present;
// missing config is a silent skip, not a failure.
func dispatchReminder(ctx context.Context, deps ReminderSweeperDeps, t store.Task, at time.Time) []string {
	out := make([]string, 0, 2)
	logger := deps.Logger

	if deps.InjectEnabled && deps.Injector != nil {
		var signal any
		if deps.BuildSignal != nil {
			signal = deps.BuildSignal(t, at)
		}
		if err := deps.Injector.RunInjectNowWithSignal(ctx, signal); err != nil {
			if logger != nil {
				logger.WithError(err).
					WithField("task_uid", t.ID).
					Warn("reminder_sweeper: inject failed; alarm marked fired so it won't repeat")
			}
		} else {
			out = append(out, "inject")
		}
	}

	if deps.WhatsAppSender != nil && deps.WhatsAppTo != "" {
		if sender := deps.WhatsAppSender(); sender != nil {
			text := formatReminderText(t)
			if err := sender.SendText(ctx, deps.WhatsAppTo, text); err != nil {
				if logger != nil {
					logger.WithError(err).
						WithField("task_uid", t.ID).
						Warn("reminder_sweeper: whatsapp send failed; alarm marked fired so it won't repeat")
				}
			} else {
				out = append(out, "whatsapp")
			}
		}
	}

	return out
}

// formatReminderText is the user-facing message string for the WA
// dispatch path.
func formatReminderText(t store.Task) string {
	if t.Body == "" {
		return fmt.Sprintf("⏰ %s", t.Title)
	}
	return fmt.Sprintf("⏰ %s\n%s", t.Title, t.Body)
}

// emitTaskAlarmFired writes the V2.11 audit row when an event log is
// wired. dispatched is the slice of channel names that succeeded
// ("inject" / "whatsapp").
func emitTaskAlarmFired(ctx context.Context, deps ReminderSweeperDeps, t store.Task, at time.Time, dispatched []string) {
	if deps.EventLog == nil {
		return
	}
	if dispatched == nil {
		dispatched = []string{}
	}
	fa := ""
	if t.FireAt != nil {
		fa = t.FireAt.Format(time.RFC3339)
	}
	payload := map[string]any{
		"task_uid": t.ID,
		"title":    t.Title,
		"fire_at":  fa,
		"fired_at": at.Format(time.RFC3339),
		"dispatch": dispatched,
	}
	if t.SourceCardID != "" {
		payload["source_card"] = t.SourceCardID
	}
	if _, err := deps.EventLog.Append(ctx, logp.KindTaskAlarmFired, "reminder_sweeper", payload); err != nil {
		if deps.Logger != nil {
			deps.Logger.WithError(err).WithField("task_uid", t.ID).Warn("reminder_sweeper: emit task.alarm_fired failed")
		}
	}
}
