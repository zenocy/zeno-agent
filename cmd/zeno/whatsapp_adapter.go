package main

import (
	"context"
	"errors"

	"github.com/zenocy/zeno-v2/internal/action"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
)

// contactResolverAdapter bridges the concrete *whatsapp.Resolver to the
// action.WhatsAppResolver interface. The action package can't import
// whatsapp directly (cycle), so the action.Resolve* error types and
// WhatsAppContact struct are mirrored on its side. This adapter
// translates between them.
type contactResolverAdapter struct {
	r *whatsapp.Resolver
}

func (a contactResolverAdapter) Resolve(ctx context.Context, query string) (action.WhatsAppContact, error) {
	c, err := a.r.Resolve(ctx, query)
	if err != nil {
		var amb *whatsapp.ErrAmbiguous
		if errors.As(err, &amb) {
			return action.WhatsAppContact{}, &action.ResolveErrAmbiguous{
				Query:      amb.Query,
				Candidates: amb.Candidates,
			}
		}
		var nf *whatsapp.ErrContactNotFound
		if errors.As(err, &nf) {
			return action.WhatsAppContact{}, &action.ResolveErrNotFound{Query: nf.Query}
		}
		return action.WhatsAppContact{}, err
	}
	return action.WhatsAppContact{
		Name:       c.Name,
		JID:        c.JID,
		IsGroup:    c.IsGroup,
		FactID:     c.FactID,
		CardDAVUID: c.CardDAVUID,
	}, nil
}
