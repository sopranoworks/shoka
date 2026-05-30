import React, { useState, useEffect } from 'react';
import { Book, Plus, Search, CheckCircle, AlertTriangle, XCircle, RefreshCw } from 'lucide-react';
import { useWebSocket } from './WebSocketContext';
import './ProjectList.css';

type ProjectState = 'healthy' | 'corrupted' | 'dangerous';

interface Project {
  name: string;
  namespace: string;
  state: ProjectState;
}

interface ProjectInfo {
  name: string;
  state: ProjectState;
}

interface DriftSummary {
  state: ProjectState;
  added: string[] | null;
  modified: string[] | null;
  deleted: string[] | null;
}

interface ProjectListProps {
  onSelectProject: (namespace: string, name: string) => void;
}

const NAMESPACE = 'default';

function stateBadge(state: ProjectState) {
  switch (state) {
    case 'corrupted':
      return <span title="corrupted: working tree drifted from git" style={{ color: '#bf8700', display: 'inline-flex' }}><AlertTriangle size={16} /></span>;
    case 'dangerous':
      return <span title="dangerous: git repository unreadable" style={{ color: '#cf222e', display: 'inline-flex' }}><XCircle size={16} /></span>;
    default:
      return <span title="healthy" style={{ color: '#1a7f37', display: 'inline-flex' }}><CheckCircle size={16} /></span>;
  }
}

const ProjectList: React.FC<ProjectListProps> = ({ onSelectProject }) => {
  const [projects, setProjects] = useState<Project[]>([]);
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(true);
  const [recovering, setRecovering] = useState<Project | null>(null);
  const { socket, connected } = useWebSocket();

  const refresh = () => {
    if (socket && connected) {
      socket.send(JSON.stringify({ type: 'GET_PROJECTS', payload: { namespace: NAMESPACE } }));
    }
  };

  useEffect(() => {
    if (connected && socket) {
      const handleMessage = (event: MessageEvent) => {
        const msg = JSON.parse(event.data);
        if (msg.type === 'GET_PROJECTS') {
          const infos = (msg.payload as ProjectInfo[]) || [];
          setProjects(infos.map(p => ({ name: p.name, namespace: NAMESPACE, state: p.state || 'healthy' })));
          setLoading(false);
        }
      };
      socket.addEventListener('message', handleMessage);
      refresh();
      return () => socket.removeEventListener('message', handleMessage);
    }
  }, [connected, socket]);

  const handleCreateProject = () => {
    const name = prompt('Enter project name:');
    if (name && socket) {
      socket.send(JSON.stringify({ type: 'CREATE_PROJECT', payload: { namespace: NAMESPACE, projectName: name } }));
      refresh();
    }
  };

  // Re-scan triggers on-demand drift detection on the server, then refreshes.
  const rescan = async (p: Project) => {
    await fetch(`/api/project/rescan?namespace=${encodeURIComponent(p.namespace)}&project=${encodeURIComponent(p.name)}`, { method: 'POST' });
    refresh();
  };

  const onProjectClick = (p: Project) => {
    if (p.state === 'healthy') {
      onSelectProject(p.namespace, p.name);
    } else {
      setRecovering(p);
    }
  };

  const filteredProjects = projects.filter(p => p.name.toLowerCase().includes(search.toLowerCase()));

  return (
    <div className="project-list-container">
      <div className="project-list-header">
        <h2>Repositories</h2>
        <button className="new-project-btn" onClick={handleCreateProject}>
          <Plus size={16} />
          New
        </button>
      </div>

      <div className="search-container" style={{ position: 'relative' }}>
        <Search size={16} style={{ position: 'absolute', left: '12px', top: '50%', transform: 'translateY(-50%)', color: '#636c76' }} />
        <input
          type="text"
          className="search-input"
          placeholder="Find a repository..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          style={{ paddingLeft: '36px' }}
        />
      </div>

      {loading ? (
        <p>Loading projects...</p>
      ) : (
        <div className="project-items">
          {filteredProjects.length > 0 ? (
            filteredProjects.map((project) => (
              <div key={`${project.namespace}/${project.name}`} className="project-item" style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                <Book size={16} className="project-icon" />
                <span className="project-namespace">{project.namespace} /</span>
                <span className="project-name" style={{ cursor: 'pointer', flex: 1 }} onClick={() => onProjectClick(project)}>{project.name}</span>
                {stateBadge(project.state)}
                <button title="Re-scan" onClick={(e) => { e.stopPropagation(); rescan(project); }} style={{ background: 'none', border: 'none', cursor: 'pointer', color: '#636c76', display: 'inline-flex' }}>
                  <RefreshCw size={14} />
                </button>
              </div>
            ))
          ) : (
            <div className="project-item">No projects found.</div>
          )}
        </div>
      )}

      {recovering && (
        <RecoveryDialog
          project={recovering}
          onClose={() => setRecovering(null)}
          onRecovered={() => { setRecovering(null); refresh(); }}
        />
      )}
    </div>
  );
};

const RecoveryDialog: React.FC<{ project: Project; onClose: () => void; onRecovered: () => void }> = ({ project, onClose, onRecovered }) => {
  const [drift, setDrift] = useState<DriftSummary | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    // Fetch a fresh drift summary on open.
    fetch(`/api/project/rescan?namespace=${encodeURIComponent(project.namespace)}&project=${encodeURIComponent(project.name)}`, { method: 'POST' })
      .then(r => r.json())
      .then((d: DriftSummary) => setDrift(d))
      .catch(() => setError('Failed to load drift summary'));
  }, [project]);

  const recover = async (mode: 'accept-working-tree' | 'accept-head') => {
    if (mode === 'accept-head' && !window.confirm('This will discard working-tree changes. Continue?')) return;
    setBusy(true);
    setError(null);
    const resp = await fetch(`/api/project/recover?namespace=${encodeURIComponent(project.namespace)}&project=${encodeURIComponent(project.name)}&mode=${mode}`, { method: 'POST' });
    setBusy(false);
    if (resp.ok) {
      onRecovered();
    } else {
      const body = await resp.json().catch(() => ({}));
      setError(body.error || `Recovery failed (${resp.status})`);
    }
  };

  const list = (label: string, items: string[] | null) =>
    items && items.length > 0 ? <div><strong>{label}:</strong> {items.join(', ')}</div> : null;

  const state = drift?.state || project.state;

  return (
    <div style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.4)', display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000 }} onClick={onClose}>
      <div style={{ background: '#fff', borderRadius: 8, padding: 24, maxWidth: 520, width: '90%' }} onClick={(e) => e.stopPropagation()}>
        <h3 style={{ marginTop: 0 }}>Recover {project.namespace}/{project.name}</h3>
        <p>State: <strong>{state}</strong></p>
        {drift ? (
          <div style={{ fontSize: 13, color: '#57606a', marginBottom: 16 }}>
            {list('Modified', drift.modified)}
            {list('Added (untracked)', drift.added)}
            {list('Deleted', drift.deleted)}
            {!drift.added?.length && !drift.modified?.length && !drift.deleted?.length && state === 'dangerous' && <div>The git repository is unreadable or absent.</div>}
          </div>
        ) : <p>Loading drift summary…</p>}
        {error && <p style={{ color: '#cf222e' }}>{error}</p>}
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button onClick={onClose} disabled={busy}>Cancel</button>
          <button onClick={() => recover('accept-head')} disabled={busy || state === 'dangerous'} title={state === 'dangerous' ? 'Not available for a dangerous project' : ''}>
            Git HEAD is correct
          </button>
          <button onClick={() => recover('accept-working-tree')} disabled={busy} style={{ fontWeight: 600 }}>
            Working tree is correct
          </button>
        </div>
      </div>
    </div>
  );
};

export default ProjectList;
