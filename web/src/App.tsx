import { useState } from 'react';
import ProjectList from './components/ProjectList';
import FileTree from './components/FileTree';
import { WebSocketProvider } from './components/WebSocketContext';

type View = 'list' | 'project';

interface SelectedProject {
  namespace: string;
  name: string;
}

function AppContent() {
  const [view, setView] = useState<View>('list');
  const [selectedProject, setSelectedProject] = useState<SelectedProject | null>(null);
  const [selectedFile, setSelectedFile] = useState<string | undefined>();

  const handleSelectProject = (namespace: string, name: string) => {
    setSelectedProject({ namespace, name });
    setView('project');
  };

  const handleSelectFile = (path: string) => {
    setSelectedFile(path);
  };

  return (
    <div className="app-container" style={{ height: '100vh', display: 'flex', flexDirection: 'column' }}>
      {view === 'list' ? (
        <ProjectList onSelectProject={handleSelectProject} />
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
          {/* Header */}
          <div style={{ 
            padding: '12px 24px', 
            borderBottom: '1px solid #d0d7de', 
            display: 'flex', 
            alignItems: 'center', 
            gap: '12px',
            backgroundColor: '#f6f8fa'
          }}>
            <button 
              onClick={() => {
                setView('list');
                setSelectedProject(null);
                setSelectedFile(undefined);
              }}
              style={{ 
                backgroundColor: '#ffffff', 
                border: '1px solid #d0d7de', 
                borderRadius: '6px', 
                padding: '5px 12px', 
                fontSize: '14px', 
                fontWeight: 600, 
                cursor: 'pointer' 
              }}
            >
              Back
            </button>
            <h2 style={{ margin: 0, fontSize: '16px', fontWeight: 600 }}>
              {selectedProject?.namespace} / {selectedProject?.name}
            </h2>
          </div>

          {/* Main Layout */}
          <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
            {/* Sidebar */}
            <div style={{ 
              width: '260px', 
              borderRight: '1px solid #d0d7de', 
              overflowY: 'auto',
              backgroundColor: '#ffffff'
            }}>
              {selectedProject && (
                <FileTree 
                  namespace={selectedProject.namespace}
                  projectName={selectedProject.name}
                  onSelectFile={handleSelectFile}
                  selectedPath={selectedFile}
                />
              )}
            </div>

            {/* Content Area */}
            <div style={{ flex: 1, display: 'flex', flexDirection: 'column', backgroundColor: '#ffffff' }}>
              {selectedFile ? (
                <div style={{ padding: '24px' }}>
                  <h3 style={{ marginTop: 0, fontSize: '14px', color: '#636c76' }}>{selectedFile}</h3>
                  <div style={{ 
                    border: '1px solid #d0d7de', 
                    borderRadius: '6px', 
                    padding: '40px', 
                    textAlign: 'center',
                    color: '#636c76',
                    backgroundColor: '#f6f8fa'
                  }}>
                    <p>Editor coming soon (Task 5)...</p>
                  </div>
                </div>
              ) : (
                <div style={{ 
                  flex: 1, 
                  display: 'flex', 
                  alignItems: 'center', 
                  justifyContent: 'center', 
                  color: '#636c76' 
                }}>
                  <p>Select a file to view its content</p>
                </div>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function App() {
  return (
    <WebSocketProvider>
      <AppContent />
    </WebSocketProvider>
  );
}

export default App;
