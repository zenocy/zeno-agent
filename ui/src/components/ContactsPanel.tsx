import { useMemo, useState } from "react";
import { Trash2, Plus, Users, BookOpen, Phone as PhoneIcon, Mail, X, Star } from "lucide-react";

import {
  useContacts,
  useCreateAlias,
  useCreateGroup,
  useDeleteContact,
  type Alias,
  type DirectoryContact,
  type Group,
  type Phone,
} from "../api/useContacts";

// ContactsPanel renders the user's WhatsApp directory:
//
//   - "Contacts" — the imported CardDAV book. Every vCard surfaces with
//     its full phone / email / nickname detail. An inline [+ Add alias]
//     per row maps a friendly name (wife, mom, etc.) to that contact.
//   - "Groups" — labelled group JIDs. Group JIDs are not in any address
//     book; the user adds them by hand.
//
// Sending a WhatsApp from the input bar reaches any contact in this
// directory. The receive allowlist (who can DM Zeno) is curated
// separately via the WhatsApp pairing modal — a footer note links
// there.
export function ContactsPanel() {
  const { data, isLoading } = useContacts();
  const [filter, setFilter] = useState("");

  const enabled = data?.carddav_enabled ?? false;
  const contacts = data?.contacts ?? [];
  const groups = data?.groups ?? [];

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return contacts;
    return contacts.filter((c) => {
      if (c.display_name.toLowerCase().includes(q)) return true;
      if (c.nicknames?.some((n) => n.toLowerCase().includes(q))) return true;
      if (c.aliases?.some((a) => a.subject.toLowerCase().includes(q))) return true;
      return false;
    });
  }, [contacts, filter]);

  return (
    <div className="px-8 py-6 max-w-3xl mx-auto space-y-8">
      <section>
        <SectionHeader title="Contacts" icon={<BookOpen className="h-3.5 w-3.5" />}>
          {data?.last_sync_at ? (
            <span className="text-[11px] font-mono text-ink-5">
              synced {fmtRelative(data.last_sync_at)}
            </span>
          ) : null}
        </SectionHeader>

        {!enabled && (
          <p className="text-[12px] text-ink-4 mb-3">
            CardDAV import is off. Set <code className="font-mono">sensors.carddav.enabled: true</code>{" "}
            and configure a username + password in <code className="font-mono">config.yaml</code>,
            then restart.
          </p>
        )}

        {enabled && (
          <input
            type="text"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter contacts…"
            className="w-full h-8 px-2 mb-3 rounded-z-sm border border-line bg-bg text-[13px] text-ink"
          />
        )}

        {isLoading ? (
          <p className="text-[12px] text-ink-4">Loading…</p>
        ) : enabled && filtered.length === 0 ? (
          <p className="text-[12px] text-ink-4">
            {filter ? "No contacts match." : "No contacts imported yet."}
          </p>
        ) : (
          <ul className="space-y-2">
            {filtered.map((c) => (
              <ContactCard key={c.uid} contact={c} />
            ))}
          </ul>
        )}
      </section>

      <section>
        <SectionHeader title="Groups" icon={<Users className="h-3.5 w-3.5" />} />
        {groups.length === 0 ? (
          <p className="text-[12px] text-ink-4">No WhatsApp groups linked yet.</p>
        ) : (
          <ul className="space-y-1">
            {groups.map((g) => (
              <GroupRow key={g.id} group={g} />
            ))}
          </ul>
        )}
        <AddGroupForm />
      </section>

      <section className="border-t border-line pt-4">
        <h3 className="font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1">
          Who can message Zeno
        </h3>
        <p className="text-[12px] text-ink-4 leading-snug">
          Sending works for anyone in your contacts above. To control which
          numbers Zeno will <em>receive</em> DMs from, edit the allowlist in
          the WhatsApp pairing modal (Settings → WhatsApp).
        </p>
      </section>
    </div>
  );
}

function SectionHeader({
  title,
  icon,
  children,
}: {
  title: string;
  icon: React.ReactNode;
  children?: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between gap-2 mb-2">
      <div className="flex items-center gap-1.5">
        <span className="text-ink-4">{icon}</span>
        <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-5">
          {title}
        </h2>
      </div>
      {children}
    </div>
  );
}

function ContactCard({ contact }: { contact: DirectoryContact }) {
  const [adding, setAdding] = useState(false);
  return (
    <li className="border border-line rounded-z-sm p-3 bg-bg">
      <div className="flex items-baseline justify-between gap-2 mb-1.5">
        <div className="font-display text-[14px] font-[500] text-ink">{contact.display_name}</div>
        {contact.nicknames && contact.nicknames.length > 0 && (
          <span className="text-[11px] text-ink-5">
            aka {contact.nicknames.join(", ")}
          </span>
        )}
      </div>

      {(contact.phones?.length ?? 0) > 0 ? (
        <ul className="space-y-0.5 mb-1">
          {contact.phones!.map((p, i) => (
            <PhoneItem key={i} phone={p} />
          ))}
        </ul>
      ) : (
        <p className="text-[11px] italic text-ink-5 mb-1">No phone number on this vCard.</p>
      )}

      {(contact.emails?.length ?? 0) > 0 && (
        <ul className="space-y-0.5 mb-1">
          {contact.emails!.map((e, i) => (
            <li
              key={i}
              className="flex items-center gap-1.5 text-[12px] text-ink-3"
            >
              <Mail className="h-3 w-3 text-ink-5" />
              <span className="font-mono">{e.value}</span>
              {e.types && e.types.length > 0 && (
                <span className="text-[10px] uppercase text-ink-5">
                  {e.types.join(", ")}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}

      {/* Aliases (or empty state) */}
      <div className="mt-2 pt-2 border-t border-line">
        {(contact.aliases?.length ?? 0) > 0 && (
          <ul className="space-y-1 mb-1">
            {contact.aliases!.map((a) => (
              <AliasRow key={a.id} alias={a} contact={contact} />
            ))}
          </ul>
        )}
        {!adding ? (
          <button
            type="button"
            onClick={() => setAdding(true)}
            className="inline-flex items-center gap-1 text-[12px] text-ink-3 hover:text-ink-2"
          >
            <Plus className="h-3 w-3" /> Add alias
          </button>
        ) : (
          <AddAliasForm
            contact={contact}
            onClose={() => setAdding(false)}
          />
        )}
      </div>
    </li>
  );
}

function PhoneItem({ phone }: { phone: Phone }) {
  const isPreferred = phone.pref === 1 || (phone.types?.some((t) => /^cell|mobile$/i.test(t)) ?? false);
  return (
    <li className="flex items-center gap-1.5 text-[12px] text-ink-3">
      <PhoneIcon className="h-3 w-3 text-ink-5" />
      <span className="font-mono">{phone.value}</span>
      {phone.types && phone.types.length > 0 && (
        <span className="text-[10px] uppercase text-ink-5">
          {phone.types.join(", ")}
        </span>
      )}
      {isPreferred && (
        <span className="inline-flex items-center gap-0.5 text-[10px] text-accent">
          <Star className="h-2.5 w-2.5" /> preferred
        </span>
      )}
    </li>
  );
}

function AliasRow({ alias, contact }: { alias: Alias; contact: DirectoryContact }) {
  const del = useDeleteContact();
  const phoneLabel = useMemo(() => {
    if (!alias.preferred_phone) return null;
    const match = contact.phones?.find((p) => p.value === alias.preferred_phone);
    if (!match) return alias.preferred_phone;
    return match.types?.[0] ?? alias.preferred_phone;
  }, [alias.preferred_phone, contact.phones]);

  return (
    <li className="flex items-center justify-between gap-2 text-[12px]">
      <div className="min-w-0">
        <span className="text-ink font-[500]">{alias.subject}</span>
        {phoneLabel && (
          <span className="text-ink-5 ml-1.5">— uses {phoneLabel}</span>
        )}
        {alias.fact && (
          <span className="text-ink-4 ml-1.5">· {alias.fact}</span>
        )}
      </div>
      <button
        type="button"
        onClick={() => del.mutate(alias.id)}
        aria-label={`Remove alias ${alias.subject}`}
        className="text-ink-5 hover:text-crit p-0.5"
      >
        <Trash2 className="h-3 w-3" />
      </button>
    </li>
  );
}

function AddAliasForm({
  contact,
  onClose,
}: {
  contact: DirectoryContact;
  onClose: () => void;
}) {
  const create = useCreateAlias();
  const [subject, setSubject] = useState("");
  const [phone, setPhone] = useState(
    contact.phones?.find((p) => p.pref === 1)?.value ?? contact.phones?.[0]?.value ?? ""
  );
  const [fact, setFact] = useState("");
  const [error, setError] = useState<string | null>(null);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!subject.trim()) return;
    create.mutate(
      {
        subject: subject.trim().toLowerCase(),
        fact: fact.trim() || undefined,
        carddav_uid: contact.uid,
        preferred_phone: phone || undefined,
      },
      {
        onError: (err) => setError(err instanceof Error ? err.message : "Add failed"),
        onSuccess: () => {
          setSubject("");
          setFact("");
          onClose();
        },
      }
    );
  };

  return (
    <form onSubmit={submit} className="space-y-1.5 mt-1">
      <input
        autoFocus
        type="text"
        value={subject}
        onChange={(e) => setSubject(e.target.value)}
        placeholder="Alias (e.g. wife)"
        className="w-full h-7 px-2 rounded-z-sm border border-line bg-bg text-[12px] text-ink"
      />
      {(contact.phones?.length ?? 0) > 1 && (
        <select
          value={phone}
          onChange={(e) => setPhone(e.target.value)}
          className="w-full h-7 px-2 rounded-z-sm border border-line bg-bg text-[12px] text-ink"
        >
          {contact.phones!.map((p) => (
            <option key={p.value} value={p.value}>
              {p.value}
              {p.types?.length ? ` (${p.types.join(", ")})` : ""}
            </option>
          ))}
        </select>
      )}
      <input
        type="text"
        value={fact}
        onChange={(e) => setFact(e.target.value)}
        placeholder="Optional context (e.g. Wife. Also called Sam.)"
        className="w-full h-7 px-2 rounded-z-sm border border-line bg-bg text-[12px] text-ink"
      />
      {error && <span className="text-[11px] text-crit">{error}</span>}
      <div className="flex justify-end gap-1.5">
        <button
          type="button"
          onClick={onClose}
          className="h-7 px-2 rounded-z-sm border border-line text-[11px] text-ink-3"
        >
          Cancel
        </button>
        <button
          type="submit"
          disabled={create.isPending || !subject.trim()}
          className="inline-flex items-center gap-1 h-7 px-2 rounded-z-sm bg-accent text-white text-[11px] font-[500] disabled:opacity-50"
        >
          <Plus className="h-3 w-3" /> Save alias
        </button>
      </div>
    </form>
  );
}

function GroupRow({ group }: { group: Group }) {
  const del = useDeleteContact();
  return (
    <li className="flex items-start justify-between gap-3 py-2 border-b border-line">
      <div className="min-w-0">
        <div className="font-display text-[14px] text-ink truncate">{group.subject}</div>
        {group.fact && (
          <p className="text-[12px] text-ink-3 leading-snug mt-0.5">{group.fact}</p>
        )}
        <p className="text-[11px] font-mono text-ink-5 mt-0.5">{group.jid}</p>
      </div>
      <button
        type="button"
        onClick={() => del.mutate(group.id)}
        aria-label={`Remove ${group.subject}`}
        className="text-ink-4 hover:text-crit p-1"
      >
        <Trash2 className="h-3.5 w-3.5" />
      </button>
    </li>
  );
}

function AddGroupForm() {
  const [name, setName] = useState("");
  const [jid, setJid] = useState("");
  const [fact, setFact] = useState("");
  const create = useCreateGroup();
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!name.trim() || !jid.trim()) return;
    if (!jid.endsWith("@g.us")) {
      setError("Group JIDs end with @g.us");
      return;
    }
    create.mutate(
      { subject: name.trim().toLowerCase(), fact: fact.trim() || undefined, jid: jid.trim() },
      {
        onError: (err) => setError(err instanceof Error ? err.message : "Add failed"),
        onSuccess: () => {
          setName("");
          setJid("");
          setFact("");
          setOpen(false);
        },
      }
    );
  };

  if (!open) {
    return (
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="mt-2 inline-flex items-center gap-1 text-[12px] text-ink-3 hover:text-ink-2"
      >
        <Plus className="h-3 w-3" /> Add group
      </button>
    );
  }

  return (
    <form onSubmit={submit} className="mt-3 flex flex-col gap-1.5">
      <div className="flex gap-1.5">
        <input
          autoFocus
          type="text"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Group label (e.g. family)"
          className="flex-1 h-8 px-2 rounded-z-sm border border-line bg-bg text-[13px] text-ink"
        />
        <input
          type="text"
          value={jid}
          onChange={(e) => setJid(e.target.value)}
          placeholder="120001@g.us"
          className="flex-1 h-8 px-2 rounded-z-sm border border-line bg-bg text-[13px] font-mono text-ink"
        />
      </div>
      <input
        type="text"
        value={fact}
        onChange={(e) => setFact(e.target.value)}
        placeholder="Optional context (e.g. Living-room family chat)"
        className="h-8 px-2 rounded-z-sm border border-line bg-bg text-[13px] text-ink"
      />
      {error && <span className="text-[11px] text-crit">{error}</span>}
      <div className="flex justify-end gap-2">
        <button
          type="button"
          onClick={() => setOpen(false)}
          className="h-8 px-3 rounded-z-sm border border-line text-[12px] text-ink-3"
          aria-label="Cancel adding group"
        >
          <X className="h-3 w-3" />
        </button>
        <button
          type="submit"
          disabled={create.isPending || !name.trim() || !jid.trim()}
          className="inline-flex items-center gap-1 h-8 px-3 rounded-z-sm bg-accent text-white text-[12px] font-[500] disabled:opacity-50"
        >
          <Plus className="h-3 w-3" /> Add group
        </button>
      </div>
    </form>
  );
}

function fmtRelative(iso: string): string {
  const t = new Date(iso).getTime();
  if (isNaN(t)) return "";
  const sec = Math.round((Date.now() - t) / 1000);
  if (sec < 60) return "just now";
  const min = Math.round(sec / 60);
  if (min < 60) return `${min} min ago`;
  const hr = Math.round(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const d = Math.round(hr / 24);
  return `${d}d ago`;
}
