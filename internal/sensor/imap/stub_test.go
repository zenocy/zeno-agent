package imap

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	gimap "github.com/emersion/go-imap/v2"
)

// folderState is the in-memory representation of one IMAP folder for tests.
type folderState struct {
	UIDValidity uint32
	Messages    []RawMessage // ordered by UID asc
}

// stubServer is the source of truth for stubClients; tests arrange it before
// running Sync.
type stubServer struct {
	mu sync.Mutex

	folders map[string]*folderState

	// Failures arranged by tests.
	loginErr     error
	selectErr    error
	searchErr    error
	fetchErr     error
	fetchAfterN  int // if >0, fetchErr fires after N successful fetch calls
	fetchCallNum int

	// V2.8 write-path failures + recordings.
	appendErr error
	storeErr  error
	moveErr   error
	appended  []recordedAppend
	stored    []recordedStore
	moved     []recordedMove
}

func newStubServer() *stubServer {
	return &stubServer{folders: make(map[string]*folderState)}
}

func (s *stubServer) addFolder(name string, uidValidity uint32) *folderState {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := &folderState{UIDValidity: uidValidity}
	s.folders[name] = f
	return f
}

// putMessage appends a message with auto-incrementing UID.
func (s *stubServer) putMessage(folder string, msg RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.folders[folder]
	msg.Folder = folder
	f.Messages = append(f.Messages, msg)
	sort.Slice(f.Messages, func(i, j int) bool { return f.Messages[i].UID < f.Messages[j].UID })
}

// removeMessage drops a message by UID, simulating an external archive
// or delete. Used to verify the inbox snapshot reflects current state.
func (s *stubServer) removeMessage(folder string, uid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.folders[folder]
	if f == nil {
		return
	}
	kept := f.Messages[:0]
	for _, m := range f.Messages {
		if m.UID == uid {
			continue
		}
		kept = append(kept, m)
	}
	f.Messages = kept
}

func (s *stubServer) bumpUIDValidity(folder string, newValidity uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.folders[folder]; ok {
		f.UIDValidity = newValidity
	}
}

// stubDialer hands out stubClients backed by stubServer.
type stubDialer struct{ srv *stubServer }

func (d *stubDialer) Dial(_ context.Context) (Client, error) {
	return &stubClient{srv: d.srv}, nil
}

type stubClient struct {
	srv *stubServer
}

func (c *stubClient) Login(_, _ string) error { return c.srv.loginErr }

func (c *stubClient) Select(folder string) (*SelectData, error) {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if c.srv.selectErr != nil {
		return nil, c.srv.selectErr
	}
	f, ok := c.srv.folders[folder]
	if !ok {
		return nil, errors.New("folder not found: " + folder)
	}
	maxUID := uint32(0)
	for _, m := range f.Messages {
		if m.UID > maxUID {
			maxUID = m.UID
		}
	}
	return &SelectData{UIDValidity: f.UIDValidity, UIDNext: maxUID + 1}, nil
}

func (c *stubClient) UIDSearchAfter(folder string, lastUID uint32) ([]uint32, error) {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if c.srv.searchErr != nil {
		return nil, c.srv.searchErr
	}
	f := c.srv.folders[folder]
	out := make([]uint32, 0, len(f.Messages))
	for _, m := range f.Messages {
		if m.UID > lastUID {
			out = append(out, m.UID)
		}
	}
	return out, nil
}

func (c *stubClient) UIDSearchAll(folder string) ([]uint32, error) {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if c.srv.searchErr != nil {
		return nil, c.srv.searchErr
	}
	f := c.srv.folders[folder]
	if f == nil {
		return nil, nil
	}
	out := make([]uint32, 0, len(f.Messages))
	for _, m := range f.Messages {
		out = append(out, m.UID)
	}
	return out, nil
}

func (c *stubClient) FetchEnvelopeAndBody(folder string, uids []uint32) ([]RawMessage, error) {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	c.srv.fetchCallNum++
	if c.srv.fetchErr != nil && (c.srv.fetchAfterN == 0 || c.srv.fetchCallNum > c.srv.fetchAfterN) {
		return nil, c.srv.fetchErr
	}
	f := c.srv.folders[folder]
	want := make(map[uint32]struct{}, len(uids))
	for _, u := range uids {
		want[u] = struct{}{}
	}
	var out []RawMessage
	for _, m := range f.Messages {
		if _, ok := want[m.UID]; ok {
			cp := m
			cp.Folder = folder
			out = append(out, cp)
		}
	}
	return out, nil
}

func (c *stubClient) Logout() error { return nil }
func (c *stubClient) Close() error  { return nil }

// V2.8 write methods. Append records the message in the named folder
// at the next UID; Store and Move tweak the in-memory state.
//
// Tests can read appendedMessages / storedFlags / movedUIDs off
// stubServer to assert against the recorded operations.

type recordedAppend struct {
	Folder string
	Flags  []string
	When   time.Time
	Raw    []byte
	UID    uint32
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

func (c *stubClient) Append(folder string, flags []string, when time.Time, raw []byte) (uint32, error) {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if c.srv.appendErr != nil {
		return 0, c.srv.appendErr
	}
	f, ok := c.srv.folders[folder]
	if !ok {
		f = &folderState{UIDValidity: 1}
		c.srv.folders[folder] = f
	}
	maxUID := uint32(0)
	for _, m := range f.Messages {
		if m.UID > maxUID {
			maxUID = m.UID
		}
	}
	uid := maxUID + 1
	f.Messages = append(f.Messages, RawMessage{UID: uid, Folder: folder, Body: raw})
	c.srv.appended = append(c.srv.appended, recordedAppend{
		Folder: folder, Flags: append([]string(nil), flags...), When: when,
		Raw: append([]byte(nil), raw...), UID: uid,
	})
	return uid, nil
}

func (c *stubClient) Store(folder string, uid uint32, addFlags, removeFlags []string) error {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if c.srv.storeErr != nil {
		return c.srv.storeErr
	}
	c.srv.stored = append(c.srv.stored, recordedStore{
		Folder: folder, UID: uid,
		AddFlags:    append([]string(nil), addFlags...),
		RemoveFlags: append([]string(nil), removeFlags...),
	})
	return nil
}

func (c *stubClient) Move(folder string, uids []uint32, dest string) error {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if c.srv.moveErr != nil {
		return c.srv.moveErr
	}
	src, ok := c.srv.folders[folder]
	if ok {
		want := make(map[uint32]struct{}, len(uids))
		for _, u := range uids {
			want[u] = struct{}{}
		}
		kept := src.Messages[:0]
		for _, m := range src.Messages {
			if _, drop := want[m.UID]; drop {
				dst, dok := c.srv.folders[dest]
				if !dok {
					dst = &folderState{UIDValidity: 1}
					c.srv.folders[dest] = dst
				}
				m.Folder = dest
				dst.Messages = append(dst.Messages, m)
				continue
			}
			kept = append(kept, m)
		}
		src.Messages = kept
	}
	c.srv.moved = append(c.srv.moved, recordedMove{
		Source: folder, Dest: dest,
		UIDs: append([]uint32(nil), uids...),
	})
	return nil
}

// fixtureMessage builds a RawMessage with an envelope and raw body bytes.
func fixtureMessage(uid uint32, subject, from, to string, body []byte) RawMessage {
	env := &gimap.Envelope{
		Subject:   subject,
		From:      []gimap.Address{addrOf(from)},
		To:        []gimap.Address{addrOf(to)},
		MessageID: subject + "@example.test",
	}
	return RawMessage{UID: uid, Env: env, Body: body}
}

func addrOf(s string) gimap.Address {
	at := -1
	for i, c := range s {
		if c == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return gimap.Address{Mailbox: s}
	}
	return gimap.Address{Mailbox: s[:at], Host: s[at+1:]}
}
