import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  listUsers,
  listInvites,
  setUserScope,
  removeUser,
  createInvite,
  revokeInvite,
  type UserInfo,
  type InviteCreated,
} from '../lib/adminOps'
import { useProjectsQuery } from '../lib/queries'
import { useToast } from '../lib/toast'
import {
  parseScope,
  serializeScope,
  describeScope,
  levelLabel,
  type Grant,
  type Level,
} from '../lib/scope'
import styles from './UserManagementPage.module.css'

// User management (B-28 stage 3), the super-user-only Settings item. Lists users
// (SELF is omitted server-side, so self-deletion/demotion is structurally
// impossible), edits each user's scope (one grant per namespace + a wildcard option),
// removes users, and mints time-limited single-use invite codes shown on screen.
export function UserManagementPage() {
  const qc = useQueryClient()
  const { add: toast } = useToast()
  const users = useQuery({ queryKey: ['admin-users'], queryFn: listUsers })
  const invites = useQuery({ queryKey: ['admin-invites'], queryFn: listInvites })
  const { data: projects = [] } = useProjectsQuery()
  const namespaces = useMemo(
    () => Array.from(new Set(projects.map((p) => p.namespace))).sort(),
    [projects],
  )

  const [editing, setEditing] = useState<string | null>(null)

  function refreshUsers() {
    void qc.invalidateQueries({ queryKey: ['admin-users'] })
  }
  function refreshInvites() {
    void qc.invalidateQueries({ queryKey: ['admin-invites'] })
  }

  async function onSaveScope(email: string, scope: string) {
    try {
      await setUserScope(email, scope)
      toast({ level: 'warn', text: `Updated permissions for ${email}.` })
      setEditing(null)
      refreshUsers()
    } catch (e) {
      toast({ level: 'warn', text: msg(e) })
    }
  }

  async function onRemove(email: string) {
    if (!window.confirm(`Remove ${email}? This deletes the account and logs them out.`)) return
    try {
      await removeUser(email)
      toast({ level: 'warn', text: `Removed ${email}.` })
      refreshUsers()
    } catch (e) {
      toast({ level: 'warn', text: msg(e) })
    }
  }

  return (
    <div className={styles.page}>
      <h1 className={styles.title}>User management</h1>

      <section>
        <h2 className={styles.h2}>Users</h2>
        {users.isError ? (
          <p className={styles.err}>{msg(users.error)}</p>
        ) : !users.data ? (
          <p className={styles.muted}>Loading…</p>
        ) : users.data.users.length === 0 ? (
          <p className={styles.muted}>No other users yet. Invite someone below.</p>
        ) : (
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Email</th>
                <th>Name</th>
                <th>Permissions</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {users.data.users.map((u) => (
                <UserRow
                  key={u.email}
                  user={u}
                  namespaces={namespaces}
                  editing={editing === u.email}
                  onEdit={() => setEditing(u.email)}
                  onCancel={() => setEditing(null)}
                  onSave={(scope) => onSaveScope(u.email, scope)}
                  onRemove={() => onRemove(u.email)}
                />
              ))}
            </tbody>
          </table>
        )}
      </section>

      <InviteSection
        namespaces={namespaces}
        invites={invites.data?.invites ?? []}
        onCreated={refreshInvites}
        onRevoked={refreshInvites}
      />
    </div>
  )
}

function UserRow({
  user,
  namespaces,
  editing,
  onEdit,
  onCancel,
  onSave,
  onRemove,
}: {
  user: UserInfo
  namespaces: string[]
  editing: boolean
  onEdit: () => void
  onCancel: () => void
  onSave: (scope: string) => void
  onRemove: () => void
}) {
  return (
    <>
      <tr>
        <td className={styles.mono}>{user.email}</td>
        <td>{user.display_name}</td>
        <td>{describeScope(user.scope)}</td>
        <td className={styles.actions}>
          {!editing && (
            <>
              <button className={styles.btn} onClick={onEdit}>
                Edit permissions
              </button>
              <button className={`${styles.btn} ${styles.danger}`} onClick={onRemove}>
                Remove
              </button>
            </>
          )}
        </td>
      </tr>
      {editing && (
        <tr>
          <td colSpan={4}>
            <ScopeEditor
              initial={user.scope}
              namespaces={namespaces}
              onCancel={onCancel}
              onSave={onSave}
            />
          </td>
        </tr>
      )}
    </>
  )
}

const LEVELS: Level[] = ['r', 'rw', 'admin']

// ScopeEditor edits a scope as one grant per target. A wildcard row (super-user when
// admin) plus per-namespace rows. Saving serializes to the scope grammar; an empty
// result is blocked (the server would read "" as super-user — a footgun).
function ScopeEditor({
  initial,
  namespaces,
  onCancel,
  onSave,
}: {
  initial: string
  namespaces: string[]
  onCancel: () => void
  onSave: (scope: string) => void
}) {
  const [grants, setGrants] = useState<Grant[]>(() => parseScope(initial))

  function setGrant(idx: number, patch: Partial<Grant>) {
    setGrants((g) => g.map((x, i) => (i === idx ? { ...x, ...patch } : x)))
  }
  function addRow() {
    setGrants((g) => [...g, { target: namespaces[0] ?? '', level: 'rw' }])
  }
  function removeRow(idx: number) {
    setGrants((g) => g.filter((_, i) => i !== idx))
  }

  const serialized = serializeScope(grants.filter((g) => g.target.trim() !== ''))
  const empty = serialized === ''
  // Duplicate non-wildcard targets (the UI should not produce them).
  const targets = grants.map((g) => g.target)
  const hasDup = targets.some((t, i) => t !== '' && targets.indexOf(t) !== i)

  return (
    <div className={styles.editor} aria-label="Scope editor">
      {grants.map((g, i) => (
        <div key={i} className={styles.editorRow}>
          {g.target === '*' ? (
            <span className={styles.wildcard}>All namespaces (*)</span>
          ) : (
            <input
              className={styles.nsInput}
              list="ns-options"
              value={g.target}
              placeholder="namespace"
              onChange={(e) => setGrant(i, { target: e.target.value.trim() })}
              aria-label="namespace"
            />
          )}
          <select
            value={g.level}
            onChange={(e) => setGrant(i, { level: e.target.value as Level })}
            aria-label="level"
          >
            {LEVELS.map((l) => (
              <option key={l} value={l}>
                {levelLabel(l)}
              </option>
            ))}
          </select>
          <button className={styles.btn} onClick={() => removeRow(i)} aria-label="remove grant">
            ✕
          </button>
        </div>
      ))}
      <datalist id="ns-options">
        {namespaces.map((n) => (
          <option key={n} value={n} />
        ))}
      </datalist>
      <div className={styles.editorActions}>
        <button className={styles.btn} onClick={addRow}>
          + Add namespace
        </button>
        {!grants.some((g) => g.target === '*') && (
          // A wildcard *:admin subsumes every per-namespace grant (authz max-wins), so
          // adding "All namespaces" REPLACES the individual rows with the single wildcard.
          <button className={styles.btn} onClick={() => setGrants([{ target: '*', level: 'admin' }])}>
            + Wildcard (all)
          </button>
        )}
        <span className={styles.spacer} />
        <button className={styles.btn} onClick={onCancel}>
          Cancel
        </button>
        <button
          className={`${styles.btn} ${styles.primary}`}
          disabled={empty || hasDup}
          title={empty ? 'Select at least one grant' : hasDup ? 'Duplicate namespace' : ''}
          onClick={() => onSave(serialized)}
        >
          Save
        </button>
      </div>
      {empty && <p className={styles.warn}>Select at least one grant (an empty scope means super-user).</p>}
      {hasDup && <p className={styles.warn}>Each namespace may appear only once.</p>}
    </div>
  )
}

function InviteSection({
  namespaces,
  invites,
  onCreated,
  onRevoked,
}: {
  namespaces: string[]
  invites: import('../lib/adminOps').InviteInfo[]
  onCreated: () => void
  onRevoked: () => void
}) {
  const { add: toast } = useToast()
  const [email, setEmail] = useState('')
  const [grants, setGrants] = useState<Grant[]>([{ target: '', level: 'rw' }])
  const [created, setCreated] = useState<InviteCreated | null>(null)
  const [copied, setCopied] = useState(false)

  // Belt-and-braces for the displayed code: the one-shot code lives only in `created`
  // (it cannot be re-derived — the list carries the hash, never the code), so the only
  // correct action when its backing invite vanishes is to clear it. seenRef guards the
  // create-time race: the freshly-minted invite is not in the (stale) list until the
  // refetch lands, so we clear ONLY a code we have already observed present and which
  // then disappears (revoked elsewhere / used / expired). The same-page revoke is
  // cleared synchronously in onRevoke below.
  const seenRef = useRef<string | null>(null)
  useEffect(() => {
    if (!created) {
      seenRef.current = null
      return
    }
    if (invites.some((inv) => inv.code_hash === created.code_hash)) {
      seenRef.current = created.code_hash
    } else if (seenRef.current === created.code_hash) {
      setCreated(null)
    }
  }, [invites, created])

  function setGrant(idx: number, patch: Partial<Grant>) {
    setGrants((g) => g.map((x, i) => (i === idx ? { ...x, ...patch } : x)))
  }

  async function onCreate(e: React.FormEvent) {
    e.preventDefault()
    const scope = serializeScope(grants.filter((g) => g.target.trim() !== ''))
    if (!email.trim() || !scope) {
      toast({ level: 'warn', text: 'An email and at least one grant are required.' })
      return
    }
    try {
      const inv = await createInvite(email.trim(), scope)
      setCreated(inv)
      setCopied(false)
      setEmail('')
      onCreated()
    } catch (err) {
      toast({ level: 'warn', text: msg(err) })
    }
  }

  async function onRevoke(codeHash: string) {
    try {
      await revokeInvite(codeHash)
      // The displayed one-shot code belongs to a pending invite; if that is the invite
      // just revoked, the code is now invalid (and not redeemable) — clear it so a stale
      // code can never linger on screen.
      if (created && created.code_hash === codeHash) setCreated(null)
      onRevoked()
    } catch (err) {
      toast({ level: 'warn', text: msg(err) })
    }
  }

  // Copy the one-shot invite code to the clipboard (the ConnectionsPage IssuedTokenPanel
  // idiom): optimistic "Copied" label, a toast on the rare clipboard failure.
  function copyCode() {
    if (!created) return
    void navigator.clipboard
      ?.writeText(created.code)
      .then(() => setCopied(true))
      .catch(() => toast({ level: 'warn', text: 'Could not copy — select and copy manually.' }))
  }

  // A non-wildcard grant row with an empty namespace is invalid; surface it BEFORE
  // submit (red frame on the input, disabled submit) instead of only erroring on submit.
  const hasEmptyNs = grants.some((g) => g.target !== '*' && g.target.trim() === '')

  return (
    <section className={styles.invites}>
      <h2 className={styles.h2}>Invitations</h2>
      <p className={styles.muted}>
        Generate a single-use code and convey it to the invitee yourself — Shoka does not send email.
      </p>
      <form className={styles.inviteForm} onSubmit={onCreate} aria-label="Create invite">
        <input
          type="email"
          placeholder="invitee email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          aria-label="invitee email"
          required
        />
        <div className={styles.editor}>
          {grants.map((g, i) => (
            <div key={i} className={styles.editorRow}>
              {g.target === '*' ? (
                <span className={styles.wildcard}>All namespaces (*)</span>
              ) : (
                <input
                  className={`${styles.nsInput} ${g.target.trim() === '' ? styles.invalid : ''}`}
                  list="ns-options-invite"
                  value={g.target}
                  placeholder="namespace"
                  onChange={(e) => setGrant(i, { target: e.target.value.trim() })}
                  aria-label="namespace"
                  aria-invalid={g.target.trim() === '' || undefined}
                />
              )}
              <select value={g.level} onChange={(e) => setGrant(i, { level: e.target.value as Level })} aria-label="level">
                {LEVELS.map((l) => (
                  <option key={l} value={l}>
                    {levelLabel(l)}
                  </option>
                ))}
              </select>
              <button type="button" className={styles.btn} onClick={() => setGrants((g) => g.filter((_, j) => j !== i))} aria-label="remove grant">
                ✕
              </button>
            </div>
          ))}
          <datalist id="ns-options-invite">
            {namespaces.map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
          <div className={styles.editorActions}>
            <button type="button" className={styles.btn} onClick={() => setGrants((g) => [...g, { target: '', level: 'rw' }])}>
              + Add namespace
            </button>
            {!grants.some((g) => g.target === '*') && (
              // "All namespaces" (*:admin) subsumes every per-namespace grant, so adding
              // it REPLACES the individual rows with the single wildcard row.
              <button type="button" className={styles.btn} onClick={() => setGrants([{ target: '*', level: 'admin' }])}>
                + Wildcard (all)
              </button>
            )}
          </div>
        </div>
        <button
          type="submit"
          className={`${styles.btn} ${styles.primary}`}
          disabled={hasEmptyNs}
          title={hasEmptyNs ? 'Fill in every namespace, or remove the empty row' : ''}
        >
          Generate invite code
        </button>
      </form>

      {created && (
        <div className={styles.codeBox} role="status">
          <div>
            Invite for <strong>{created.email}</strong> ({describeScope(created.scope)}):
          </div>
          <div className={styles.codeRow}>
            <code className={styles.code}>{created.code}</code>
            <button
              type="button"
              className={styles.btn}
              onClick={copyCode}
              aria-label="Copy invite code"
            >
              {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
          <div className={styles.muted}>Copy this now — it is shown only once.</div>
        </div>
      )}

      <h3 className={styles.h3}>Pending invites</h3>
      {invites.length === 0 ? (
        <p className={styles.muted}>None.</p>
      ) : (
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Email</th>
              <th>Permissions</th>
              <th>Status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {invites.map((inv) => (
              <tr key={inv.code_hash}>
                <td className={styles.mono}>{inv.email}</td>
                <td>{describeScope(inv.scope)}</td>
                <td>{inv.used ? 'used' : 'pending'}</td>
                <td className={styles.actions}>
                  <button className={`${styles.btn} ${styles.danger}`} onClick={() => onRevoke(inv.code_hash)}>
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

function msg(e: unknown): string {
  return e instanceof Error ? e.message : 'Something went wrong.'
}
