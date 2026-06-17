import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useIsSuperUser } from '../lib/authStatus'
import { useToast } from '../lib/toast'
import { PromptDialog } from '../components/PromptDialog'
import { TypeToConfirmDialog } from '../components/TypeToConfirmDialog'
import { MoveProjectDialog } from '../components/MoveProjectDialog'
import {
  namespaceHealth,
  createNamespace,
  deleteNamespace,
  createProject,
  deleteProject,
  moveProject,
  namespaceRecover,
  type NamespaceHealth,
  type ProjectHealth,
} from '../lib/nsManageOps'
import styles from './NamespaceManagementPage.module.css'

const NAME_RE = /^[A-Za-z0-9_-]+$/
function validateName(v: string): string | null {
  return NAME_RE.test(v) ? null : 'Only letters, digits, hyphen, and underscore are allowed.'
}
function msg(e: unknown): string {
  return e instanceof Error ? e.message : 'Something went wrong.'
}

// AddTarget: the name-entry dialog is either adding a namespace (super-user) or a project
// to a namespace (admin-on-ns).
type AddTarget = { kind: 'namespace' } | { kind: 'project'; namespace: string } | null
// DelTarget: the type-the-name confirm dialog is deleting a project or a namespace.
type DelTarget =
  | { kind: 'project'; namespace: string; name: string }
  | { kind: 'namespace'; name: string }
  | null

// Namespace / project management (B-28 part 2), the third Settings item (visible to a
// super-user OR any namespace-admin). It renders the stage-B admin-filtered
// namespace→projects listing WITH per-entry HEALTH, capability-gated ADD (namespace add =
// super-user; project add = admin-on-ns), and DELETE that is deliberate, sequential, and
// high-friction (type-the-exact-name-then-confirm; a namespace deletes only once empty).
export function NamespaceManagementPage() {
  const qc = useQueryClient()
  const { add: toast } = useToast()
  const isSuperUser = useIsSuperUser()
  const health = useQuery({ queryKey: ['namespace-health'], queryFn: namespaceHealth })

  const [add, setAdd] = useState<AddTarget>(null)
  const [del, setDel] = useState<DelTarget>(null)
  // The project being moved (B-28), or null. Move is super-user only and DISTINCT from delete.
  const [move, setMove] = useState<{ namespace: string; project: string } | null>(null)

  function refresh() {
    void qc.invalidateQueries({ queryKey: ['namespace-health'] })
    void qc.invalidateQueries({ queryKey: ['projects'] })
  }
  async function run(p: Promise<unknown>, ok: string) {
    try {
      await p
      toast({ level: 'warn', text: ok })
      refresh()
    } catch (e) {
      toast({ level: 'warn', text: msg(e) })
    }
  }

  function onConfirmDelete() {
    const d = del
    setDel(null)
    if (!d) return
    if (d.kind === 'project') {
      void run(deleteProject(d.namespace, d.name), `Deleted project ${d.namespace}/${d.name}.`)
    } else {
      void run(deleteNamespace(d.name), `Deleted namespace ${d.name}.`)
    }
  }
  function onConfirmAdd(name: string) {
    const a = add
    setAdd(null)
    if (!a) return
    if (a.kind === 'namespace') {
      void run(createNamespace(name), `Created namespace ${name}.`)
    } else {
      void run(createProject(a.namespace, name), `Created project ${a.namespace}/${name}.`)
    }
  }
  function onConfirmMove(target: string) {
    const m = move
    setMove(null)
    if (!m || !target) return
    void run(moveProject(m.namespace, m.project, target), `Moved ${m.namespace}/${m.project} → ${target}/${m.project}.`)
  }

  const allNamespaces = (health.data?.namespaces ?? []).map((n) => n.name)

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <h1 className={styles.title}>Namespace / project management</h1>
        {isSuperUser && (
          <button className={`${styles.btn} ${styles.primary}`} onClick={() => setAdd({ kind: 'namespace' })}>
            + New namespace
          </button>
        )}
      </div>

      {health.isError ? (
        <p className={styles.err}>{msg(health.error)}</p>
      ) : !health.data ? (
        <p className={styles.muted}>Loading…</p>
      ) : (health.data.namespaces ?? []).length === 0 ? (
        <p className={styles.muted}>No namespaces you manage yet.</p>
      ) : (
        (health.data.namespaces ?? []).map((nh) => (
          <NamespaceBlock
            key={nh.name}
            nh={nh}
            isSuperUser={isSuperUser}
            onAddProject={() => setAdd({ kind: 'project', namespace: nh.name })}
            onMoveProject={(name) => setMove({ namespace: nh.name, project: name })}
            onDeleteProject={(name) => setDel({ kind: 'project', namespace: nh.name, name })}
            onDeleteNamespace={() => setDel({ kind: 'namespace', name: nh.name })}
            onDropMissing={(proj) =>
              run(namespaceRecover('drop_missing', nh.name, proj), `Dropped missing record ${nh.name}/${proj}.`)
            }
            onCleanOrphan={(name) =>
              run(namespaceRecover('clean_orphaned', nh.name, name), `Cleaned orphaned ${nh.name}/${name}.`)
            }
            onAdoptProject={(name) =>
              run(namespaceRecover('adopt', nh.name, name), `Adopted ${nh.name}/${name}.`)
            }
          />
        ))
      )}

      {isSuperUser && (health.data?.foreign_namespaces?.length ?? 0) > 0 && (
        <section className={styles.foreignSection}>
          <h2 className={styles.h2}>Untracked directories (not managed)</h2>
          <p className={styles.muted}>
            Directories under the base path that Shoka does not manage. A valid one can be adopted.
          </p>
          {health.data!.foreign_namespaces!.map((f) => (
            <div key={f.name} className={styles.foreignRow}>
              <span className={styles.mono}>{f.name}</span>
              {f.adoptable ? (
                <button
                  className={styles.btn}
                  onClick={() => run(namespaceRecover('adopt', f.name, ''), `Adopted namespace ${f.name}.`)}
                >
                  Adopt namespace
                </button>
              ) : (
                <span className={styles.muted}>not a project</span>
              )}
            </div>
          ))}
        </section>
      )}

      <PromptDialog
        open={add !== null}
        title={add?.kind === 'project' ? `New project in ${add.namespace}` : 'New namespace'}
        label={add?.kind === 'project' ? 'Project name' : 'Namespace name'}
        confirmLabel="Create"
        validate={validateName}
        onConfirm={onConfirmAdd}
        onCancel={() => setAdd(null)}
      />

      <MoveProjectDialog
        open={move !== null}
        sourceNamespace={move?.namespace ?? ''}
        project={move?.project ?? ''}
        targets={allNamespaces.filter((n) => n !== move?.namespace)}
        onConfirm={onConfirmMove}
        onCancel={() => setMove(null)}
      />

      <TypeToConfirmDialog
        open={del !== null}
        title={del?.kind === 'namespace' ? `Delete namespace ${del.name}` : `Delete project ${del?.name ?? ''}`}
        message={
          del?.kind === 'namespace'
            ? `This permanently deletes the empty namespace "${del.name}". This cannot be undone.`
            : `This permanently deletes the project "${del?.name ?? ''}" and all its history. This cannot be undone.`
        }
        expected={del?.name ?? ''}
        confirmLabel="Delete"
        onConfirm={onConfirmDelete}
        onCancel={() => setDel(null)}
      />
    </div>
  )
}

function healthBadge(state: string): { label: string; cls: string } {
  switch (state) {
    case 'healthy':
      return { label: 'healthy', cls: styles.badgeHealthy }
    case 'corrupted':
      return { label: 'corrupted', cls: styles.badgeBad }
    case 'dangerous':
      return { label: 'dangerous', cls: styles.badgeBad }
    case 'missing':
      return { label: 'missing', cls: styles.badgeBad }
    default:
      return { label: state, cls: styles.badge }
  }
}

function NamespaceBlock({
  nh,
  isSuperUser,
  onAddProject,
  onMoveProject,
  onDeleteProject,
  onDeleteNamespace,
  onDropMissing,
  onCleanOrphan,
  onAdoptProject,
}: {
  nh: NamespaceHealth
  isSuperUser: boolean
  onAddProject: () => void
  onMoveProject: (name: string) => void
  onDeleteProject: (name: string) => void
  onDeleteNamespace: () => void
  onDropMissing: (proj: string) => void
  onCleanOrphan: (name: string) => void
  onAdoptProject: (name: string) => void
}) {
  // Go marshals an empty namespace's project slice as JSON null — tolerate it.
  const projects = nh.projects ?? []
  const hasProjects = projects.length > 0
  return (
    <section className={styles.nsBlock} data-testid={`ns-${nh.name}`}>
      <div className={styles.nsHeader}>
        <span className={styles.nsName}>{nh.name}</span>
        {!nh.healthy && <span className={styles.badgeBad}>needs attention</span>}
        <span className={styles.spacer} />
        <button className={styles.btn} onClick={onAddProject}>
          + Add project
        </button>
        {isSuperUser && (
          <button
            className={`${styles.btn} ${styles.danger}`}
            disabled={hasProjects}
            title={hasProjects ? 'Delete its projects first (one at a time)' : ''}
            onClick={onDeleteNamespace}
          >
            Delete namespace
          </button>
        )}
      </div>

      {projects.length === 0 ? (
        <p className={styles.muted}>No projects.</p>
      ) : (
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Project</th>
              <th>Health</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {projects.map((p) => (
              <ProjectRow
                key={p.name}
                p={p}
                namespace={nh.name}
                isSuperUser={isSuperUser}
                onMove={() => onMoveProject(p.name)}
                onDelete={() => onDeleteProject(p.name)}
                onDropMissing={() => onDropMissing(p.name)}
              />
            ))}
          </tbody>
        </table>
      )}

      {(nh.orphaned?.length ?? 0) > 0 && (
        <div className={styles.recoverRow}>
          <span className={styles.muted}>Orphaned data (no project):</span>
          {nh.orphaned!.map((o) => (
            <button key={o.name} className={styles.btn} onClick={() => onCleanOrphan(o.name)}>
              Clean {o.name}
            </button>
          ))}
        </div>
      )}

      {(nh.foreign?.length ?? 0) > 0 && (
        <div className={styles.recoverRow}>
          <span className={styles.muted}>Untracked here:</span>
          {nh.foreign!.map((f) => (
            <span key={f.name} className={styles.foreignInline}>
              <span className={styles.mono}>{f.name}</span>
              {f.adoptable && (
                <button className={styles.btn} onClick={() => onAdoptProject(f.name)}>
                  Adopt
                </button>
              )}
            </span>
          ))}
        </div>
      )}
    </section>
  )
}

function ProjectRow({
  p,
  namespace,
  isSuperUser,
  onMove,
  onDelete,
  onDropMissing,
}: {
  p: ProjectHealth
  namespace: string
  isSuperUser: boolean
  onMove: () => void
  onDelete: () => void
  onDropMissing: () => void
}) {
  const badge = healthBadge(p.state)
  const missing = p.state === 'missing'
  const present = !missing
  return (
    <tr data-testid={`proj-${namespace}-${p.name}`}>
      <td className={styles.mono}>{p.name}</td>
      <td>
        <span className={badge.cls}>{badge.label}</span>
      </td>
      <td className={styles.rowActions}>
        {/* The move action lives in its own dedicated slot, kept VISUALLY DISTINCT from
            Delete (separate placement, non-danger styling, its own pick-target flow) so it
            can never be confused with delete. Super-user only (project move is super-user). */}
        <span className={styles.moveSlot} data-testid={`move-slot-${namespace}-${p.name}`}>
          {isSuperUser && present && (
            <button className={styles.btn} onClick={onMove}>
              Move…
            </button>
          )}
        </span>
        {missing ? (
          <button className={styles.btn} onClick={onDropMissing}>
            Drop record
          </button>
        ) : (
          <button className={`${styles.btn} ${styles.danger}`} onClick={onDelete}>
            Delete
          </button>
        )}
      </td>
    </tr>
  )
}
