import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useIsAdmin } from '../lib/admin'
import {
  useConnectionsQuery,
  useDomainsQuery,
  useConfidentialClientsQuery,
  OAUTH_CONNECTIONS_KEY,
  OAUTH_DOMAINS_KEY,
  OAUTH_CLIENTS_KEY,
} from '../lib/queries'
import {
  revokeConnection,
  issueSelfToken,
  clientDomain,
  OAuthDeniedError,
} from '../lib/oauthOps'
import { createDomain, updateDomain, deleteDomain } from '../lib/domainOps'
import {
  issueConfidentialClient,
  revokeConfidentialClient,
} from '../lib/confidentialOps'
import { useToast } from '../lib/toast'
import type {
  OAuthConnection,
  OAuthIssueSelfPayload,
  DomainInfo,
  ConfidentialClientInfo,
  ConfidentialIssuePayload,
} from '../lib/types'
import styles from './ConnectionsPage.module.css'

// The administrator-only OAuth management view. Per the B-71 binding principle the screen is
// structured BY management mode; this is DOMAIN mode (B-71 Stage 2d): the operator manages
// trusted domains + their per-domain TTL + per-domain consent FROM THE SCREEN (the settings
// that used to live in static config — moved to the UI, not removed), and each domain's
// tokens group beneath its entry. The per-domain consent is WRITE-ONLY: it is set or cleared,
// never displayed (hashed at rest). Self-issued / confidential tokens sit in a separate
// section. The server-side gate is authoritative; useIsAdmin() governs UI exposure only.
export function ConnectionsPage() {
  const isAdmin = useIsAdmin()
  const [issued, setIssued] = useState<OAuthIssueSelfPayload | null>(null)
  const {
    data: connections,
    isLoading: connLoading,
    isFetching,
    error,
    refetch,
  } = useConnectionsQuery(isAdmin)
  const { data: domains, isLoading: domLoading, refetch: refetchDomains } =
    useDomainsQuery(isAdmin)

  if (!isAdmin) {
    return (
      <div className={styles.page}>
        <div className={styles.body}>
          <p className={styles.hint}>
            You are not authorized to manage OAuth connections.
          </p>
        </div>
      </div>
    )
  }

  const oauthDisabled =
    error instanceof OAuthDeniedError && error.reason === 'oauth_disabled'

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.scope}>
          <strong>OAuth connections</strong>
          <span className={styles.sub}> · trusted domains &amp; active authorizations</span>
        </span>
        <IssueTokenButton disabled={oauthDisabled} onIssued={setIssued} />
        <button
          className={styles.refresh}
          onClick={() => {
            void refetch()
            void refetchDomains()
          }}
          disabled={isFetching}
          aria-label="Refresh connections"
        >
          {isFetching ? 'Refreshing…' : 'Refresh'}
        </button>
      </div>

      {issued && (
        <IssuedTokenPanel token={issued} onDismiss={() => setIssued(null)} />
      )}

      <div className={styles.body} data-scroll-restoration-id="connections-body">
        {connLoading || domLoading ? (
          <p className={styles.hint}>Loading…</p>
        ) : oauthDisabled ? (
          <p className={styles.hint}>
            OAuth is not enabled on this server, so there is nothing to manage.
          </p>
        ) : error ? (
          <p className={styles.error}>Failed to load connections. Try Refresh.</p>
        ) : (
          <>
            <DomainManagement
              domains={domains ?? []}
              connections={connections ?? []}
            />
            <ConfidentialManagement />
          </>
        )}
      </div>
    </div>
  )
}

// DomainManagement is the domain-mode screen body: the Add-domain form, the list of trusted
// "domain" entries (each with its grouped tokens), and the separate self-issued/other section.
function DomainManagement({
  domains,
  connections,
}: {
  domains: DomainInfo[]
  connections: OAuthConnection[]
}) {
  const grouped = new Map<string, OAuthConnection[]>()
  const orphans: OAuthConnection[] = [] // domain === "" (self-issued/confidential)
  for (const c of connections) {
    if (!c.domain) {
      orphans.push(c)
      continue
    }
    const arr = grouped.get(c.domain) ?? []
    arr.push(c)
    grouped.set(c.domain, arr)
  }

  return (
    <>
      <section className={styles.domainsSection}>
        <h3 className={styles.sectionTitle}>Trusted domains</h3>
        <AddDomainForm />
        {domains.length === 0 ? (
          <p className={styles.hint} data-testid="domains-empty">
            No trusted domains configured. Add one to allow connections from it.
          </p>
        ) : (
          domains.map((d) => (
            <DomainCard key={d.id} domain={d} tokens={grouped.get(d.domain) ?? []} />
          ))
        )}
      </section>

      <section className={styles.selfSection} data-testid="self-issued-section">
        <h3 className={styles.sectionTitle}>Self-issued &amp; other tokens</h3>
        {orphans.length === 0 ? (
          <p className={styles.hint}>No self-issued tokens.</p>
        ) : (
          <TokensTable tokens={orphans} />
        )}
      </section>
    </>
  )
}

// ConfidentialManagement is the confidential-mode screen (B-71 Stage 3), composed alongside the
// domain-mode screen: the operator pre-issues a Client ID + Secret (with a scope + a finite
// expiry) for Claude.ai's custom-connector advanced settings. The secret is shown ONCE at
// issuance (never retrievable after); the list never carries it; revoke cascades to its tokens.
function ConfidentialManagement() {
  const { data: clients, isLoading } = useConfidentialClientsQuery(true)
  const [issued, setIssued] = useState<ConfidentialIssuePayload | null>(null)

  return (
    <section className={styles.domainsSection} data-testid="confidential-section">
      <h3 className={styles.sectionTitle}>Confidential clients (Client ID + Secret)</h3>
      <p className={styles.hintSmall}>
        Pre-issue a Client ID + Secret for a Claude.ai custom connector. The secret is shown once
        — copy it now. PKCE is still required in addition to the secret.
      </p>
      <IssueClientForm onIssued={setIssued} />
      {issued && (
        <IssuedClientPanel issued={issued} onDismiss={() => setIssued(null)} />
      )}
      {isLoading ? (
        <p className={styles.hint}>Loading…</p>
      ) : (clients ?? []).length === 0 ? (
        <p className={styles.hint} data-testid="confidential-empty">
          No confidential clients issued.
        </p>
      ) : (
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Client ID</th>
              <th>Scope</th>
              <th>Expires</th>
              <th>Issued</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {(clients ?? []).map((c) => (
              <ConfidentialClientRow key={c.id} client={c} />
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

// parseValidityDays: a confidential credential's validity in whole positive days (no indefinite).
// Returns null for a non-finite / non-positive / non-integer value.
function parseValidityDays(v: string): number | null {
  const n = Number(v)
  if (!Number.isInteger(n) || n <= 0) return null
  return n
}

function IssueClientForm({
  onIssued,
}: {
  onIssued: (p: ConfidentialIssuePayload) => void
}) {
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const [scope, setScope] = useState('')
  const [days, setDays] = useState('30')

  const validDays = parseValidityDays(days)
  const valid = scope.trim() !== '' && validDays !== null

  const issue = useMutation({
    mutationFn: () =>
      issueConfidentialClient({
        scope: scope.trim(),
        validitySeconds: (validDays ?? 0) * 86400,
      }),
    onSuccess: (p) => {
      void queryClient.invalidateQueries({ queryKey: OAUTH_CLIENTS_KEY })
      onIssued(p)
      setScope('')
      setDays('30')
    },
    onError: (e) =>
      addToast({
        level: 'warn',
        text: e instanceof Error ? e.message : 'Failed to issue a confidential client.',
      }),
  })

  return (
    <form
      className={styles.domainForm}
      onSubmit={(e) => {
        e.preventDefault()
        if (valid) issue.mutate()
      }}
    >
      <input
        className={styles.domainInput}
        placeholder="scope (e.g. namespace:docs:rw, or * for all access)"
        value={scope}
        onChange={(e) => setScope(e.target.value)}
        data-testid="client-issue-scope"
        aria-label="Pre-issued scope"
      />
      <input
        className={styles.ttlInput}
        placeholder="valid days"
        value={days}
        onChange={(e) => setDays(e.target.value)}
        data-testid="client-issue-validity"
        aria-label="Validity in days"
        aria-invalid={validDays === null}
      />
      <button
        type="submit"
        className={styles.issue}
        disabled={!valid || issue.isPending}
        data-testid="client-issue-submit"
      >
        {issue.isPending ? 'Issuing…' : 'Issue client'}
      </button>
      {!valid && (scope !== '' || days !== '30') && (
        <span className={styles.invalid}>
          A scope is required; validity must be a whole positive number of days (no indefinite).
        </span>
      )}
    </form>
  )
}

// IssuedClientPanel shows the freshly minted client_id + secret ONCE, with copy controls and a
// clear "shown only once" caption — the one place the raw secret crosses /ws/ui.
function IssuedClientPanel({
  issued,
  onDismiss,
}: {
  issued: ConfidentialIssuePayload
  onDismiss: () => void
}) {
  const { add: addToast } = useToast()
  const copy = (value: string, label: string) => {
    void navigator.clipboard
      ?.writeText(value)
      .then(() => addToast({ level: 'warn', text: `${label} copied.` }))
      .catch(() =>
        addToast({ level: 'warn', text: 'Could not copy — select and copy manually.' }),
      )
  }
  return (
    <div className={styles.tokenPanel} role="status" data-testid="client-issued-panel">
      <div className={styles.tokenWarn}>
        <strong>Copy the secret now.</strong> It is shown only once and cannot be retrieved
        again. Paste the Client ID and Secret into Claude.ai&apos;s connector advanced settings.
      </div>
      <div className={styles.tokenRow}>
        <code className={styles.tokenValue} data-testid="client-issued-id">
          {issued.client_id}
        </code>
        <button
          className={styles.copy}
          onClick={() => copy(issued.client_id, 'Client ID')}
          aria-label="Copy client id"
        >
          Copy ID
        </button>
      </div>
      <div className={styles.tokenRow}>
        <code className={styles.tokenValue} data-testid="client-issued-secret">
          {issued.client_secret}
        </code>
        <button
          className={styles.copy}
          onClick={() => copy(issued.client_secret, 'Client secret')}
          aria-label="Copy client secret"
        >
          Copy secret
        </button>
        <button className={styles.dismiss} onClick={onDismiss} aria-label="Dismiss">
          Done
        </button>
      </div>
      <div className={styles.tokenExpiry}>
        Scope {fmtScope(issued.scope)} · expires {fmtTime(issued.expires_at)}
      </div>
    </div>
  )
}

function ConfidentialClientRow({ client }: { client: ConfidentialClientInfo }) {
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const [confirming, setConfirming] = useState(false)

  const revoke = useMutation({
    mutationFn: () => revokeConfidentialClient(client.id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: OAUTH_CLIENTS_KEY })
      void queryClient.invalidateQueries({ queryKey: OAUTH_CONNECTIONS_KEY })
      addToast({ level: 'warn', text: 'Revoked the confidential client (its tokens were cut).' })
    },
    onError: (e) => {
      addToast({
        level: 'warn',
        text: e instanceof Error ? e.message : 'Failed to revoke the client.',
      })
      setConfirming(false)
    },
  })

  return (
    <tr data-testid={`client-row-${client.client_id}`}>
      <td className={styles.client}>
        <code className={styles.series}>{client.client_id}</code>
      </td>
      <td className={styles.scopeCell}>{fmtScope(client.scope)}</td>
      <td className={styles.time}>{fmtTime(client.expires_at)}</td>
      <td className={styles.time}>{fmtTime(client.created_at)}</td>
      <td className={styles.actions}>
        {confirming ? (
          <>
            <button
              className={styles.danger}
              disabled={revoke.isPending}
              onClick={() => revoke.mutate()}
              data-testid={`client-revoke-confirm-${client.client_id}`}
            >
              {revoke.isPending ? 'Revoking…' : 'Confirm revoke'}
            </button>
            <button className={styles.cancel} onClick={() => setConfirming(false)}>
              Cancel
            </button>
          </>
        ) : (
          <button
            className={styles.revoke}
            onClick={() => setConfirming(true)}
            data-testid={`client-revoke-${client.client_id}`}
          >
            Revoke
          </button>
        )}
      </td>
    </tr>
  )
}

// validTtl: a per-domain TTL is finite and non-negative (0 = unset → the finite global
// default; no indefinite). A blank field is 0 (use the default).
function parseTtl(v: string): number | null {
  if (v.trim() === '') return 0
  const n = Number(v)
  if (!Number.isInteger(n) || n < 0) return null
  return n
}

function AddDomainForm() {
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const [domain, setDomain] = useState('')
  const [accessTtl, setAccessTtl] = useState('')
  const [refreshTtl, setRefreshTtl] = useState('')
  const [consent, setConsent] = useState('')

  const access = parseTtl(accessTtl)
  const refresh = parseTtl(refreshTtl)
  const valid = domain.trim() !== '' && access !== null && refresh !== null

  const create = useMutation({
    mutationFn: () =>
      createDomain({
        domain: domain.trim(),
        accessTtlSeconds: access ?? 0,
        refreshTtlSeconds: refresh ?? 0,
        consent: consent === '' ? undefined : consent,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: OAUTH_DOMAINS_KEY })
      addToast({ level: 'warn', text: `Added trusted domain ${domain.trim()}.` })
      setDomain('')
      setAccessTtl('')
      setRefreshTtl('')
      setConsent('')
    },
    onError: (e) => {
      addToast({
        level: 'warn',
        text: e instanceof Error ? e.message : 'Failed to add domain.',
      })
    },
  })

  return (
    <form
      className={styles.domainForm}
      onSubmit={(e) => {
        e.preventDefault()
        if (valid) create.mutate()
      }}
    >
      <input
        className={styles.domainInput}
        placeholder="trusted domain (e.g. connector.example.com)"
        value={domain}
        onChange={(e) => setDomain(e.target.value)}
        data-testid="domain-add-identifier"
        aria-label="Domain identifier"
      />
      <input
        className={styles.ttlInput}
        placeholder="access TTL (s)"
        value={accessTtl}
        onChange={(e) => setAccessTtl(e.target.value)}
        data-testid="domain-add-access-ttl"
        aria-label="Access TTL seconds"
        aria-invalid={access === null}
      />
      <input
        className={styles.ttlInput}
        placeholder="refresh TTL (s)"
        value={refreshTtl}
        onChange={(e) => setRefreshTtl(e.target.value)}
        data-testid="domain-add-refresh-ttl"
        aria-label="Refresh TTL seconds"
        aria-invalid={refresh === null}
      />
      <input
        className={styles.consentInput}
        type="password"
        autoComplete="off"
        placeholder="consent (optional, write-only)"
        value={consent}
        onChange={(e) => setConsent(e.target.value)}
        data-testid="domain-add-consent"
        aria-label="Per-domain consent (write-only)"
      />
      <button
        type="submit"
        className={styles.issue}
        disabled={!valid || create.isPending}
        data-testid="domain-add-submit"
      >
        {create.isPending ? 'Adding…' : 'Add domain'}
      </button>
      {!valid && (domain !== '' || accessTtl !== '' || refreshTtl !== '') && (
        <span className={styles.invalid}>
          A domain is required; TTLs must be whole non-negative seconds (0 = default).
        </span>
      )}
    </form>
  )
}

function fmtTtl(seconds: number): string {
  if (seconds <= 0) return 'default'
  if (seconds % 86400 === 0) return `${seconds / 86400}d`
  if (seconds % 3600 === 0) return `${seconds / 3600}h`
  if (seconds % 60 === 0) return `${seconds / 60}m`
  return `${seconds}s`
}

function DomainCard({ domain, tokens }: { domain: DomainInfo; tokens: OAuthConnection[] }) {
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const [editing, setEditing] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const del = useMutation({
    mutationFn: () => deleteDomain(domain.id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: OAUTH_DOMAINS_KEY })
      void queryClient.invalidateQueries({ queryKey: OAUTH_CONNECTIONS_KEY })
      addToast({ level: 'warn', text: `Deleted ${domain.domain} (its tokens were revoked).` })
    },
    onError: (e) =>
      addToast({ level: 'warn', text: e instanceof Error ? e.message : 'Delete failed.' }),
  })

  return (
    <div className={styles.domainCard} data-testid={`domain-row-${domain.domain}`}>
      <div className={styles.domainHeader}>
        <span className={styles.domainName}>{domain.domain}</span>
        <span className={styles.domainTtl}>
          access {fmtTtl(domain.access_ttl_seconds)} · refresh {fmtTtl(domain.refresh_ttl_seconds)}
        </span>
        <span
          className={domain.consent_set ? styles.consentSet : styles.consentUnset}
          data-testid={`domain-consent-${domain.domain}`}
        >
          consent: {domain.consent_set ? 'Set' : 'Not set'}
        </span>
        <span className={styles.domainActions}>
          <button
            className={styles.cancel}
            onClick={() => setEditing((v) => !v)}
            data-testid={`domain-edit-${domain.domain}`}
          >
            {editing ? 'Close' : 'Edit'}
          </button>
          {confirmDelete ? (
            <>
              <button
                className={styles.danger}
                disabled={del.isPending}
                onClick={() => del.mutate()}
                data-testid={`domain-delete-confirm-${domain.domain}`}
              >
                {del.isPending ? 'Deleting…' : 'Confirm delete'}
              </button>
              <button className={styles.cancel} onClick={() => setConfirmDelete(false)}>
                Cancel
              </button>
            </>
          ) : (
            <button
              className={styles.revoke}
              onClick={() => setConfirmDelete(true)}
              data-testid={`domain-delete-${domain.domain}`}
            >
              Delete
            </button>
          )}
        </span>
      </div>

      {editing && <EditDomainForm domain={domain} onDone={() => setEditing(false)} />}

      <div className={styles.domainTokens} data-testid={`domain-tokens-${domain.domain}`}>
        {tokens.length === 0 ? (
          <p className={styles.hintSmall}>No active tokens for this domain.</p>
        ) : (
          <TokensTable tokens={tokens} />
        )}
      </div>
    </div>
  )
}

function EditDomainForm({ domain, onDone }: { domain: DomainInfo; onDone: () => void }) {
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const [accessTtl, setAccessTtl] = useState(String(domain.access_ttl_seconds || ''))
  const [refreshTtl, setRefreshTtl] = useState(String(domain.refresh_ttl_seconds || ''))
  const [consent, setConsent] = useState('')
  const [clear, setClear] = useState(false)

  const access = parseTtl(accessTtl)
  const refresh = parseTtl(refreshTtl)
  const valid = access !== null && refresh !== null

  const save = useMutation({
    mutationFn: () =>
      updateDomain({
        id: domain.id,
        accessTtlSeconds: access ?? 0,
        refreshTtlSeconds: refresh ?? 0,
        // write-only consent: cleared, set to a new value, or left unchanged.
        setConsent: clear ? '' : consent === '' ? undefined : consent,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: OAUTH_DOMAINS_KEY })
      addToast({ level: 'warn', text: `Updated ${domain.domain}.` })
      onDone()
    },
    onError: (e) =>
      addToast({ level: 'warn', text: e instanceof Error ? e.message : 'Update failed.' }),
  })

  return (
    <form
      className={styles.domainForm}
      onSubmit={(e) => {
        e.preventDefault()
        if (valid) save.mutate()
      }}
    >
      <input
        className={styles.ttlInput}
        value={accessTtl}
        placeholder="access TTL (s)"
        onChange={(e) => setAccessTtl(e.target.value)}
        data-testid={`domain-edit-access-ttl-${domain.domain}`}
        aria-label="Edit access TTL seconds"
        aria-invalid={access === null}
      />
      <input
        className={styles.ttlInput}
        value={refreshTtl}
        placeholder="refresh TTL (s)"
        onChange={(e) => setRefreshTtl(e.target.value)}
        data-testid={`domain-edit-refresh-ttl-${domain.domain}`}
        aria-label="Edit refresh TTL seconds"
        aria-invalid={refresh === null}
      />
      <input
        className={styles.consentInput}
        type="password"
        autoComplete="off"
        placeholder="new consent (write-only)"
        value={consent}
        disabled={clear}
        onChange={(e) => setConsent(e.target.value)}
        data-testid={`domain-edit-consent-${domain.domain}`}
        aria-label="Set per-domain consent (write-only)"
      />
      <label className={styles.clearConsent}>
        <input
          type="checkbox"
          checked={clear}
          onChange={(e) => setClear(e.target.checked)}
          data-testid={`domain-edit-clear-consent-${domain.domain}`}
        />
        clear consent
      </label>
      <button
        type="submit"
        className={styles.issue}
        disabled={!valid || save.isPending}
        data-testid={`domain-edit-save-${domain.domain}`}
      >
        {save.isPending ? 'Saving…' : 'Save'}
      </button>
    </form>
  )
}

function TokensTable({ tokens }: { tokens: OAuthConnection[] }) {
  return (
    <table className={styles.table}>
      <thead>
        <tr>
          <th>Client</th>
          <th>Principal</th>
          <th>Scope</th>
          <th>Issued</th>
          <th>Access expires</th>
          <th>Series</th>
          <th aria-label="Actions" />
        </tr>
      </thead>
      <tbody>
        {tokens.map((c) => (
          <ConnectionRow key={c.series_id} conn={c} />
        ))}
      </tbody>
    </table>
  )
}

function fmtTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

// Expiry status drives the row's attention colour (the 2026-06-15 admin-UI
// directive): surface problems, keep healthy rows quiet. Derived CLIENT-side from
// access_expiry so it stays live without a refetch. An unparseable time yields
// 'healthy' (never raise a false alarm on bad data).
const NEAR_EXPIRY_MS = 15 * 60 * 1000

type ExpiryStatus = 'expired' | 'near' | 'healthy'

export function expiryStatus(
  accessExpiry: string,
  now: number = Date.now(),
): ExpiryStatus {
  const exp = new Date(accessExpiry).getTime()
  if (Number.isNaN(exp)) return 'healthy'
  const left = exp - now
  if (left <= 0) return 'expired'
  if (left <= NEAR_EXPIRY_MS) return 'near'
  return 'healthy'
}

// fmtRelative renders a short, legible distance to/from the expiry instant:
// "expires in 8m" / "expired 2h ago". Returns '' for an unparseable time.
export function fmtRelative(accessExpiry: string, now: number = Date.now()): string {
  const exp = new Date(accessExpiry).getTime()
  if (Number.isNaN(exp)) return ''
  const diff = exp - now
  const abs = Math.abs(diff)
  let unit: string
  if (abs < 60_000) unit = '<1m'
  else if (abs < 3_600_000) unit = `${Math.round(abs / 60_000)}m`
  else if (abs < 86_400_000) unit = `${Math.round(abs / 3_600_000)}h`
  else unit = `${Math.round(abs / 86_400_000)}d`
  return diff < 0 ? `expired ${unit} ago` : `expires in ${unit}`
}

// fmtScope renders the token's authorization grant: "*" reads clearly as
// "all access"; a scoped value (e.g. "namespace:foo") is shown verbatim.
export function fmtScope(scope: string): string {
  return scope === '*' || scope === '' ? 'all access' : scope
}

// IssueTokenButton mints a "token to self" for the CLI (B-46b §2.2). The minted token is the one
// secret that crosses /ws/ui; on success it is handed up to the page (onIssued) for a display-once
// panel, never stored or toasted. B-71 Stage 4: the operator chooses a per-issuance FINITE expiry
// (GitHub-PAT-style, whole positive days — no indefinite option) via the SAME no-indefinite rule
// as confidential issuance (parseValidityDays); the chosen lifetime bounds the token.
function IssueTokenButton({
  disabled,
  onIssued,
}: {
  disabled: boolean
  onIssued: (t: OAuthIssueSelfPayload) => void
}) {
  const { add: addToast } = useToast()
  const [days, setDays] = useState('30')
  const validDays = parseValidityDays(days)
  const issue = useMutation({
    mutationFn: () => issueSelfToken((validDays ?? 0) * 86400),
    onSuccess: (t) => onIssued(t),
    onError: (e) =>
      addToast({
        level: 'warn',
        text:
          e instanceof OAuthDeniedError
            ? e.message
            : 'Failed to generate a CLI token.',
      }),
  })
  return (
    <span className={styles.selfIssue}>
      <input
        className={styles.ttlInput}
        value={days}
        onChange={(e) => setDays(e.target.value)}
        disabled={disabled}
        data-testid="self-issue-days"
        aria-label="CLI token validity in days"
        aria-invalid={validDays === null}
        title="Token validity in days (no indefinite)"
      />
      <button
        className={styles.issue}
        onClick={() => issue.mutate()}
        disabled={disabled || validDays === null || issue.isPending}
        aria-label="Generate a token for the CLI"
      >
        {issue.isPending ? 'Generating…' : 'Generate CLI token'}
      </button>
    </span>
  )
}

// IssuedTokenPanel shows the freshly minted token ONCE with a copy button and a
// warning that it will not be shown again.
function IssuedTokenPanel({
  token,
  onDismiss,
}: {
  token: OAuthIssueSelfPayload
  onDismiss: () => void
}) {
  const { add: addToast } = useToast()
  const [copied, setCopied] = useState(false)
  const copy = () => {
    void navigator.clipboard
      ?.writeText(token.access_token)
      .then(() => setCopied(true))
      .catch(() =>
        addToast({
          level: 'warn',
          text: 'Could not copy — select and copy manually.',
        }),
      )
  }
  return (
    <div className={styles.tokenPanel} role="status">
      <div className={styles.tokenWarn}>
        <strong>Copy this token now.</strong> It is shown once. Paste it into your
        CLI config with <code>shoka-cli auth</code>.
      </div>
      <div className={styles.tokenRow}>
        <code className={styles.tokenValue}>{token.access_token}</code>
        <button className={styles.copy} onClick={copy} aria-label="Copy token">
          {copied ? 'Copied' : 'Copy'}
        </button>
        <button
          className={styles.dismiss}
          onClick={onDismiss}
          aria-label="Dismiss token"
        >
          Done
        </button>
      </div>
      <div className={styles.tokenExpiry}>Expires {fmtTime(token.access_expiry)}</div>
    </div>
  )
}

function ConnectionRow({ conn }: { conn: OAuthConnection }) {
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const [confirming, setConfirming] = useState(false)

  const revoke = useMutation({
    mutationFn: () => revokeConnection(conn.series_id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: OAUTH_CONNECTIONS_KEY })
      addToast({
        level: 'warn',
        text: `Revoked the connection for ${clientDomain(conn.client_id)}.`,
      })
    },
    onError: (e) => {
      const text =
        e instanceof OAuthDeniedError
          ? e.message
          : 'Failed to revoke the connection.'
      addToast({ level: 'warn', text })
      setConfirming(false)
    },
  })

  const busy = revoke.isPending
  const status = expiryStatus(conn.access_expiry)

  return (
    <tr data-testid={`token-row-${conn.series_id_short}`}>
      <td className={styles.client} title={conn.client_id}>
        {clientDomain(conn.client_id)}
      </td>
      <td className={styles.principal}>
        <span className={styles.principalName}>
          {conn.principal_name || '—'}
        </span>
        {conn.principal_email && (
          <span className={styles.principalEmail}>{conn.principal_email}</span>
        )}
      </td>
      <td className={styles.scopeCell} title={conn.scope}>
        {fmtScope(conn.scope)}
      </td>
      <td className={styles.time}>{fmtTime(conn.issued_at)}</td>
      <td className={styles.time} data-status={status}>
        <div className={styles.expiryCell}>
          <span>{fmtTime(conn.access_expiry)}</span>
          {status !== 'healthy' && (
            <span
              className={
                status === 'expired' ? styles.badgeExpired : styles.badgeNear
              }
            >
              {fmtRelative(conn.access_expiry)}
            </span>
          )}
        </div>
      </td>
      <td className={styles.series}>
        <code>{conn.series_id_short}</code>
      </td>
      <td className={styles.actions}>
        {confirming ? (
          <>
            <button
              className={styles.danger}
              disabled={busy}
              onClick={() => revoke.mutate()}
            >
              {busy ? 'Revoking…' : 'Confirm revoke'}
            </button>
            <button
              className={styles.cancel}
              disabled={busy}
              onClick={() => setConfirming(false)}
            >
              Cancel
            </button>
          </>
        ) : (
          <button
            className={styles.revoke}
            disabled={busy}
            onClick={() => setConfirming(true)}
          >
            Revoke
          </button>
        )}
      </td>
    </tr>
  )
}
