package imap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

const (
	defaultPreviewMax = 4 * 1024
	defaultBatchSize  = 50
)

// Sensor is the IMAP poller. One Sync iterates every configured folder.
type Sensor struct {
	cfg        config.IMAPConfig
	reader     log.Reader
	writer     log.Writer
	dialer     Dialer
	log        *logrus.Entry
	previewMax int
	batchSize  int
}

// New constructs a Sensor with a real Dialer.
func New(cfg config.IMAPConfig, r log.Reader, w log.Writer, l *logrus.Entry) *Sensor {
	return NewWithDialer(cfg, r, w, NewRealDialer(cfg), l)
}

// NewWithDialer is the test seam: inject a stub Dialer.
func NewWithDialer(cfg config.IMAPConfig, r log.Reader, w log.Writer, d Dialer, l *logrus.Entry) *Sensor {
	return &Sensor{
		cfg:        cfg,
		reader:     r,
		writer:     w,
		dialer:     d,
		log:        l,
		previewMax: defaultPreviewMax,
		batchSize:  defaultBatchSize,
	}
}

// Name returns the sensor identifier.
func (s *Sensor) Name() string { return "imap" }

// Sync connects, logs in, and polls every configured folder. One folder's
// failure does not abort others.
func (s *Sensor) Sync(ctx context.Context) error {
	folders := s.cfg.Folders
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}

	if s.log != nil {
		s.log.WithFields(logrus.Fields{
			"host":          s.cfg.Host,
			"port":          s.cfg.Port,
			"tls":           s.cfg.TLS,
			"username_hint": redactUsername(s.cfg.Username),
			"folders":       len(folders),
		}).Info("imap: connecting")
	}

	c, err := s.dialer.Dial(ctx)
	if err != nil {
		if s.log != nil {
			s.log.WithError(err).WithFields(logrus.Fields{
				"host": s.cfg.Host,
				"port": s.cfg.Port,
			}).Warn("imap: dial failed")
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Login(s.cfg.Username, s.cfg.Password); err != nil {
		if s.log != nil {
			s.log.WithError(err).WithField("username_hint", redactUsername(s.cfg.Username)).
				Warn("imap: login failed")
		}
		return fmt.Errorf("login: %w", err)
	}
	defer func() { _ = c.Logout() }()

	var firstErr error
	for _, folder := range folders {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.syncFolder(ctx, c, folder); err != nil {
			if s.log != nil {
				s.log.WithError(err).WithField("folder", folder).Error("imap folder sync failed")
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// redactUsername renders an IMAP username as a hint without leaking the
// full address. "jamie@example.com" → "and…(20)".
func redactUsername(s string) string {
	n := len(s)
	if n == 0 {
		return ""
	}
	prefix := s
	if n > 3 {
		prefix = s[:3]
	}
	return fmt.Sprintf("%s…(%d)", prefix, n)
}

// cursorPayload is the JSON shape of an imap.cursor event.
type cursorPayload struct {
	Folder      string `json:"folder"`
	UIDValidity uint32 `json:"uidvalidity"`
	UIDNext     uint32 `json:"uidnext"`
}

// inboxSnapshotPayload is the JSON shape of an imap.inbox_snapshot
// event: the full UID set currently present in folder, sorted ascending.
// Latest-per-folder is authoritative; projections use it to filter
// mail.received events down to messages still in the folder.
type inboxSnapshotPayload struct {
	Folder      string   `json:"folder"`
	UIDValidity uint32   `json:"uidvalidity"`
	UIDs        []uint32 `json:"uids"`
}

// mailPayload is the JSON shape of a mail.received event.
type mailPayload struct {
	Folder      string    `json:"folder"`
	UID         uint32    `json:"uid"`
	UIDValidity uint32    `json:"uidvalidity"`
	MessageID   string    `json:"message_id,omitempty"`
	From        string    `json:"from,omitempty"`
	To          []string  `json:"to,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	Date        time.Time `json:"date,omitzero"`
	InReplyTo   string    `json:"in_reply_to,omitempty"`
	References  []string  `json:"references,omitempty"`
	BodyPreview string    `json:"body_preview,omitempty"`
}

func (s *Sensor) syncFolder(ctx context.Context, c Client, folder string) error {
	cursor, err := s.readCursor(ctx, folder)
	if err != nil {
		return fmt.Errorf("read cursor: %w", err)
	}

	sel, err := c.Select(folder)
	if err != nil {
		return fmt.Errorf("select %s: %w", folder, err)
	}

	lastUID := uint32(0)
	if cursor != nil && cursor.UIDValidity == sel.UIDValidity {
		lastUID = cursor.UIDNext - 1
		if cursor.UIDNext == 0 {
			lastUID = 0
		}
	}

	uids, err := c.UIDSearchAfter(folder, lastUID)
	if err != nil {
		return fmt.Errorf("uid search: %w", err)
	}
	if len(uids) == 0 {
		if s.log != nil {
			s.log.WithFields(logrus.Fields{"folder": folder, "fetched": 0, "new": 0}).
				Debug("imap: folder synced")
		}
		// Even with no new mail, write a fresh snapshot + cursor: the
		// snapshot lets projections drop mail the user archived
		// externally since last poll; the cursor lets us observe
		// UIDVALIDITY changes if the server bumps it.
		s.emitInboxSnapshot(ctx, c, folder, sel.UIDValidity)
		return s.writeCursor(ctx, folder, sel.UIDValidity, max32(sel.UIDNext, lastUID+1))
	}

	maxUID := lastUID
	written := 0
	seenUIDs := make(map[uint32]struct{}, len(uids))
	for start := 0; start < len(uids); start += s.batchSize {
		end := min(start+s.batchSize, len(uids))
		batch := uids[start:end]

		msgs, err := c.FetchEnvelopeAndBody(folder, batch)
		if err != nil {
			// Persist the cursor up to the highest successfully written UID
			// so the next run resumes there, then surface the error.
			_ = s.writeCursor(ctx, folder, sel.UIDValidity, maxUID+1)
			return fmt.Errorf("fetch batch: %w", err)
		}
		for _, m := range msgs {
			if _, dup := seenUIDs[m.UID]; dup {
				continue
			}
			seenUIDs[m.UID] = struct{}{}

			if exists, _ := s.mailAlreadyLogged(ctx, folder, m.UID, sel.UIDValidity); exists {
				continue
			}
			if err := s.writeMail(ctx, m, sel.UIDValidity); err != nil {
				_ = s.writeCursor(ctx, folder, sel.UIDValidity, maxUID+1)
				return fmt.Errorf("write mail uid=%d: %w", m.UID, err)
			}
			written++
			if m.UID > maxUID {
				maxUID = m.UID
			}
		}
	}
	if s.log != nil {
		s.log.WithFields(logrus.Fields{
			"folder":  folder,
			"fetched": len(uids),
			"new":     written,
		}).Debug("imap: folder synced")
	}
	// Snapshot before cursor: if the process crashes between the two,
	// the truth is fresh and the resume point is slightly stale —
	// strictly safer than the inverse.
	s.emitInboxSnapshot(ctx, c, folder, sel.UIDValidity)
	return s.writeCursor(ctx, folder, sel.UIDValidity, maxUID+1)
}

func (s *Sensor) readCursor(ctx context.Context, folder string) (*cursorPayload, error) {
	events, err := s.reader.ByKind(ctx, log.KindIMAPCursor)
	if err != nil {
		return nil, err
	}
	// ByKind returns oldest-first; walk in reverse to find the latest cursor
	// for this folder.
	for i := len(events) - 1; i >= 0; i-- {
		var p cursorPayload
		if err := json.Unmarshal(events[i].Payload, &p); err != nil {
			continue
		}
		if p.Folder == folder {
			return &p, nil
		}
	}
	return nil, nil
}

func (s *Sensor) writeCursor(ctx context.Context, folder string, uidvalidity, uidnext uint32) error {
	_, err := s.writer.Append(ctx, log.KindIMAPCursor, s.Name(), cursorPayload{
		Folder: folder, UIDValidity: uidvalidity, UIDNext: uidnext,
	})
	return err
}

func (s *Sensor) writeInboxSnapshot(ctx context.Context, folder string, uidvalidity uint32, uids []uint32) error {
	_, err := s.writer.Append(ctx, log.KindIMAPInboxSnapshot, s.Name(), inboxSnapshotPayload{
		Folder: folder, UIDValidity: uidvalidity, UIDs: uids,
	})
	return err
}

// emitInboxSnapshot fetches the full UID set for folder and writes a
// snapshot event. Best-effort: a search failure is logged and skipped
// (the next successful poll will restore truth) so a transient network
// blip does not abort the surrounding sync.
func (s *Sensor) emitInboxSnapshot(ctx context.Context, c Client, folder string, uidvalidity uint32) {
	allUIDs, err := c.UIDSearchAll(folder)
	if err != nil {
		if s.log != nil {
			s.log.WithError(err).WithField("folder", folder).
				Warn("imap: snapshot search failed; skipping snapshot for this poll")
		}
		return
	}
	slices.Sort(allUIDs)
	if err := s.writeInboxSnapshot(ctx, folder, uidvalidity, allUIDs); err != nil {
		if s.log != nil {
			s.log.WithError(err).WithField("folder", folder).
				Warn("imap: snapshot append failed")
		}
	}
}

// mailAlreadyLogged checks whether a mail.received event with matching
// (folder, uid, uidvalidity) exists. This is a cheap guard against
// crash-mid-batch double-writes.
func (s *Sensor) mailAlreadyLogged(ctx context.Context, folder string, uid, uidvalidity uint32) (bool, error) {
	events, err := s.reader.ByKind(ctx, log.KindMailReceived)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		var p mailPayload
		if err := json.Unmarshal(events[i].Payload, &p); err != nil {
			continue
		}
		if p.Folder == folder && p.UID == uid && p.UIDValidity == uidvalidity {
			return true, nil
		}
	}
	return false, nil
}

func (s *Sensor) writeMail(ctx context.Context, m RawMessage, uidvalidity uint32) error {
	if m.Env == nil {
		return errors.New("missing envelope")
	}
	preview, perr := ExtractPreview(m.Body, s.previewMax)
	if perr != nil && s.log != nil {
		s.log.WithError(perr).WithField("uid", m.UID).Debug("preview extraction failed")
	}

	var fromStr string
	if len(m.Env.From) > 0 {
		fromStr = NormalizeAddress(&m.Env.From[0])
	}
	var inReply string
	if len(m.Env.InReplyTo) > 0 {
		inReply = m.Env.InReplyTo[0]
	}

	payload := mailPayload{
		Folder:      m.Folder,
		UID:         m.UID,
		UIDValidity: uidvalidity,
		MessageID:   m.Env.MessageID,
		From:        fromStr,
		To:          NormalizeAddresses(m.Env.To),
		Subject:     m.Env.Subject,
		Date:        m.Env.Date,
		InReplyTo:   inReply,
		References:  m.Env.InReplyTo, // RFC 5322 References header isn't on the IMAP envelope; in_reply_to is the closest signal here. Synthesizer can re-fetch headers in Phase 2.
		BodyPreview: preview,
	}
	if _, err := s.writer.Append(ctx, log.KindMailReceived, s.Name(), payload); err != nil {
		return err
	}

	// V2.4 reactive trigger: publish strictly AFTER the successful log
	// append. The inject subscriber's detector folds projections from the
	// durable log; if publish ran before append, the freshly-arrived event
	// would be invisible at Detect time.
	evidenceID := fmt.Sprintf("%s:%d:%d", payload.Folder, payload.UID, payload.UIDValidity)
	sensor.PublishObserved(ctx, "mail.received", evidenceID, map[string]any{
		"folder":      payload.Folder,
		"uid":         payload.UID,
		"uidvalidity": payload.UIDValidity,
		"message_id":  payload.MessageID,
		"from":        payload.From,
		"subject":     payload.Subject,
	})
	return nil
}

func max32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}
