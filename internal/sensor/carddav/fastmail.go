package carddav

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"

	"github.com/zenocy/zeno-v2/internal/config"
)

// FastmailProvider implements Provider against Fastmail's CardDAV
// endpoint. URL is the server root (e.g. https://carddav.fastmail.com/);
// the principal/home-set discovery picks the default address book.
type FastmailProvider struct {
	cfg      config.CardDAVConfig
	client   *carddav.Client
	bookPath string
}

// NewFastmail builds a Provider. Discovers the address-book home set
// using the same principal-path trick the CalDAV provider uses (Fastmail
// rejects PROPFIND on collection roots, so we construct the principal
// path directly).
func NewFastmail(ctx context.Context, cfg config.CardDAVConfig) (*FastmailProvider, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("carddav: url is required")
	}
	if cfg.Username == "" {
		return nil, fmt.Errorf("carddav: username is required")
	}
	rawHTTP := &http.Client{Timeout: 30 * time.Second}
	httpC := webdav.HTTPClientWithBasicAuth(rawHTTP, cfg.Username, cfg.Password)
	c, err := carddav.NewClient(httpC, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("carddav client: %w", err)
	}

	principal := "/dav/principals/user/" + cfg.Username + "/"
	homeSet, err := c.FindAddressBookHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("carddav: find home set: %w", err)
	}

	books, err := c.FindAddressBooks(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("carddav: list address books: %w", err)
	}
	if len(books) == 0 {
		return nil, fmt.Errorf("carddav: no address books found under %s", homeSet)
	}

	// V1 picks the first book. Fastmail surfaces "Default" first; users
	// with multiple books can override by setting cfg.URL to the
	// specific book path (the discovery still works since FindAddressBooks
	// at a book URL returns that book).
	return &FastmailProvider{
		cfg:      cfg,
		client:   c,
		bookPath: books[0].Path,
	}, nil
}

// ListAll fetches every AddressObject in the discovered book.
//
// The query carries a permissive `PropFilter{Name: "FN"}` so the server
// returns every vCard where FN is defined — which is every vCard, since
// FN is mandatory in both vCard 3.0 and 4.0 (RFC 6350 §6.2.1). RFC 6352
// §10.5 leaves an empty `<filter/>` element undefined, and Fastmail (and
// some other strict CardDAV servers) interpret it as "match nothing" —
// silently returning an empty MultiStatus. The trivial "FN is defined"
// filter sidesteps that ambiguity while still asking for the entire book.
func (p *FastmailProvider) ListAll(ctx context.Context) ([]carddav.AddressObject, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("carddav: provider not initialized")
	}
	q := &carddav.AddressBookQuery{
		DataRequest: carddav.AddressDataRequest{AllProp: true},
		PropFilters: []carddav.PropFilter{{Name: "FN"}},
	}
	objs, err := p.client.QueryAddressBook(ctx, p.bookPath, q)
	if err != nil {
		return nil, fmt.Errorf("carddav: query address book: %w", err)
	}
	return objs, nil
}

// BookPath exposes the discovered address-book path. Useful for
// diagnostics + future incremental-sync work.
func (p *FastmailProvider) BookPath() string {
	if p == nil {
		return ""
	}
	return p.bookPath
}
