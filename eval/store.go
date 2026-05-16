package eval

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// EphemeralStore is a temp-file SQLite store seeded with a fixture's events
// and migrated for synth tables. The harness opens one per fixture run and
// removes the file after the run completes.
type EphemeralStore struct {
	Path  string
	DB    *gorm.DB
	Store log.Store
}

// NewEphemeralStore creates a temp-file SQLite store under dir (typically
// $TMPDIR) and runs the synth + log migrations.
func NewEphemeralStore(dir string) (*EphemeralStore, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "eval-*.db")
	if err != nil {
		return nil, fmt.Errorf("create temp db: %w", err)
	}
	path := f.Name()
	_ = f.Close()

	db, logStore, err := log.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	if err := synth.Migrate(db, true, false); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("migrate synth: %w", err)
	}
	// V2.11: tasks live in SQLite now (no more event-log fold).
	if err := (&store.TaskRepo{DB: db}).Migrate(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("migrate tasks: %w", err)
	}
	return &EphemeralStore{Path: path, DB: db, Store: logStore}, nil
}

// Close removes the underlying temp file. Errors are non-fatal for callers.
func (s *EphemeralStore) Close() error {
	if s == nil || s.Path == "" {
		return nil
	}
	return os.Remove(s.Path)
}

// Seed expands a fixture into observation-log events: one mail.received
// per email thread (collapsed into the single most-recent message), one
// cal.event_seen per calendar event, one weather.snapshot if weather is
// set. Event timestamps are anchored to the fixture's Today date.
func (s *EphemeralStore) Seed(ctx context.Context, f *Fixture) error {
	loc, err := time.LoadLocation(f.User.TZ)
	if err != nil {
		return fmt.Errorf("load tz: %w", err)
	}
	today, err := time.ParseInLocation("2006-01-02", f.Today, loc)
	if err != nil {
		return fmt.Errorf("parse today: %w", err)
	}

	// Email threads — one mail.received per thread; the projection groups
	// by subject so this minimal seed reconstructs the same Thread shape.
	for i, t := range f.EmailThreads {
		uid := uint32(i + 1)
		when := t.LastReceived
		if when.IsZero() {
			when = today.Add(6 * time.Hour)
		}
		mail := map[string]any{
			"folder":      "INBOX",
			"uid":         uid,
			"uidvalidity": 1,
			"message_id":  fmt.Sprintf("<eval-%s@zeno-eval>", slug(t.Subject)),
			"from":        t.LastSender,
			"to":          []string{f.User.Name},
			"subject":     t.Subject,
			"date":        when,
			"in_reply_to": "",
			"references":  []string{},
			// preview gets stored under body_preview so the read_thread tool
			// can return it; matches the IMAP sensor's payload shape.
			"body_preview": t.Preview,
		}
		if _, err := s.Store.Append(ctx, log.KindMailReceived, "eval", mail); err != nil {
			return fmt.Errorf("append mail %s: %w", t.Subject, err)
		}
	}

	// Calendar — one cal.event_seen per event. V2.3.0 P3: propagate
	// attendees + last_modified so the projection reflects the fixture's
	// declared meeting shape (powers the pre_meeting state detector and
	// the inject detector's calendar-move path).
	for _, e := range f.Calendar {
		evt := map[string]any{
			"uid":      e.UID,
			"title":    e.Title,
			"location": e.Location,
			"tag":      e.Tag,
			"start":    e.Start,
			"end":      e.End,
		}
		if len(e.Attendees) > 0 {
			evt["attendees"] = e.Attendees
		}
		if !e.LastModified.IsZero() {
			evt["last_modified"] = e.LastModified
		}
		if _, err := s.Store.Append(ctx, log.KindCalEventSeen, "eval", evt); err != nil {
			return fmt.Errorf("append calendar %s: %w", e.UID, err)
		}
	}

	// Weather — single snapshot. The hourly array is expanded into rough
	// {time, code, wind_kmh, precip_mm} rows; codes follow WMO (0=sun,
	// 3=cloud, 61=rain) so the projection's run-window logic sees them.
	if f.Weather != nil && len(f.Weather.Hours) > 0 {
		hourly := make([]map[string]any, 0, len(f.Weather.Hours))
		for _, h := range f.Weather.Hours {
			t := today.Add(parseHour(h.H) * time.Hour)
			code := wmoForIcon(h.Icon)
			precip := 0.0
			if code >= 60 {
				precip = 1.5
			}
			hourly = append(hourly, map[string]any{
				"time":      t,
				"code":      code,
				"wind_kmh":  5.0,
				"precip_mm": precip,
			})
		}
		snapshot := map[string]any{
			"captured_at": today.Add(5 * time.Hour),
			"timezone":    f.User.TZ,
			"hourly":      hourly,
		}
		if _, err := s.Store.Append(ctx, log.KindWeatherSnapshot, "eval", snapshot); err != nil {
			return fmt.Errorf("append weather: %w", err)
		}
	}

	// V2.11 open tasks — insert one row per fixture-declared task into
	// the unified tasks table. Timestamps anchor at the fixture's morning.
	taskRepo := &store.TaskRepo{DB: s.DB}
	for _, ft := range f.OpenTasks {
		title := strings.TrimSpace(ft.Title)
		if title == "" {
			return fmt.Errorf("fixture open_tasks entry missing title")
		}
		uid := ft.UID
		if uid == "" {
			uid = taskFixtureUID(title, ft.DueDate)
		}
		priority := ft.Priority
		if priority == "" {
			priority = "med"
		}
		row := store.Task{
			ID:        uid,
			Title:     title,
			Completed: ft.Completed,
			DueDate:   ft.DueDate,
			Priority:  priority,
		}
		if ft.DoneDate != "" {
			// Parse YYYY-MM-DD as midnight UTC; the projection only
			// uses CompletedAt to derive done_date.
			if d, err := time.Parse("2006-01-02", ft.DoneDate); err == nil {
				row.CompletedAt = &d
			}
		}
		if len(ft.Tags) > 0 {
			b, err := json.Marshal(ft.Tags)
			if err == nil {
				row.Tags = datatypes.JSON(b)
			}
		}
		if err := taskRepo.Insert(ctx, row); err != nil {
			return fmt.Errorf("insert task %q: %w", title, err)
		}
	}

	// V2.7 stock sensor — append one stock.snapshot per fixture entry so
	// the Stock + MarketsContext projections see prices, then one
	// stock.alert per fixture entry so RecentStockBreaches + the inject
	// detector see breach evidence. Alerts may carry a custom EvidenceID
	// — that's the durable handle the InjectSignal references.
	for _, fs := range f.StockSnapshots {
		ticker := strings.TrimSpace(fs.Ticker)
		if ticker == "" {
			return fmt.Errorf("fixture stock_snapshots entry missing ticker")
		}
		ts := stockEventTS(today, fs.MinutesAgo)
		changePct := fs.ChangePct
		if changePct == 0 && fs.PrevClose != 0 {
			changePct = (fs.Price/fs.PrevClose - 1) * 100
		}
		currency := fs.Currency
		if currency == "" {
			currency = "USD"
		}
		payload := map[string]any{
			"ticker":     ticker,
			"price":      fs.Price,
			"prev_close": fs.PrevClose,
			"currency":   currency,
			"change_pct": changePct,
			"as_of":      ts,
		}
		if fs.Open != 0 {
			payload["open"] = fs.Open
		}
		if fs.DayHigh != 0 {
			payload["day_high"] = fs.DayHigh
		}
		if fs.DayLow != 0 {
			payload["day_low"] = fs.DayLow
		}
		if fs.Volume != 0 {
			payload["volume"] = fs.Volume
		}
		if fs.PostPrice != 0 {
			payload["post_price"] = fs.PostPrice
		}
		if fs.PostChange != 0 {
			payload["post_change_pct"] = fs.PostChange
		}
		if fs.MarketState != "" {
			payload["market_state"] = fs.MarketState
		}
		if err := writeStockEvent(s.DB, "", ts, log.KindStockSnapshot, payload); err != nil {
			return fmt.Errorf("append stock snapshot %q: %w", ticker, err)
		}
	}
	for _, fa := range f.StockAlerts {
		ticker := strings.TrimSpace(fa.Ticker)
		if ticker == "" {
			return fmt.Errorf("fixture stock_alerts entry missing ticker")
		}
		ts := stockEventTS(today, fa.MinutesAgo)
		currency := fa.Currency
		if currency == "" {
			currency = "USD"
		}
		payload := map[string]any{
			"ticker":        ticker,
			"price":         fa.Price,
			"prev_close":    fa.PrevClose,
			"change_pct":    fa.ChangePct,
			"threshold_pct": fa.ThresholdPct,
			"currency":      currency,
			"as_of":          ts,
		}
		if err := writeStockEvent(s.DB, fa.EvidenceID, ts, log.KindStockAlert, payload); err != nil {
			return fmt.Errorf("append stock alert %q: %w", ticker, err)
		}
	}

	// Memory — seed the fixture's MemoryFact rows directly into memory_facts
	// so the cards-loop projection picks them up. The synth migrations from
	// NewEphemeralStore already created the table.
	if len(f.Memory) > 0 {
		repo := &store.MemoryRepo{DB: s.DB, Table: "memory_facts"}
		rows := make([]store.MemoryFact, 0, len(f.Memory))
		now := time.Now()
		for _, m := range f.Memory {
			if m.Subject == "" || m.Fact == "" {
				return fmt.Errorf("fixture memory entry missing subject or fact")
			}
			conf := m.Confidence
			if conf == "" {
				conf = "low"
			}
			src := m.Source
			if src == "" {
				src = "synth"
			}
			cat := m.Category
			if cat == "" {
				cat = "misc"
			}
			ev := m.EvidenceCount
			if ev == 0 {
				ev = 1
			}
			rows = append(rows, store.MemoryFact{
				ID:             memoryFactID(m.Subject, m.Fact),
				Subject:        m.Subject,
				Fact:           m.Fact,
				Category:       cat,
				Confidence:     conf,
				Source:         src,
				EvidenceCount:  ev,
				FirstSeen:      now,
				LastReinforced: now,
			})
		}
		if err := repo.Upsert(ctx, rows); err != nil {
			return fmt.Errorf("seed memory facts: %w", err)
		}
	}

	return nil
}

// taskFixtureUID matches the runtime tasks.MakeUID scheme so seeded fixture
// rows are de-duplicated identically to live syncs. Lower-cased + trimmed
// title, joined with the literal due_date, sha256, first 6 bytes hex.
func taskFixtureUID(title, dueDate string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(title)) + "|" + dueDate))
	return hex.EncodeToString(sum[:6])
}

// memoryFactID derives a deterministic ID from subject + fact text, matching
// the same scheme cmd/zeno/replay.go uses for fixture seeds. Re-seeding the
// same fixture produces stable IDs.
func memoryFactID(subject, fact string) string {
	subj := strings.ToLower(strings.TrimSpace(subject))
	sum := sha256.Sum256([]byte(fact))
	return subj + "-" + hex.EncodeToString(sum[:4])
}

// parseHour permissively parses "9" / "11" / "9:30" → hours as Duration units.
// Used only by the weather seed; resolution beyond hours not required.
func parseHour(s string) time.Duration {
	var h int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			h = h*10 + int(c-'0')
		} else {
			break
		}
	}
	return time.Duration(h)
}

// wmoForIcon maps the fixture's icon enum to a WMO weather code so the
// projection's RunWindow can classify dryness.
func wmoForIcon(icon string) int {
	switch icon {
	case "sun":
		return 0
	case "cloud":
		return 3
	case "rain":
		return 61
	default:
		return 0
	}
}

// slug shortens a string for use in synthetic message IDs.
func slug(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:4])
}

// PathFor returns a deterministic path under dir for fixture-named files.
func PathFor(dir, fixtureName, ext string) string {
	return filepath.Join(dir, fixtureName+"."+ext)
}

// stockEventTS anchors a stock event relative to the fixture's today
// at 09:30 ET (US market open) — close enough to "this morning" that
// MarketsContext's 24h lookback still admits the breach. minutesAgo
// shifts backward from that anchor; 0 → exactly the anchor.
func stockEventTS(today time.Time, minutesAgo int) time.Time {
	// today is at 00:00 in the fixture's local TZ. Bias 9.5h forward
	// (close enough to NY market open in any TZ for relative ordering)
	// then shift back by minutesAgo.
	base := today.Add(9*time.Hour + 30*time.Minute)
	return base.Add(-time.Duration(minutesAgo) * time.Minute).UTC()
}

// writeStockEvent appends a stock.snapshot or stock.alert with an
// optional explicit ID. Bypasses log.Store.Append so callers can
// control both the timestamp and the ID — required for inject
// fixtures whose InjectSignal.EvidenceID must match a real row.
func writeStockEvent(db *gorm.DB, id string, ts time.Time, kind string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal stock payload: %w", err)
	}
	rowID := id
	if rowID == "" {
		rowID = "evt-" + slug(kind+ts.String()+fmt.Sprintf("%v", payload["ticker"]))
	}
	ev := log.Event{
		ID:      rowID,
		TS:      ts.UTC(),
		Kind:    kind,
		Source:  "eval",
		Payload: datatypes.JSON(b),
	}
	return db.Create(&ev).Error
}
