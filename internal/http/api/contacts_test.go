package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func buildContactsHandler(t *testing.T) (*echo.Echo, *store.MemoryRepo, *store.ContactLinkRepo, *store.CardDAVRepo) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	mem := &store.MemoryRepo{DB: db}
	require.NoError(t, mem.Migrate())
	link := &store.ContactLinkRepo{DB: db}
	require.NoError(t, link.Migrate())
	cd := &store.CardDAVRepo{DB: db}
	require.NoError(t, cd.Migrate())

	e := echo.New()
	(&ContactsHandler{
		Memory:  mem,
		Link:    link,
		CardDAV: cd,
		Now:     func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}).Register(e)
	return e, mem, link, cd
}

func doContactsRequest(t *testing.T, e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, req)
	return rr
}

func TestContacts_GET_EmptyShape(t *testing.T) {
	e, _, _, _ := buildContactsHandler(t)
	rr := doContactsRequest(t, e, http.MethodGet, "/api/contacts", "")
	require.Equal(t, http.StatusOK, rr.Code)
	var resp contactsListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.CardDAVEnabled, "CardDAV repo wired → enabled flag true")
	require.Empty(t, resp.Contacts)
	require.Empty(t, resp.Groups)
}

func TestContacts_GET_DirectorySurfacesAllImportedContacts(t *testing.T) {
	// Even with NO aliases attached, every imported vCard appears in the
	// directory so the user sees the full address book.
	e, _, _, cd := buildContactsHandler(t)
	now := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	require.NoError(t, cd.Upsert(context.Background(), store.CardDAVContact{
		UID: "vc-sam", DisplayName: "Sam Carter", GivenName: "Sam", FamilyName: "Carter",
		Phones: store.EncodePhones([]store.Phone{
			{Value: "+44 7700 900111", Types: []string{"CELL"}, Pref: 1},
			{Value: "+44 20 1234 5678", Types: []string{"WORK"}},
		}),
		Emails:     store.EncodeEmails([]store.Email{{Value: "partner@example.com"}}),
		Nicknames:  store.EncodeStringList([]string{"Sammy"}),
		LastSyncAt: now,
	}))
	require.NoError(t, cd.Upsert(context.Background(), store.CardDAVContact{
		UID: "vc-bob", DisplayName: "Bob Smith",
		Phones:     store.EncodePhones([]store.Phone{{Value: "+1"}}),
		LastSyncAt: now.Add(-time.Hour),
	}))

	rr := doContactsRequest(t, e, http.MethodGet, "/api/contacts", "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp contactsListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Contacts, 2, "both contacts returned even without aliases")
	require.Equal(t, now, resp.LastSyncAt, "freshest LastSyncAt across rows")

	// Sort by display name for stable assertions.
	var sam *directoryContactDTO
	for i := range resp.Contacts {
		if resp.Contacts[i].UID == "vc-sam" {
			sam = &resp.Contacts[i]
		}
	}
	require.NotNil(t, sam)
	require.Equal(t, "Sam Carter", sam.DisplayName)
	require.Len(t, sam.Phones, 2, "all phones returned")
	require.Len(t, sam.Emails, 1)
	require.Equal(t, []string{"Sammy"}, sam.Nicknames)
	require.Empty(t, sam.Aliases, "no aliases yet")
}

func TestContacts_GET_AliasesNestedUnderCardDAV(t *testing.T) {
	e, _, _, cd := buildContactsHandler(t)
	require.NoError(t, cd.Upsert(context.Background(), store.CardDAVContact{
		UID: "vc-sam", DisplayName: "Sam Carter",
		Phones: store.EncodePhones([]store.Phone{{Value: "+1", Pref: 1}}),
	}))

	// Create two aliases for the same vCard.
	for _, body := range []string{
		`{"subject":"wife","fact":"Partner.","carddav_uid":"vc-sam"}`,
		`{"subject":"partner","fact":"Same Sam.","carddav_uid":"vc-sam","preferred_phone":"+1"}`,
	} {
		rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts", body)
		require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	}

	rr := doContactsRequest(t, e, http.MethodGet, "/api/contacts", "")
	require.Equal(t, http.StatusOK, rr.Code)
	var resp contactsListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Contacts, 1)
	require.Len(t, resp.Contacts[0].Aliases, 2)

	subjects := []string{resp.Contacts[0].Aliases[0].Subject, resp.Contacts[0].Aliases[1].Subject}
	require.Contains(t, subjects, "wife")
	require.Contains(t, subjects, "partner")
}

func TestContacts_GET_GroupsSeparateFromContacts(t *testing.T) {
	e, _, _, cd := buildContactsHandler(t)
	require.NoError(t, cd.Upsert(context.Background(), store.CardDAVContact{
		UID: "vc-sam", DisplayName: "Sam",
		Phones: store.EncodePhones([]store.Phone{{Value: "+1"}}),
	}))
	rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts",
		`{"subject":"family group","fact":"Living-room","jid":"120001@g.us"}`)
	require.Equal(t, http.StatusCreated, rr.Code)

	rr = doContactsRequest(t, e, http.MethodGet, "/api/contacts", "")
	require.Equal(t, http.StatusOK, rr.Code)
	var resp contactsListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Contacts, 1, "group does NOT appear in contacts directory")
	require.Empty(t, resp.Contacts[0].Aliases)
	require.Len(t, resp.Groups, 1)
	require.Equal(t, "family group", resp.Groups[0].Subject)
	require.Equal(t, "120001@g.us", resp.Groups[0].JID)
}

func TestContacts_POST_CreateGroup(t *testing.T) {
	e, _, link, _ := buildContactsHandler(t)
	rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts", `{
		"subject": "family group",
		"fact": "Living-room family chat.",
		"jid": "120001@g.us"
	}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var dto groupDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &dto))
	require.Equal(t, "120001@g.us", dto.JID)
	require.NotEmpty(t, dto.ID)

	row, err := link.GetByID(context.Background(), dto.ID)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.True(t, row.IsGroup)
}

func TestContacts_POST_CreatePerson_RequiresExistingCardDAV(t *testing.T) {
	e, _, _, _ := buildContactsHandler(t)
	rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts", `{
		"subject": "sam",
		"fact": "wife.",
		"carddav_uid": "vc-missing"
	}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestContacts_POST_CreatePerson_LinkedToCardDAV(t *testing.T) {
	e, _, _, cd := buildContactsHandler(t)
	require.NoError(t, cd.Upsert(context.Background(), store.CardDAVContact{
		UID: "vc-sam", DisplayName: "Sam Carter",
		Phones: store.EncodePhones([]store.Phone{{Value: "+1", Pref: 1}}),
	}))
	rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts", `{
		"subject": "sam",
		"fact": "wife.",
		"carddav_uid": "vc-sam"
	}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var dto aliasDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &dto))
	require.NotEmpty(t, dto.ID)
	require.Equal(t, "sam", dto.Subject)
}

func TestContacts_POST_RejectBothCardDAVAndJID(t *testing.T) {
	e, _, _, _ := buildContactsHandler(t)
	rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts", `{
		"subject": "x",
		"jid": "1@s.whatsapp.net",
		"carddav_uid": "vc-1"
	}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestContacts_POST_RejectNeither(t *testing.T) {
	e, _, _, _ := buildContactsHandler(t)
	rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts", `{"subject":"x","fact":"y"}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestContacts_DELETE_RemovesPair(t *testing.T) {
	e, mem, link, _ := buildContactsHandler(t)
	rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts", `{"subject":"fam","fact":"x","jid":"120001@g.us"}`)
	require.Equal(t, http.StatusCreated, rr.Code)
	var dto groupDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &dto))

	rr = doContactsRequest(t, e, http.MethodDelete, "/api/contacts/"+dto.ID, "")
	require.Equal(t, http.StatusNoContent, rr.Code)

	got, _ := mem.GetByID(context.Background(), dto.ID)
	require.Nil(t, got, "memory fact soft-deleted")
	gotLink, _ := link.GetByID(context.Background(), dto.ID)
	require.Nil(t, gotLink, "link soft-deleted")
}

func TestContacts_PATCH_UpdatesPreferredPhone(t *testing.T) {
	e, _, link, cd := buildContactsHandler(t)
	require.NoError(t, cd.Upsert(context.Background(), store.CardDAVContact{
		UID: "vc-sam", DisplayName: "Sam Carter",
		Phones: store.EncodePhones([]store.Phone{{Value: "+1"}, {Value: "+2"}}),
	}))
	rr := doContactsRequest(t, e, http.MethodPost, "/api/contacts", `{"subject":"sam","fact":"wife","carddav_uid":"vc-sam","preferred_phone":"+1"}`)
	require.Equal(t, http.StatusCreated, rr.Code)
	var dto aliasDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &dto))

	rr = doContactsRequest(t, e, http.MethodPatch, "/api/contacts/"+dto.ID, `{"preferred_phone":"+2"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	row, _ := link.GetByID(context.Background(), dto.ID)
	require.Equal(t, "+2", row.PreferredPhone)
}

func TestContacts_CardDAVSearch(t *testing.T) {
	e, _, _, cd := buildContactsHandler(t)
	require.NoError(t, cd.Upsert(context.Background(), store.CardDAVContact{
		UID: "vc-sam", DisplayName: "Sam Carter",
		Phones:    store.EncodePhones([]store.Phone{{Value: "+1", Types: []string{"CELL"}}}),
		Nicknames: store.EncodeStringList([]string{"Sammy"}),
	}))
	require.NoError(t, cd.Upsert(context.Background(), store.CardDAVContact{
		UID: "vc-other", DisplayName: "Bob Smith",
		Phones: store.EncodePhones([]store.Phone{{Value: "+9"}}),
	}))

	rr := doContactsRequest(t, e, http.MethodGet, "/api/contacts/carddav?q=sam", "")
	require.Equal(t, http.StatusOK, rr.Code)
	var resp cardDAVListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Contacts, 1)
	require.Equal(t, "Sam Carter", resp.Contacts[0].DisplayName)
	require.Len(t, resp.Contacts[0].Phones, 1)
}

func TestContacts_GET_CardDAVDisabledShape(t *testing.T) {
	// When CardDAV repo is nil, the response signals carddav_enabled=false
	// and the contacts directory is empty (groups can still exist).
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	mem := &store.MemoryRepo{DB: db}
	require.NoError(t, mem.Migrate())
	link := &store.ContactLinkRepo{DB: db}
	require.NoError(t, link.Migrate())

	e := echo.New()
	(&ContactsHandler{
		Memory:  mem,
		Link:    link,
		CardDAV: nil, // disabled
		Now:     time.Now,
	}).Register(e)

	rr := doContactsRequest(t, e, http.MethodGet, "/api/contacts", "")
	require.Equal(t, http.StatusOK, rr.Code)
	var resp contactsListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.False(t, resp.CardDAVEnabled)
}
