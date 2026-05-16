package projection

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
)

// OpenEmailThreads folds mail.received events into a slice of Threads.
type OpenEmailThreads struct {
	Cfg Config
}

// Name returns the projection identifier.
func (p OpenEmailThreads) Name() string { return "open_email_threads" }

type rawMail struct {
	Folder      string    `json:"folder"`
	UID         uint32    `json:"uid"`
	UIDValidity uint32    `json:"uidvalidity"`
	MessageID   string    `json:"message_id"`
	From        string    `json:"from"`
	To          []string  `json:"to"`
	Subject     string    `json:"subject"`
	Date        time.Time `json:"date"`
	InReplyTo   string    `json:"in_reply_to"`
	References  []string  `json:"references"`
}

type rawInboxSnapshot struct {
	Folder      string   `json:"folder"`
	UIDValidity uint32   `json:"uidvalidity"`
	UIDs        []uint32 `json:"uids"`
}

// loadInboxSnapshots reads imap.inbox_snapshot events and builds:
//   - foldersWithSnapshot: folder names that have any snapshot event
//   - present: "folder:uidvalidity" → set of UIDs for the latest
//     snapshot per folder
//
// Latest is determined by event TS, not lookback. The snapshot is
// always authoritative truth.
func loadInboxSnapshots(ctx context.Context, r log.Reader) (map[string]struct{}, map[string]map[uint32]struct{}, error) {
	events, err := r.ByKind(ctx, log.KindIMAPInboxSnapshot)
	if err != nil {
		return nil, nil, err
	}
	foldersWithSnapshot := map[string]struct{}{}
	latestByFolder := map[string]rawInboxSnapshot{} // folder → latest payload
	latestTS := map[string]time.Time{}              // folder → ts of latest seen

	for _, e := range events {
		var snap rawInboxSnapshot
		if err := json.Unmarshal(e.Payload, &snap); err != nil {
			continue
		}
		foldersWithSnapshot[snap.Folder] = struct{}{}
		if prev, ok := latestTS[snap.Folder]; !ok || e.TS.After(prev) {
			latestTS[snap.Folder] = e.TS
			latestByFolder[snap.Folder] = snap
		}
	}

	present := make(map[string]map[uint32]struct{}, len(latestByFolder))
	for _, snap := range latestByFolder {
		key := snap.Folder + ":" + strconv.FormatUint(uint64(snap.UIDValidity), 10)
		set := make(map[uint32]struct{}, len(snap.UIDs))
		for _, u := range snap.UIDs {
			set[u] = struct{}{}
		}
		present[key] = set
	}
	return foldersWithSnapshot, present, nil
}

// Compute groups mails into threads, capping at OpenThreadsMax newest first.
//
// The latest imap.inbox_snapshot per folder filters mail.received events
// down to messages still present in that folder. Snapshots are NOT
// subject to the lookback window — the latest is always authoritative,
// since it represents current IMAP truth, not historical signal. A
// folder with no snapshot at all (e.g. on first deploy) defaults to
// include-all so the briefing isn't blanked.
func (p OpenEmailThreads) Compute(ctx context.Context, r log.Reader) ([]Thread, error) {
	cap_ := p.Cfg.OpenThreadsMax
	if cap_ <= 0 {
		cap_ = 20
	}

	since := p.Cfg.now().Add(-p.Cfg.lookback())

	foldersWithSnapshot, present, err := loadInboxSnapshots(ctx, r)
	if err != nil {
		return nil, err
	}

	events, err := r.ByKind(ctx, log.KindMailReceived)
	if err != nil {
		return nil, err
	}

	type bucket struct {
		Subject      string
		LastSender   string
		LastReceived time.Time
		Count        int
	}
	buckets := make(map[string]*bucket)

	for _, e := range events {
		if e.TS.Before(since) {
			continue
		}
		var m rawMail
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			continue
		}
		if _, anySnap := foldersWithSnapshot[m.Folder]; anySnap {
			folderKey := m.Folder + ":" + strconv.FormatUint(uint64(m.UIDValidity), 10)
			uids, ok := present[folderKey]
			if !ok {
				continue // snapshot exists for this folder but uidvalidity doesn't match (post-bump stale)
			}
			if _, ok := uids[m.UID]; !ok {
				continue // vanished externally (user moved/deleted in their own client)
			}
		}
		key := threadKey(m)
		when := m.Date
		if when.IsZero() {
			when = e.TS
		}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &bucket{
				Subject:      m.Subject,
				LastSender:   m.From,
				LastReceived: when,
				Count:        1,
			}
			continue
		}
		b.Count++
		if when.After(b.LastReceived) {
			b.LastReceived = when
			b.LastSender = m.From
			if m.Subject != "" {
				b.Subject = m.Subject
			}
		}
	}

	out := make([]Thread, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, Thread{
			Subject:      b.Subject,
			LastSender:   b.LastSender,
			LastReceived: b.LastReceived,
			MessageCount: b.Count,
			UnreadCount:  b.Count, // read-state isn't tracked yet; mirror the count
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].LastReceived.After(out[j].LastReceived) })
	if len(out) > cap_ {
		out = out[:cap_]
	}
	return out, nil
}

// threadKey returns the grouping key for a message: prefer the root of
// References, then In-Reply-To, then a normalized subject.
func threadKey(m rawMail) string {
	if len(m.References) > 0 {
		return "ref:" + m.References[0]
	}
	if m.InReplyTo != "" {
		return "irt:" + m.InReplyTo
	}
	return "subj:" + normalizeSubject(m.Subject)
}

var subjectPrefixRE = regexp.MustCompile(`(?i)^\s*(re|fwd?|aw|sv|ref)\s*:\s*`)

func normalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		stripped := subjectPrefixRE.ReplaceAllString(s, "")
		if stripped == s {
			break
		}
		s = stripped
	}
	return strings.ToLower(strings.TrimSpace(s))
}
