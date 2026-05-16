// Package imap is the IMAP sensor: connects to the configured account, polls
// each folder for new mail, and emits mail.received and imap.cursor events.
//
// The package isolates network I/O behind a Dialer/Client interface so that
// poller logic (cursor reasoning, dedupe, UIDVALIDITY handling) can be
// tested with a stub.
//
// V2.8.0 added the write methods (Append/Store/Move) used by the action
// surface in internal/action. The poller path remains read-only; only the
// action Executors call the new methods. Tests stub them in the same
// stubClient that already covers the read path.
package imap

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/zenocy/zeno-v2/internal/config"
)

// SelectData holds the fields we care about from an IMAP SELECT response.
type SelectData struct {
	UIDValidity uint32
	UIDNext     uint32
}

// RawMessage is one fetched message: envelope plus raw body bytes.
type RawMessage struct {
	UID    uint32
	Folder string
	Env    *imap.Envelope
	Body   []byte
}

// Client is the subset of go-imap's behaviour Zeno relies on. The
// poller uses the read methods; the V2.8 action surface uses the
// write methods. Stubbed in tests by stubClient.
type Client interface {
	Login(user, pass string) error
	Select(folder string) (*SelectData, error)
	UIDSearchAfter(folder string, lastUID uint32) ([]uint32, error)

	// UIDSearchAll returns every UID currently present in folder. The
	// folder must already be SELECTed. The poller uses this to emit an
	// imap.inbox_snapshot so projections can filter out mail that
	// vanished externally (user moved/deleted in their own client).
	UIDSearchAll(folder string) ([]uint32, error)

	FetchEnvelopeAndBody(folder string, uids []uint32) ([]RawMessage, error)

	// Append uploads a raw RFC 5322 message to folder with the given
	// IMAP flags (e.g. []string{`\Draft`} for the Drafts folder).
	// `when` is the INTERNALDATE the server records; pass time.Time{}
	// to let the server stamp it. Returns the new UID per the
	// APPENDUID resp-code; 0 when the server did not return one.
	Append(folder string, flags []string, when time.Time, raw []byte) (uid uint32, err error)

	// Store mutates IMAP flags on a single UID in folder (the folder
	// must currently be SELECTed). addFlags adds, removeFlags removes;
	// either may be empty.
	Store(folder string, uid uint32, addFlags, removeFlags []string) error

	// Move relocates the given UIDs from folder to dest. On servers
	// without the MOVE extension implementations may emulate via
	// COPY+STORE+EXPUNGE, but go-imap/v2's Move() handles this.
	Move(folder string, uids []uint32, dest string) error

	Logout() error
	Close() error
}

// Dialer constructs a Client. Production uses RealDialer (a TLS dial via
// imapclient); tests inject a stub.
type Dialer interface {
	Dial(ctx context.Context) (Client, error)
}

// RealDialer is the production go-imap Dialer.
type RealDialer struct {
	Cfg config.IMAPConfig
}

// NewRealDialer returns a Dialer wired against the provided config.
func NewRealDialer(cfg config.IMAPConfig) *RealDialer { return &RealDialer{Cfg: cfg} }

// Dial connects, applies STARTTLS if configured, and returns a Client.
func (d *RealDialer) Dial(_ context.Context) (Client, error) {
	addr := fmt.Sprintf("%s:%d", d.Cfg.Host, d.Cfg.Port)
	opts := &imapclient.Options{
		TLSConfig: &tls.Config{ServerName: d.Cfg.Host},
	}
	mode := strings.ToLower(strings.TrimSpace(d.Cfg.TLS))
	var (
		c   *imapclient.Client
		err error
	)
	switch mode {
	case "", "implicit":
		c, err = imapclient.DialTLS(addr, opts)
	case "starttls":
		c, err = imapclient.DialStartTLS(addr, opts)
	case "insecure":
		c, err = imapclient.DialInsecure(addr, nil)
	default:
		return nil, fmt.Errorf("unsupported tls mode %q", d.Cfg.TLS)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return &realClient{c: c}, nil
}

// realClient wraps imapclient.Client to satisfy our Client interface.
type realClient struct {
	c *imapclient.Client
}

func (r *realClient) Login(user, pass string) error {
	return r.c.Login(user, pass).Wait()
}

func (r *realClient) Select(folder string) (*SelectData, error) {
	d, err := r.c.Select(folder, nil).Wait()
	if err != nil {
		return nil, err
	}
	return &SelectData{UIDValidity: d.UIDValidity, UIDNext: uint32(d.UIDNext)}, nil
}

func (r *realClient) UIDSearchAfter(_ string, lastUID uint32) ([]uint32, error) {
	criteria := &imap.SearchCriteria{}
	if lastUID > 0 {
		criteria.UID = []imap.UIDSet{{imap.UIDRange{Start: imap.UID(lastUID + 1), Stop: 0}}}
	}
	data, err := r.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, err
	}
	uids := data.AllUIDs()
	out := make([]uint32, 0, len(uids))
	for _, u := range uids {
		out = append(out, uint32(u))
	}
	return out, nil
}

func (r *realClient) UIDSearchAll(_ string) ([]uint32, error) {
	data, err := r.c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return nil, err
	}
	uids := data.AllUIDs()
	out := make([]uint32, 0, len(uids))
	for _, u := range uids {
		out = append(out, uint32(u))
	}
	return out, nil
}

func (r *realClient) FetchEnvelopeAndBody(folder string, uids []uint32) ([]RawMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	imapUIDs := make([]imap.UID, len(uids))
	for i, u := range uids {
		imapUIDs[i] = imap.UID(u)
	}
	bodySection := &imap.FetchItemBodySection{Peek: true}
	opts := &imap.FetchOptions{
		UID:         true,
		Envelope:    true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}
	msgs, err := r.c.Fetch(imap.UIDSetNum(imapUIDs...), opts).Collect()
	if err != nil {
		return nil, err
	}
	out := make([]RawMessage, 0, len(msgs))
	for _, m := range msgs {
		body := m.FindBodySection(bodySection)
		out = append(out, RawMessage{
			UID:    uint32(m.UID),
			Folder: folder,
			Env:    m.Envelope,
			Body:   body,
		})
	}
	return out, nil
}

func (r *realClient) Append(folder string, flags []string, when time.Time, raw []byte) (uint32, error) {
	opts := &imap.AppendOptions{}
	if !when.IsZero() {
		opts.Time = when
	}
	if len(flags) > 0 {
		opts.Flags = make([]imap.Flag, 0, len(flags))
		for _, f := range flags {
			opts.Flags = append(opts.Flags, imap.Flag(f))
		}
	}
	cmd := r.c.Append(folder, int64(len(raw)), opts)
	if _, err := cmd.Write(raw); err != nil {
		return 0, fmt.Errorf("append write: %w", err)
	}
	if err := cmd.Close(); err != nil {
		return 0, fmt.Errorf("append close: %w", err)
	}
	data, err := cmd.Wait()
	if err != nil {
		return 0, err
	}
	if data == nil {
		return 0, nil
	}
	return uint32(data.UID), nil
}

func (r *realClient) Store(_ string, uid uint32, addFlags, removeFlags []string) error {
	uidSet := imap.UIDSetNum(imap.UID(uid))
	if len(addFlags) > 0 {
		flags := toFlags(addFlags)
		if err := r.c.Store(uidSet, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: flags, Silent: true}, nil).Close(); err != nil {
			return fmt.Errorf("store add: %w", err)
		}
	}
	if len(removeFlags) > 0 {
		flags := toFlags(removeFlags)
		if err := r.c.Store(uidSet, &imap.StoreFlags{Op: imap.StoreFlagsDel, Flags: flags, Silent: true}, nil).Close(); err != nil {
			return fmt.Errorf("store del: %w", err)
		}
	}
	return nil
}

func (r *realClient) Move(_ string, uids []uint32, dest string) error {
	if len(uids) == 0 {
		return nil
	}
	imapUIDs := make([]imap.UID, len(uids))
	for i, u := range uids {
		imapUIDs[i] = imap.UID(u)
	}
	cmd := r.c.Move(imap.UIDSetNum(imapUIDs...), dest)
	if cmd == nil {
		return fmt.Errorf("move: command returned nil")
	}
	_, err := cmd.Wait()
	return err
}

func toFlags(in []string) []imap.Flag {
	out := make([]imap.Flag, len(in))
	for i, f := range in {
		out[i] = imap.Flag(f)
	}
	return out
}

func (r *realClient) Logout() error {
	cmd := r.c.Logout()
	if cmd == nil {
		return nil
	}
	return cmd.Wait()
}

func (r *realClient) Close() error { return r.c.Close() }
