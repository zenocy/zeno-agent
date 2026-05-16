import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

// Phone is one TEL value with type tags + optional PREF priority.
export interface Phone {
  value: string;
  types?: string[];
  pref?: number;
}

// Email is one EMAIL value with type tags. Reserved for future use.
export interface Email {
  value: string;
  types?: string[];
}

// Alias is a user-curated nickname → CardDAV-contact mapping.
// "wife" → Sam Carter, with an optional preferred-phone override that
// pins which TEL the WhatsApp send path should use.
export interface Alias {
  id: string;
  subject: string;
  fact?: string;
  preferred_phone?: string;
}

// DirectoryContact is one row in the unified address book — full vCard
// detail plus any aliases the user attached.
export interface DirectoryContact {
  uid: string;
  display_name: string;
  given_name?: string;
  family_name?: string;
  nicknames?: string[];
  phones?: Phone[];
  emails?: Email[];
  aliases?: Alias[];
}

// Group is one labelled WhatsApp group. Group JIDs do not exist in the
// CardDAV book; the user adds them by hand or picks them from the
// receive log (future).
export interface Group {
  id: string;
  subject: string;
  fact?: string;
  jid: string;
}

// ContactsResponse is the unified shape from GET /api/contacts.
//
// `carddav_enabled` is true when the daemon has a CardDAV repo wired
// (i.e. `sensors.carddav.enabled: true`). When false, the directory is
// always empty regardless of `contacts` length.
export interface ContactsResponse {
  carddav_enabled: boolean;
  last_sync_at?: string;
  contacts: DirectoryContact[];
  groups: Group[];
}

export function useContacts() {
  return useQuery<ContactsResponse>({
    queryKey: ["contacts"],
    queryFn: async () => {
      const r = await fetch("/api/contacts");
      if (!r.ok) throw new Error(`/api/contacts returned ${r.status}`);
      return r.json();
    },
    refetchInterval: false,
    staleTime: 60_000,
  });
}

// CreateAliasInput → POST /api/contacts to attach an alias to a CardDAV
// contact. carddav_uid is required; preferred_phone overrides the
// vCard's default TEL.
export interface CreateAliasInput {
  subject: string;
  fact?: string;
  carddav_uid: string;
  preferred_phone?: string;
}

// CreateGroupInput → POST /api/contacts to register a labelled group.
// The JID must end in @g.us.
export interface CreateGroupInput {
  subject: string;
  fact?: string;
  jid: string;
}

export function useCreateAlias() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: CreateAliasInput) => {
      const r = await fetch("/api/contacts", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(input),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(text || `POST /api/contacts ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["contacts"] });
    },
  });
}

export function useCreateGroup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: CreateGroupInput) => {
      const r = await fetch("/api/contacts", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(input),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(text || `POST /api/contacts ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["contacts"] });
    },
  });
}

// useDeleteContact removes either an alias or a group by id. The
// backend dispatches by row type — the UI just hands over the id.
export function useDeleteContact() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const r = await fetch(`/api/contacts/${encodeURIComponent(id)}`, {
        method: "DELETE",
      });
      if (!r.ok && r.status !== 204) {
        throw new Error(`DELETE /api/contacts/${id} ${r.status}`);
      }
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["contacts"] });
    },
  });
}

// CardDAVSearchResponse is the wire shape of GET /api/contacts/carddav.
// Used as a fallback when the directory list rendered client-side is
// truncated for very large books.
export interface CardDAVSearchResponse {
  contacts: {
    uid: string;
    display_name: string;
    nicknames?: string[];
    phones?: Phone[];
  }[];
}

export function useCardDAVSearch(q: string, enabled: boolean = true) {
  return useQuery<CardDAVSearchResponse>({
    queryKey: ["carddav-search", q],
    queryFn: async () => {
      const r = await fetch(`/api/contacts/carddav?q=${encodeURIComponent(q)}`);
      if (!r.ok) throw new Error(`/api/contacts/carddav returned ${r.status}`);
      return r.json();
    },
    enabled,
    staleTime: 30_000,
  });
}
