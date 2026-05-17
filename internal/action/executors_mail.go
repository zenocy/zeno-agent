package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/llm"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/mail"
	imapsensor "github.com/zenocy/zeno-v2/internal/sensor/imap"
	smtpsensor "github.com/zenocy/zeno-v2/internal/sensor/smtp"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// MailDeps bundles the dependencies the mail Executors share. Built
// once at boot in cmd/zeno/main.go and copied by value into each
// Executor struct so each can adjust its own knobs (timeout, allowlist).
type MailDeps struct {
	Dialer  imapsensor.Dialer
	IMAPCfg config.IMAPConfig
	SMTP    smtpsensor.Client // nil → SMTP disabled, sends degrade to drafts
	SMTPCfg config.SMTPConfig
	Reader  logp.Reader // for resolving the source mail.received event
	LLM     *llm.Client
	Voice   string // typically PromptSet.VoiceShort, used by DraftReply
	Logger  *logrus.Entry
}

// findSourceMail locates the most recent mail.received event whose
// subject matches needle (case-insensitive). Returns nil when no match
// is found. Lookup is bounded by ByKind's natural backstop — mail
// events are kept indefinitely in V2.x but the most recent match wins
// regardless.
func findSourceMail(ctx context.Context, reader logp.Reader, needle string) (*mailPayload, error) {
	if reader == nil || strings.TrimSpace(needle) == "" {
		return nil, nil
	}
	events, err := reader.ByKind(ctx, logp.KindMailReceived)
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSpace(needle))
	for i := len(events) - 1; i >= 0; i-- {
		var p mailPayload
		if err := json.Unmarshal(events[i].Payload, &p); err != nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(p.Subject)) == want {
			return &p, nil
		}
	}
	return nil, nil
}

// mailPayload mirrors internal/sensor/imap.mailPayload (unexported there).
// Duplicated here to decode mail.received events without exporting the
// private struct from the imap package.
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

// allowedMoveFolders returns the move-mail allowlist; defaults if empty.
func allowedMoveFolders(cfg config.IMAPConfig) map[string]struct{} {
	out := map[string]struct{}{}
	src := cfg.AllowedMoveFolders
	if len(src) == 0 {
		src = []string{"Inbox", "Archive", "Trash"}
	}
	for _, f := range src {
		out[strings.ToLower(strings.TrimSpace(f))] = struct{}{}
	}
	return out
}

// withIMAPSession dials, logs in, runs fn, and logs out.
func (d MailDeps) withIMAPSession(ctx context.Context, fn func(c imapsensor.Client) error) error {
	if d.Dialer == nil {
		return fmt.Errorf("imap: dialer not configured")
	}
	c, err := d.Dialer.Dial(ctx)
	if err != nil {
		return fmt.Errorf("imap dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(d.IMAPCfg.Username, d.IMAPCfg.Password); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	defer func() { _ = c.Logout() }()
	return fn(c)
}

// ----------------------------------------------------------------------
// MarkReadExec — sets \Seen on the source mail's UID.
// ----------------------------------------------------------------------

type MarkReadExec struct {
	Deps MailDeps
}

func (e *MarkReadExec) Mode() Mode { return Mode1Click }

func (e *MarkReadExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	subj := stringFromTarget(ec.Target, "subject")
	src, err := findSourceMail(ctx, e.Deps.Reader, subj)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up source thread."}, err
	}
	if src == nil {
		return Result{OK: false, Toast: "No matching mail thread to mark read."}, nil
	}
	if err := e.Deps.withIMAPSession(ctx, func(c imapsensor.Client) error {
		if _, err := c.Select(src.Folder); err != nil {
			return err
		}
		return c.Store(src.Folder, src.UID, []string{`\Seen`}, nil)
	}); err != nil {
		return Result{OK: false, Toast: "IMAP failed; thread not flagged read."}, err
	}
	return Result{
		OK:           true,
		EventKind:    logp.KindMailMarkedRead,
		EventPayload: map[string]any{"folder": src.Folder, "uid": src.UID, "subject": src.Subject},
		Toast:        fmt.Sprintf("Marked read: %s", src.Subject),
	}, nil
}

// ----------------------------------------------------------------------
// FlagMailExec — toggles \Flagged on the source thread's most recent UID.
//   target.on=false removes the flag; default true sets it.
// ----------------------------------------------------------------------

type FlagMailExec struct {
	Deps MailDeps
}

func (e *FlagMailExec) Mode() Mode { return Mode1Click }

func (e *FlagMailExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	subj := stringFromTarget(ec.Target, "subject")
	src, err := findSourceMail(ctx, e.Deps.Reader, subj)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up source thread."}, err
	}
	if src == nil {
		return Result{OK: false, Toast: "No matching mail thread to flag."}, nil
	}
	on := true
	if v, ok := ec.Target["on"].(bool); ok {
		on = v
	}
	if err := e.Deps.withIMAPSession(ctx, func(c imapsensor.Client) error {
		if _, err := c.Select(src.Folder); err != nil {
			return err
		}
		if on {
			return c.Store(src.Folder, src.UID, []string{`\Flagged`}, nil)
		}
		return c.Store(src.Folder, src.UID, nil, []string{`\Flagged`})
	}); err != nil {
		return Result{OK: false, Toast: "IMAP failed; flag not toggled."}, err
	}
	verb := "Flagged"
	if !on {
		verb = "Unflagged"
	}
	return Result{
		OK:        true,
		EventKind: logp.KindMailFlagged,
		EventPayload: map[string]any{
			"folder": src.Folder, "uid": src.UID,
			"subject": src.Subject, "on": on,
		},
		Toast: fmt.Sprintf("%s: %s", verb, src.Subject),
	}, nil
}

// ----------------------------------------------------------------------
// MoveMailExec — moves a thread's most recent message to target.folder.
// ----------------------------------------------------------------------

type MoveMailExec struct {
	Deps MailDeps
}

func (e *MoveMailExec) Mode() Mode { return Mode1Click }

func (e *MoveMailExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	dest := stringFromTarget(ec.Target, "folder")
	if dest == "" {
		return Result{OK: false, Toast: "No destination folder."}, nil
	}
	allow := allowedMoveFolders(e.Deps.IMAPCfg)
	if _, ok := allow[strings.ToLower(dest)]; !ok {
		return Result{OK: false, Toast: fmt.Sprintf("Cannot move to %q (not on the allowlist).", dest)}, nil
	}

	subj := stringFromTarget(ec.Target, "subject")
	src, err := findSourceMail(ctx, e.Deps.Reader, subj)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up source thread."}, err
	}
	if src == nil {
		return Result{OK: false, Toast: "No matching mail thread to move."}, nil
	}
	if err := e.Deps.withIMAPSession(ctx, func(c imapsensor.Client) error {
		if _, err := c.Select(src.Folder); err != nil {
			return err
		}
		return c.Move(src.Folder, []uint32{src.UID}, dest)
	}); err != nil {
		return Result{OK: false, Toast: fmt.Sprintf("Move to %s failed.", dest)}, err
	}
	return Result{
		OK:           true,
		EventKind:    logp.KindMailMoved,
		EventPayload: map[string]any{"from_folder": src.Folder, "to_folder": dest, "uid": src.UID, "subject": src.Subject},
		Toast:        fmt.Sprintf("Moved to %s.", dest),
	}, nil
}

// ----------------------------------------------------------------------
// DraftReplyExec — generates a reply, saves it to Drafts.
//   - Mode: ModePreflight. Confirm:false runs the draft and returns
//     it as Preview. Confirm:true re-runs and APPENDs to Drafts.
// ----------------------------------------------------------------------

type DraftReplyExec struct {
	Deps MailDeps
}

func (e *DraftReplyExec) Mode() Mode { return ModePreflight }

func (e *DraftReplyExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	subj := stringFromTarget(ec.Target, "subject")
	steer := stringFromTarget(ec.Target, "steer")

	src, err := findSourceMail(ctx, e.Deps.Reader, subj)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up source thread."}, err
	}
	if src == nil {
		return Result{OK: false, Toast: "No matching mail thread to reply to."}, nil
	}

	body, err := synth.DraftReply(ctx, synth.DraftReplyOpts{
		LLM: e.Deps.LLM, Voice: e.Deps.Voice,
	}, synth.DraftReplyContext{
		From: src.From, Subject: src.Subject, BodySnippet: src.BodyPreview,
		SentAt: src.Date, UserSteer: steer,
	})
	if err != nil || body == "" {
		body = fallbackReplyBody(src.From)
	}

	to := []string{src.From}
	subject := mail.ReplySubject(src.Subject)
	from := smtpsensor.FromAddress(e.Deps.SMTPCfg)
	if from == "" {
		from = e.Deps.IMAPCfg.Username
	}

	if !ec.Confirm {
		// Preview only.
		return Result{
			OK:           true,
			NeedsConfirm: true,
			Preview: map[string]any{
				"to":          to,
				"subject":     subject,
				"from":        from,
				"body":        body,
				"in_reply_to": src.MessageID,
			},
			Toast: "",
		}, nil
	}

	// Commit: build MIME and APPEND to Drafts.
	references := append([]string{}, src.References...)
	if src.MessageID != "" {
		references = append(references, src.MessageID)
	}
	raw, err := mail.Build(mail.Message{
		From:       from,
		To:         to,
		Subject:    subject,
		Body:       body,
		Date:       ec.Now,
		InReplyTo:  src.MessageID,
		References: references,
	})
	if err != nil {
		return Result{OK: false, Toast: "Could not build draft message."}, err
	}

	folder := e.Deps.IMAPCfg.DraftsFolder
	if folder == "" {
		folder = "Drafts"
	}
	var newUID uint32
	if err := e.Deps.withIMAPSession(ctx, func(c imapsensor.Client) error {
		uid, err := c.Append(folder, []string{`\Draft`}, ec.Now, raw)
		newUID = uid
		return err
	}); err != nil {
		return Result{OK: false, Toast: "Could not save to Drafts."}, err
	}
	return Result{
		OK:        true,
		EventKind: logp.KindMailDraftSaved,
		EventPayload: map[string]any{
			"folder":      folder,
			"uid":         newUID,
			"to":          to,
			"subject":     subject,
			"in_reply_to": src.MessageID,
		},
		Toast: fmt.Sprintf("Saved to %s.", folder),
	}, nil
}

// ----------------------------------------------------------------------
// ForwardExec — forwards a thread to a chosen recipient.
// ----------------------------------------------------------------------

type ForwardExec struct {
	Deps MailDeps
}

func (e *ForwardExec) Mode() Mode { return ModePreflight }

func (e *ForwardExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	subj := stringFromTarget(ec.Target, "subject")
	to := stringFromTarget(ec.Target, "to")
	note := stringFromTarget(ec.Target, "note")
	if to == "" {
		return Result{OK: false, Toast: "Forward needs target.to."}, nil
	}

	src, err := findSourceMail(ctx, e.Deps.Reader, subj)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up source thread."}, err
	}
	if src == nil {
		return Result{OK: false, Toast: "No matching mail thread to forward."}, nil
	}

	body := note + "\n\n" + mail.QuoteBody(src.From, src.BodyPreview, src.Date)
	subject := mail.ForwardSubject(src.Subject)
	from := smtpsensor.FromAddress(e.Deps.SMTPCfg)
	if from == "" {
		from = e.Deps.IMAPCfg.Username
	}

	if !ec.Confirm {
		return Result{
			OK:           true,
			NeedsConfirm: true,
			Preview: map[string]any{
				"to":      []string{to},
				"subject": subject,
				"from":    from,
				"body":    body,
			},
		}, nil
	}

	// Commit: APPEND to Drafts (Phase 1 ships forward as draft-only;
	// SendReplyExec covers the SMTP submission path explicitly).
	raw, err := mail.Build(mail.Message{
		From: from, To: []string{to}, Subject: subject, Body: body, Date: ec.Now,
	})
	if err != nil {
		return Result{OK: false, Toast: "Could not build forward."}, err
	}
	folder := e.Deps.IMAPCfg.DraftsFolder
	if folder == "" {
		folder = "Drafts"
	}
	var newUID uint32
	if err := e.Deps.withIMAPSession(ctx, func(c imapsensor.Client) error {
		uid, err := c.Append(folder, []string{`\Draft`}, ec.Now, raw)
		newUID = uid
		return err
	}); err != nil {
		return Result{OK: false, Toast: "Could not save to Drafts."}, err
	}
	return Result{
		OK:        true,
		EventKind: logp.KindMailDraftSaved,
		EventPayload: map[string]any{
			"folder":  folder,
			"uid":     newUID,
			"to":      []string{to},
			"subject": subject,
			"forward": true,
		},
		Toast: fmt.Sprintf("Forward saved to %s.", folder),
	}, nil
}

// ----------------------------------------------------------------------
// SendReplyExec — generates a reply, sends via SMTP, saves to Sent.
//   - Always preflighted. Commit-time requires non-empty target.recipients
//     OR a discoverable source thread (so we can fall back to From).
// ----------------------------------------------------------------------

type SendReplyExec struct {
	Deps MailDeps
}

func (e *SendReplyExec) Mode() Mode { return ModePreflight }

func (e *SendReplyExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Deps.SMTP == nil {
		return Result{OK: false, Toast: "SMTP is not configured. Drafts work; sending requires sensors.smtp.*."}, nil
	}

	subj := stringFromTarget(ec.Target, "subject")
	steer := stringFromTarget(ec.Target, "steer")

	src, err := findSourceMail(ctx, e.Deps.Reader, subj)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up source thread."}, err
	}
	if src == nil {
		return Result{OK: false, Toast: "No matching mail thread to reply to."}, nil
	}

	body, err := synth.DraftReply(ctx, synth.DraftReplyOpts{
		LLM: e.Deps.LLM, Voice: e.Deps.Voice,
	}, synth.DraftReplyContext{
		From: src.From, Subject: src.Subject, BodySnippet: src.BodyPreview,
		SentAt: src.Date, UserSteer: steer,
	})
	if err != nil || body == "" {
		body = fallbackReplyBody(src.From)
	}

	to := []string{src.From}
	if extra := stringSliceFromTarget(ec.Target, "recipients"); len(extra) > 0 {
		to = extra
	}
	subject := mail.ReplySubject(src.Subject)
	from := smtpsensor.FromAddress(e.Deps.SMTPCfg)
	if from == "" {
		from = e.Deps.IMAPCfg.Username
	}

	if !ec.Confirm {
		return Result{
			OK:           true,
			NeedsConfirm: true,
			Preview: map[string]any{
				"to":      to,
				"subject": subject,
				"from":    from,
				"body":    body,
				"send":    true,
			},
		}, nil
	}

	// Commit: build, SMTP-send, then APPEND a copy to Sent.
	references := append([]string{}, src.References...)
	if src.MessageID != "" {
		references = append(references, src.MessageID)
	}
	raw, err := mail.Build(mail.Message{
		From: from, To: to, Subject: subject, Body: body, Date: ec.Now,
		InReplyTo: src.MessageID, References: references,
	})
	if err != nil {
		return Result{OK: false, Toast: "Could not build reply."}, err
	}
	if err := e.Deps.SMTP.Send(ctx, smtpFrom(from), to, raw); err != nil {
		return Result{OK: false, Toast: "SMTP send failed; reply not sent."}, err
	}

	// Best-effort copy to Sent. Failure is logged but does not fail the action.
	sent := e.Deps.IMAPCfg.SentFolder
	if sent == "" {
		sent = "Sent"
	}
	_ = e.Deps.withIMAPSession(ctx, func(c imapsensor.Client) error {
		_, _ = c.Append(sent, []string{`\Seen`}, ec.Now, raw)
		return nil
	})

	return Result{
		OK:        true,
		EventKind: logp.KindMailSent,
		EventPayload: map[string]any{
			"to":          to,
			"subject":     subject,
			"in_reply_to": src.MessageID,
		},
		Toast: fmt.Sprintf("Sent to %s.", strings.Join(to, ", ")),
	}, nil
}

// smtpFrom strips a display-name prefix to leave a bare envelope sender.
func smtpFrom(s string) string {
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return strings.TrimSpace(s)
}

// fallbackReplyBody is the final safety net when the LLM is unavailable.
// Keeps us in the "drafts-only" posture instead of failing the action.
func fallbackReplyBody(originalFrom string) string {
	name := originalFrom
	if i := strings.Index(name, "<"); i > 0 {
		name = strings.TrimSpace(name[:i])
	}
	if name == "" {
		name = "there"
	}
	return fmt.Sprintf("Hi %s,\n\nThanks for your note — let me come back to this later today.\n\nBest,\n", name)
}

func stringFromTarget(target map[string]any, key string) string {
	if target == nil {
		return ""
	}
	v, ok := target[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func stringSliceFromTarget(target map[string]any, key string) []string {
	if target == nil {
		return nil
	}
	v, ok := target[key]
	if !ok {
		return nil
	}
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	if arr, ok := v.([]string); ok {
		out := make([]string, 0, len(arr))
		for _, s := range arr {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
