import React, { useState } from 'react';
import ProjectList from './components/ProjectList';

type View = 'list' | 'project';

interface SelectedProject {
  namespace: string;
  name: string;
}

function App() {
  const [view, setView] = useState<View>('list');
  const [selectedProject, setSelectedProject] = useState<SelectedProject | null>(null);

  const handleSelectProject = (namespace: string, name: string) => {
    setSelectedProject({ namespace, name });
    setView('project');
  };

  return (
    <div className="app-container">
      {view === 'list' ? (
        <ProjectList onSelectProject={handleSelectProject} />
      ) : (
        <div style={{ padding: '24px', maxWidth: '1012px', margin: '0 auto', fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif' }}>
          <div style={{ marginBottom: '16px', borderBottom: '1px solid #d0d7de', paddingBottom: '16px', display: 'flex', alignItems: 'center', gap: '12px' }}>
            <button 
              onClick={() => setView('list')}
              style={{ 
                backgroundColor: '#f6f8fa', 
                border: '1px solid #d0d7de', 
                borderRadius: '6px', 
                padding: '5px 12px', 
                fontSize: '14px', 
                fontWeight: 600, 
                cursor: 'pointer' 
              }}
            >
              Back to list
            </button>
            <h2 style={{ margin: 0, fontSize: '20px' }}>
              {selectedProject?.namespace} / {selectedProject?.name}
            </h2>
          </div>
          <div style={{ padding: '40px', textAlign: 'center', border: '1px dashed #d0d7de', borderRadius: '6px', color: '#636c76' }}>
            <p>Project view coming soon (Task 4)...</p>
          </div>
        </div>
      )}
    </div>
  );
}

export default App;
