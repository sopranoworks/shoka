import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useIsAdmin } from '../lib/admin'
import { useConnectionsQuery, OAUTH_CONNECTIONS_KEY } from '../lib/queries'
import {
  revokeConnection,
  issueSelfToken,
  clientDomain,
  OAuthDeniedError,
} from '../lib/oauthOps'
import { useToast } from '../lib/toast'
import type { OAuthConnection, OAuthIssueSelfPayload } from '../lib/types'
import styles from './ConnectionsPage.module.css'

// The administrator-only OAuth connection management view (the 2026-06-03 MCP
// OAuth (c) directive §2.2). It lists the live MCP connections the built-in
// authorization server holds — each shown by the connecting client's own
// identity (its CIMD domain) and the principal it acts for, never by any secret —
// and revokes any one with a confirm-gated, per-row action that cuts exactly that
// connection and leaves the rest. The server-side gate is authoritative; the
// useIsAdmin() check here only governs UI EXPOSURE (secondary). There is no oauth
// NOTIFY, so the list refreshes manually (invalidate-on-revoke + a Refresh
// button), not live.
export function ConnectionsPage() {
  const isAdmin = useIsAdmin()
  const [issued, setIssued] = useState<OAuthIssueSelfPayload | null>(null)
  const {
    data: connections,
    isLoading,
    isFetching,
    error,
    refetch,
  } = useConnectionsQuery(isAdmin)

  // Secondary UI gate. The authoritative refusal is server-side, but a non-admin
  // should not even see the surface.
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
          <span className={styles.sub}> · active MCP authorizations</span>
        </span>
        <IssueTokenButton disabled={oauthDisabled} onIssued={setIssued} />
        <button
          className={styles.refresh}
          onClick={() => void refetch()}
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
        {isLoading ? (
          <p className={styles.hint}>Loading connections…</p>
        ) : oauthDisabled ? (
          <p className={styles.hint}>
            OAuth is not enabled on this server, so there are no connections to
            manage.
          </p>
        ) : error ? (
          <p className={styles.error}>Failed to load connections. Try Refresh.</p>
        ) : !connections || connections.length === 0 ? (
          <p className={styles.hint}>No active OAuth connections.</p>
        ) : (
          <>
            <div className={styles.count}>
              {connections.length}{' '}
              {connections.length === 1 ? 'connection' : 'connections'}
            </div>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Client</th>
                  <th>Principal</th>
                  <th>Issued</th>
                  <th>Access expires</th>
                  <th>Series</th>
                  <th aria-label="Actions" />
                </tr>
              </thead>
              <tbody>
                {connections.map((c) => (
                  <ConnectionRow key={c.series_id} conn={c} />
                ))}
              </tbody>
            </table>
          </>
        )}
      </div>
    </div>
  )
}

function fmtTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

// IssueTokenButton mints a "token to self" for the CLI (B-46b §2.2). The minted
// token is the one secret that crosses /ws/ui; on success it is handed up to the
// page (onIssued) for a display-once panel, never stored or toasted.
function IssueTokenButton({
  disabled,
  onIssued,
}: {
  disabled: boolean
  onIssued: (t: OAuthIssueSelfPayload) => void
}) {
  const { add: addToast } = useToast()
  const issue = useMutation({
    mutationFn: () => issueSelfToken(),
    onSuccess: (t) => onIssued(t),
    onError: (e) => {
      const text =
        e instanceof OAuthDeniedError
          ? e.message
          : 'Failed to generate a CLI token.'
      addToast({ level: 'warn', text })
    },
  })
  return (
    <button
      className={styles.issue}
      onClick={() => issue.mutate()}
      disabled={disabled || issue.isPending}
      aria-label="Generate a token for the CLI"
    >
      {issue.isPending ? 'Generating…' : 'Generate CLI token'}
    </button>
  )
}

// IssuedTokenPanel shows the freshly minted token ONCE with a copy button and a
// warning that it will not be shown again. The operator pastes it into their CLI
// client config (`shoka-cli auth`). Dismissing clears it from the page state.
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

  return (
    <tr>
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
      <td className={styles.time}>{fmtTime(conn.issued_at)}</td>
      <td className={styles.time}>{fmtTime(conn.access_expiry)}</td>
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
