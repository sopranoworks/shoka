import { useState } from 'react'
import {
  registerFirstAdmin,
  registerPasskey,
  isPasskeyCapable,
  newTOTP,
  type NewTOTP,
} from '../lib/authClient'
import styles from './AuthScreen.module.css'

// FirstRunWizard is the zero-config first-run setup (B-28 stage 1): with no users
// yet, the first person to complete it becomes the wildcard admin. Password is the
// required floor; TOTP is an optional companion enrolled here; a passkey can be
// added immediately after when the origin is secure-context capable.
export function FirstRunWizard({
  passkeyEnabled,
  onDone,
}: {
  passkeyEnabled: boolean
  onDone: () => void
}) {
  const [email, setEmail] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [useTOTP, setUseTOTP] = useState(false)
  const [totp, setTOTP] = useState<NewTOTP | null>(null)
  const [totpCode, setTOTPCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const canPasskey = passkeyEnabled && isPasskeyCapable()

  async function toggleTOTP(on: boolean) {
    setUseTOTP(on)
    setError(null)
    if (on && !totp) {
      try {
        setTOTP(await newTOTP(email || 'admin'))
      } catch (e) {
        setError(errMsg(e))
        setUseTOTP(false)
      }
    }
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)
    if (password.length < 8) return setError('Password must be at least 8 characters.')
    if (password !== confirm) return setError('Passwords do not match.')
    if (useTOTP && (!totp || totpCode.trim() === '')) {
      return setError('Enter the 6-digit code from your authenticator to confirm TOTP.')
    }
    setBusy(true)
    try {
      await registerFirstAdmin({
        email: email.trim(),
        display_name: displayName.trim() || email.trim(),
        password,
        totp_secret: useTOTP && totp ? totp.secret : undefined,
        totp_code: useTOTP ? totpCode.trim() : undefined,
      })
      // Account created and session established. Offer a passkey where possible,
      // then enter the app. A passkey failure is non-fatal (the floor still works).
      if (canPasskey && window.confirm('Set up a passkey for faster, passwordless sign-in?')) {
        try {
          await registerPasskey()
        } catch {
          /* non-fatal: password+TOTP remains usable */
        }
      }
      onDone()
    } catch (e) {
      setError(errMsg(e))
      setBusy(false)
    }
  }

  return (
    <div className={styles.backdrop}>
      <form className={styles.card} onSubmit={submit} aria-label="First-run setup">
        <h1 className={styles.title}>Welcome to Shoka</h1>
        <p className={styles.subtitle}>
          No users exist yet. Create the first administrator account — this account
          becomes the owner of this Shoka instance.
        </p>
        {error && <div className={styles.error}>{error}</div>}
        <div className={styles.field}>
          <label htmlFor="fr-email">Email (your commit author identity)</label>
          <input
            id="fr-email"
            type="email"
            autoComplete="username"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
          />
        </div>
        <div className={styles.field}>
          <label htmlFor="fr-name">Display name</label>
          <input
            id="fr-name"
            type="text"
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
          />
        </div>
        <div className={styles.field}>
          <label htmlFor="fr-pw">Password</label>
          <input
            id="fr-pw"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </div>
        <div className={styles.field}>
          <label htmlFor="fr-pw2">Confirm password</label>
          <input
            id="fr-pw2"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            required
          />
        </div>
        <div className={styles.toggleRow}>
          <input
            id="fr-totp"
            type="checkbox"
            checked={useTOTP}
            onChange={(e) => toggleTOTP(e.target.checked)}
          />
          <label htmlFor="fr-totp">Enable two-factor authentication (TOTP)</label>
        </div>
        {useTOTP && totp && (
          <div className={styles.totpBox}>
            <div>Add this secret to your authenticator app, then enter a code:</div>
            <div className={styles.totpSecret}>{totp.secret}</div>
            <div className={styles.field}>
              <label htmlFor="fr-code">6-digit code</label>
              <input
                id="fr-code"
                inputMode="numeric"
                value={totpCode}
                onChange={(e) => setTOTPCode(e.target.value)}
              />
            </div>
          </div>
        )}
        <button className={styles.button} type="submit" disabled={busy}>
          {busy ? 'Creating…' : 'Create administrator'}
        </button>
      </form>
    </div>
  )
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : 'Something went wrong.'
}
