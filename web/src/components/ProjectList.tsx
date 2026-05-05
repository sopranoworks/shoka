import React, { useState, useEffect } from 'react';
import { Book, Plus, Search } from 'lucide-react';
import { useWebSocket } from './WebSocketContext';
import './ProjectList.css';

interface Project {
  name: string;
  namespace: string;
}

interface ProjectListProps {
  onSelectProject: (namespace: string, name: string) => void;
}

const ProjectList: React.FC<ProjectListProps> = ({ onSelectProject }) => {
  const [projects, setProjects] = useState<Project[]>([]);
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(true);
  const { socket, connected } = useWebSocket();

  useEffect(() => {
    if (connected && socket) {
      const handleMessage = (event: MessageEvent) => {
        const msg = JSON.parse(event.data);
        if (msg.type === 'GET_PROJECTS') {
          const projectNames = msg.payload as string[];
          setProjects(projectNames.map(name => ({ name, namespace: 'default' })));
          setLoading(false);
        }
      };

      socket.addEventListener('message', handleMessage);

      socket.send(JSON.stringify({
        type: 'GET_PROJECTS',
        payload: { namespace: 'default' }
      }));

      return () => {
        socket.removeEventListener('message', handleMessage);
      };
    }
  }, [connected, socket]);

  const handleCreateProject = () => {
    const name = prompt('Enter project name:');
    if (name && socket) {
      socket.send(JSON.stringify({
        type: 'CREATE_PROJECT',
        payload: { namespace: 'default', projectName: name }
      }));
      // Refresh list
      socket.send(JSON.stringify({
        type: 'GET_PROJECTS',
        payload: { namespace: 'default' }
      }));
    }
  };

  const filteredProjects = projects.filter(p => 
    p.name.toLowerCase().includes(search.toLowerCase())
  );

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
        <Search 
          size={16} 
          style={{ position: 'absolute', left: '12px', top: '50%', transform: 'translateY(-50%)', color: '#636c76' }} 
        />
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
              <div 
                key={`${project.namespace}/${project.name}`} 
                className="project-item"
                onClick={() => onSelectProject(project.namespace, project.name)}
              >
                <Book size={16} className="project-icon" />
                <span className="project-namespace">{project.namespace} /</span>
                <span className="project-name">{project.name}</span>
              </div>
            ))
          ) : (
            <div className="project-item">No projects found.</div>
          )}
        </div>
      )}
    </div>
  );
};

export default ProjectList;
