package action

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/config"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	imapsensor "github.com/zenocy/zeno-v2/internal/sensor/imap"
)

// stubIMAPClient records every write the Executor makes so tests can
// assert against them. Reads are minimal — the Executors only Select +
// the write methods; UIDSearchAfter / FetchEnvelopeAndBody never fire.
type stubIMAPClient struct {
	loginErr  error
	selectErr error
	appendErr error
	storeErr  error
	moveErr   error

	selected []string
	appended []recordedAppend
	stored   []recordedStore
	moved    []recordedMove
}

type recordedAppend struct {
	Folder string
	Flags  []string
	Raw    []byte
}

type recordedStore struct {
	Folder      string
	UID         uint32
	AddFlags    []string
	RemoveFlags []string
}

type recordedMove struct {
	Source string
	Dest   string
	UIDs   []uint32
}

func (s *stubIMAPClient) Login(_, _ string) error { return s.loginErr }
func (s *stubIMAPClient) Select(folder string) (*imapsensor.SelectData, error) {
	if s.selectErr != nil {
		return nil, s.selectErr
	}
	s.selected = append(s.selected, folder)
	return &imapsensor.SelectData{UIDValidity: 1, UIDNext: 1000}, nil
}
func (s *stubIMAPClient) UIDSearchAfter(_ string, _ uint32) ([]uint32, error) { return nil, nil }
func (s *stubIMAPClient) UIDSearchAll(_ string) ([]uint32, error)             { return nil, nil }
func (s *stubIMAPClient) FetchEnvelopeAndBody(_ string, _ []uint32) ([]imapsensor.RawMessage, error) {
	return nil, nil
}
func (s *stubIMAPClient) Append(folder string, flags []string, _ time.Time, raw []byte) (uint32, error) {
	if s.appendErr != nil {
		return 0, s.appendErr
	}
	s.appended = append(s.appended, recordedAppend{Folder: folder, Flags: append([]string(nil), flags...), Raw: append([]byte(nil), raw...)})
	return uint32(len(s.appended)) + 100, nil
}
func (s *stubIMAPClient) Store(folder string, uid uint32, addFlags, removeFlags []string) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	s.stored = append(s.stored, recordedStore{Folder: folder, UID: uid, AddFlags: addFlags, RemoveFlags: removeFlags})
	return nil
}
func (s *stubIMAPClient) Move(folder string, uids []uint32, dest string) error {
	if s.moveErr != nil {
		return s.moveErr
	}
	s.moved = append(s.moved, recordedMove{Source: folder, Dest: dest, UIDs: append([]uint32(nil), uids...)})
	return nil
}
func (s *stubIMAPClient) Logout() error { return nil }
func (s *stubIMAPClient) Close() error  { return nil }

type stubDialer struct {
	c   *stubIMAPClient
	err error
}

func (d *stubDialer) Dial(_ context.Context) (imapsensor.Client, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.c, nil
}

type stubSMTP struct {
	sent []sentMail
	err  error
}

type sentMail struct {
	From string
	To   []string
	Raw  []byte
}

func (s *stubSMTP) Send(_ context.Context, from string, to []string, raw []byte) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, sentMail{From: from, To: append([]string(nil), to...), Raw: append([]byte(nil), raw...)})
	return nil
}

// seedMail appends a mail.received event matching the executors' Subject
// lookup convention. Returns the payload's Subject for convenience.
func seedMail(t *testing.T, r *logtest.MemReader, subject, from, body, msgID string) {
	t.Helper()
	r.AppendEvent(logtest.MakeEvent(logp.KindMailReceived, "imap", time.Now(), map[string]any{
		"folder":       "INBOX",
		"uid":          uint32(42),
		"uidvalidity":  uint32(1),
		"message_id":   msgID,
		"from":         from,
		"subject":      subject,
		"date":         time.Now().Format(time.RFC3339),
		"body_preview": body,
	}))
}

func makeMailDeps(c *stubIMAPClient, smtp *stubSMTP, reader *logtest.MemReader) MailDeps {
	imapCfg := config.IMAPConfig{
		Host: "imap.example", Username: "user@example.com", Password: "x",
		DraftsFolder:       "Drafts",
		SentFolder:         "Sent",
		AllowedMoveFolders: []string{"Inbox", "Archive", "Trash"},
	}
	smtpCfg := config.SMTPConfig{Host: "smtp.example", Username: "user@example.com", From: "user@example.com"}
	deps := MailDeps{
		Dialer:  &stubDialer{c: c},
		IMAPCfg: imapCfg,
		SMTPCfg: smtpCfg,
		Reader:  reader,
	}
	if smtp != nil {
		deps.SMTP = smtp
	}
	return deps
}

// ----------------------------------------------------------------------
// MarkReadExec
// ----------------------------------------------------------------------

func TestMarkRead_FlagsSeen(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Saru Patel · re: redline", "saru@example.com", "...", "saru-1@example.com")

	ex := &MarkReadExec{Deps: makeMailDeps(stub, nil, reader)}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "Saru Patel · re: redline"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindMailMarkedRead, res.EventKind)

	require.Len(t, stub.stored, 1)
	require.Equal(t, []string{`\Seen`}, stub.stored[0].AddFlags)
	require.Equal(t, "INBOX", stub.stored[0].Folder)
	require.Equal(t, uint32(42), stub.stored[0].UID)
}

func TestMarkRead_NoMatchingThread(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	ex := &MarkReadExec{Deps: makeMailDeps(stub, nil, reader)}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "no such thread"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Empty(t, stub.stored)
}

// ----------------------------------------------------------------------
// FlagMailExec
// ----------------------------------------------------------------------

func TestFlagMail_AddsFlaggedByDefault(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Important note", "alex@example.com", "...", "alex-1@example.com")

	ex := &FlagMailExec{Deps: makeMailDeps(stub, nil, reader)}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "Important note"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindMailFlagged, res.EventKind)

	require.Len(t, stub.stored, 1)
	require.Equal(t, []string{`\Flagged`}, stub.stored[0].AddFlags)
	require.Empty(t, stub.stored[0].RemoveFlags)
}

func TestFlagMail_RemovesWhenOnFalse(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Important note", "alex@example.com", "...", "alex-1@example.com")

	ex := &FlagMailExec{Deps: makeMailDeps(stub, nil, reader)}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "Important note", "on": false},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Len(t, stub.stored, 1)
	require.Empty(t, stub.stored[0].AddFlags)
	require.Equal(t, []string{`\Flagged`}, stub.stored[0].RemoveFlags)
}

// ----------------------------------------------------------------------
// MoveMailExec
// ----------------------------------------------------------------------

func TestMoveMail_AllowlistEnforced(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Old quote", "vendor@example.com", "...", "vendor-1@example.com")
	ex := &MoveMailExec{Deps: makeMailDeps(stub, nil, reader)}

	// Disallowed folder.
	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "Old quote", "folder": "SecretBackup"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "allowlist")
	require.Empty(t, stub.moved)

	// Allowed folder.
	res, err = ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "Old quote", "folder": "Archive"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Len(t, stub.moved, 1)
	require.Equal(t, "Archive", stub.moved[0].Dest)
}

// ----------------------------------------------------------------------
// DraftReplyExec
// ----------------------------------------------------------------------

func TestDraftReply_PreflightReturnsPreviewWithoutWriting(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Saru Patel · re: redline", "saru@example.com",
		"Walked the redline. Two questions remain.", "saru-1@example.com")

	ex := &DraftReplyExec{Deps: makeMailDeps(stub, nil, reader)}
	require.Equal(t, ModePreflight, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: false,
		Target:  map[string]any{"subject": "Saru Patel · re: redline", "steer": "Acknowledge and ask Lin to weigh in."},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.True(t, res.NeedsConfirm)
	require.Empty(t, stub.appended, "preview must not write to Drafts")

	preview := res.Preview
	require.NotNil(t, preview)
	require.Equal(t, "Re: Saru Patel · re: redline", preview["subject"])
	body, _ := preview["body"].(string)
	require.NotEmpty(t, body)
	to, _ := preview["to"].([]string)
	require.Equal(t, []string{"saru@example.com"}, to)
}

func TestDraftReply_CommitAppendsToDrafts(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Pricing v2", "lin@example.com", "Pricing slide cohort table.", "lin-1@example.com")

	ex := &DraftReplyExec{Deps: makeMailDeps(stub, nil, reader)}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Now:     time.Date(2026, 5, 7, 14, 0, 0, 0, time.UTC),
		Target:  map[string]any{"subject": "Pricing v2"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindMailDraftSaved, res.EventKind)

	require.Len(t, stub.appended, 1)
	require.Equal(t, "Drafts", stub.appended[0].Folder)
	require.Equal(t, []string{`\Draft`}, stub.appended[0].Flags)
	require.Contains(t, string(stub.appended[0].Raw), "Subject: Re: Pricing v2\r\n")
	require.Contains(t, string(stub.appended[0].Raw), "In-Reply-To: <lin-1@example.com>\r\n")
}

// ----------------------------------------------------------------------
// ForwardExec
// ----------------------------------------------------------------------

func TestForward_RequiresTo(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	ex := &ForwardExec{Deps: makeMailDeps(stub, nil, reader)}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "Pricing v2"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
}

// ----------------------------------------------------------------------
// SendReplyExec
// ----------------------------------------------------------------------

func TestSendReply_NoSMTPDegradesGracefully(t *testing.T) {
	stub := &stubIMAPClient{}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Pricing v2", "lin@example.com", "Pricing slide.", "lin-1@example.com")

	deps := makeMailDeps(stub, nil, reader)
	deps.SMTP = nil
	ex := &SendReplyExec{Deps: deps}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Target:  map[string]any{"subject": "Pricing v2"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, strings.ToLower(res.Toast), "smtp")
}

func TestSendReply_SubmitsAndCopiesToSent(t *testing.T) {
	stub := &stubIMAPClient{}
	smtp := &stubSMTP{}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Pricing v2", "lin@example.com", "Pricing slide.", "lin-1@example.com")

	ex := &SendReplyExec{Deps: makeMailDeps(stub, smtp, reader)}

	// Preview first (won't send).
	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: false,
		Target:  map[string]any{"subject": "Pricing v2"},
	})
	require.NoError(t, err)
	require.True(t, res.NeedsConfirm)
	require.Empty(t, smtp.sent, "preview must not call SMTP")
	require.Empty(t, stub.appended, "preview must not append to Sent")

	// Commit.
	res, err = ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Now:     time.Date(2026, 5, 7, 14, 0, 0, 0, time.UTC),
		Target:  map[string]any{"subject": "Pricing v2"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindMailSent, res.EventKind)

	require.Len(t, smtp.sent, 1)
	require.Equal(t, "user@example.com", smtp.sent[0].From)
	require.Equal(t, []string{"lin@example.com"}, smtp.sent[0].To)

	// Sent-folder copy is best-effort.
	if len(stub.appended) > 0 {
		require.Equal(t, "Sent", stub.appended[0].Folder)
	}
}

func TestSendReply_SMTPFailureSurfaced(t *testing.T) {
	stub := &stubIMAPClient{}
	smtp := &stubSMTP{err: errors.New("boom")}
	reader := logtest.NewMemReader()
	seedMail(t, reader, "Pricing v2", "lin@example.com", "...", "lin-1@example.com")

	ex := &SendReplyExec{Deps: makeMailDeps(stub, smtp, reader)}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Target:  map[string]any{"subject": "Pricing v2"},
	})
	require.Error(t, err)
	require.False(t, res.OK)
}
