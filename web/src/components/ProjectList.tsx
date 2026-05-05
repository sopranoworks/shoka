import React, { useState, useEffect, useRef } from 'react';
import { Book, Plus } from 'lucide-react';
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
  const socketRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const host = window.location.host;
    const socket = new WebSocket(`${protocol}//${host}/ws/ui`);
    socketRef.current = socket;

    socket.onopen = () => {
      console.log('Connected to WebSocket');
      socket.send(JSON.stringify({
        type: 'GET_PROJECTS',
        payload: { namespace: 'default' }
      }));
    };

    socket.onmessage = (event) => {
      const msg = JSON.parse(event.data);
      if (msg.type === 'GET_PROJECTS') {
        const projectNames = msg.payload as string[];
        setProjects(projectNames.map(name => ({ name, namespace: 'default' })));
        setLoading(false);
      }
    };

    socket.onerror = (error) => {
      console.error('WebSocket error:', error);
      setLoading(false);
    };

    return () => {
      socket.close();
    };
  }, []);

  const handleCreateProject = () => {
    const name = prompt('Enter project name:');
    if (name && socketRef.current) {
      socketRef.current.send(JSON.stringify({
        type: 'CREATE_PROJECT',
        payload: { namespace: 'default', projectName: name }
      }));
      // Refresh list
      socketRef.current.send(JSON.stringify({
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

      <div className="search-container">
        <input 
          type="text" 
          className="search-input" 
          placeholder="Find a repository..." 
          value={search}
          onChange={(e) => setSearch(e.target.value)}
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
