import React, { useState, useEffect } from 'react';
import { Folder, FileText, ChevronRight, ChevronDown, Plus } from 'lucide-react';
import { useWebSocket } from './WebSocketContext';
import './FileTree.css';

interface FileNode {
  name: string;
  path: string;
  isDir: boolean;
  children?: FileNode[];
}

interface FileTreeProps {
  namespace: string;
  projectName: string;
  onSelectFile: (path: string) => void;
  selectedPath?: string;
}

const FileTreeNode: React.FC<{
  node: FileNode;
  depth: number;
  onSelect: (path: string) => void;
  selectedPath?: string;
}> = ({ node, depth, onSelect, selectedPath }) => {
  const [expanded, setExpanded] = useState(false);

  const handleClick = (e: React.MouseEvent) => {
    e.stopPropagation();
    if (node.isDir) {
      setExpanded(!expanded);
    } else {
      onSelect(node.path);
    }
  };

  return (
    <div>
      <div 
        className={`file-node ${selectedPath === node.path ? 'selected' : ''}`}
        onClick={handleClick}
        style={{ paddingLeft: `${depth * 12 + 8}px` }}
      >
        {node.isDir ? (
          <>
            {expanded ? <ChevronDown size={14} className="file-icon" /> : <ChevronRight size={14} className="file-icon" />}
            <Folder size={16} className="file-icon" />
          </>
        ) : (
          <FileText size={16} className="file-icon" />
        )}
        <span>{node.name}</span>
      </div>
      {node.isDir && expanded && node.children && (
        <div className="folder-children">
          {node.children.map((child) => (
            <FileTreeNode 
              key={child.path} 
              node={child} 
              depth={depth + 1} 
              onSelect={onSelect}
              selectedPath={selectedPath}
            />
          ))}
        </div>
      )}
    </div>
  );
};

const FileTree: React.FC<FileTreeProps> = ({ namespace, projectName, onSelectFile, selectedPath }) => {
  const [tree, setTree] = useState<FileNode[]>([]);
  const { socket, connected } = useWebSocket();

  useEffect(() => {
    if (connected && socket) {
      const handleMessage = (event: MessageEvent) => {
        const msg = JSON.parse(event.data);
        if (msg.type === 'GET_TREE') {
          setTree(msg.payload);
        }
      };

      socket.addEventListener('message', handleMessage);
      
      socket.send(JSON.stringify({
        type: 'GET_TREE',
        payload: { namespace, projectName }
      }));

      return () => {
        socket.removeEventListener('message', handleMessage);
      };
    }
  }, [connected, socket, namespace, projectName]);

  const handleNewFile = () => {
    const fileName = prompt('Enter file name:');
    if (fileName && socket) {
      // For now, we just send a WRITE_DRAFT with empty content
      socket.send(JSON.stringify({
        type: 'WRITE_DRAFT',
        payload: { 
          namespace, 
          projectName, 
          path: fileName,
          content: ''
        }
      }));
      // Refresh tree
      socket.send(JSON.stringify({
        type: 'GET_TREE',
        payload: { namespace, projectName }
      }));
    }
  };

  return (
    <div className="file-tree">
      <div className="file-tree-header">
        <h3>Files</h3>
        <button className="new-file-btn" onClick={handleNewFile} title="New File">
          <Plus size={16} />
        </button>
      </div>
      <div className="file-tree-nodes">
        {tree.length > 0 ? (
          tree.map((node) => (
            <FileTreeNode 
              key={node.path} 
              node={node} 
              depth={0} 
              onSelect={onSelectFile}
              selectedPath={selectedPath}
            />
          ))
        ) : (
          <div style={{ padding: '8px 16px', color: '#636c76', fontStyle: 'italic' }}>
            No files found.
          </div>
        )}
      </div>
    </div>
  );
};

export default FileTree;
