import React, { useState, useEffect, useRef } from 'react';
import ReactMarkdown from 'react-markdown';
import { Save, Eye, EyeOff } from 'lucide-react';
import { useWebSocket } from './WebSocketContext';
import './Editor.css';

interface EditorProps {
  namespace: string;
  projectName: string;
  filePath: string;
}

const Editor: React.FC<EditorProps> = ({ namespace, projectName, filePath }) => {
  const [content, setContent] = useState('');
  const [isPreviewVisible, setIsPreviewVisible] = useState(true);
  const [isSaving, setIsSaving] = useState(false);
  const { socket, connected } = useWebSocket();
  const debounceTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (connected && socket && filePath) {
      const handleMessage = (event: MessageEvent) => {
        const msg = JSON.parse(event.data);
        if (msg.type === 'READ_FILE') {
          setContent(msg.payload.content || '');
        }
      };

      socket.addEventListener('message', handleMessage);

      socket.send(JSON.stringify({
        type: 'READ_FILE',
        payload: { namespace, projectName, path: filePath }
      }));

      return () => {
        socket.removeEventListener('message', handleMessage);
      };
    }
  }, [connected, socket, namespace, projectName, filePath]);

  const handleContentChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const newContent = e.target.value;
    setContent(newContent);

    if (debounceTimer.current) {
      clearTimeout(debounceTimer.current);
    }

    debounceTimer.current = setTimeout(() => {
      if (socket && connected) {
        socket.send(JSON.stringify({
          type: 'WRITE_DRAFT',
          payload: { namespace, projectName, path: filePath, content: newContent }
        }));
      }
    }, 1000);
  };

  const handleSave = () => {
    if (socket && connected) {
      setIsSaving(true);
      socket.send(JSON.stringify({
        type: 'SAVE_FILE',
        payload: { namespace, projectName, path: filePath, content }
      }));
      // We'll assume it saves successfully for now
      setTimeout(() => setIsSaving(false), 500);
    }
  };

  return (
    <div className="editor-container">
      <div className="editor-toolbar">
        <div className="file-info">
          <span className="file-path">{filePath}</span>
        </div>
        <div className="editor-actions">
          <button 
            className="toolbar-btn" 
            onClick={() => setIsPreviewVisible(!isPreviewVisible)}
            title={isPreviewVisible ? "Hide Preview" : "Show Preview"}
          >
            {isPreviewVisible ? <EyeOff size={18} /> : <Eye size={18} />}
          </button>
          <button 
            className="save-btn" 
            onClick={handleSave}
            disabled={isSaving}
          >
            <Save size={18} />
            {isSaving ? 'Saving...' : 'Save'}
          </button>
        </div>
      </div>
      <div className={`editor-main ${isPreviewVisible ? 'with-preview' : ''}`}>
        <div className="editor-pane">
          <textarea
            value={content}
            onChange={handleContentChange}
            placeholder="Type your markdown here..."
            spellCheck={false}
          />
        </div>
        {isPreviewVisible && (
          <div className="preview-pane markdown-body">
            <ReactMarkdown>{content}</ReactMarkdown>
          </div>
        )}
      </div>
    </div>
  );
};

export default Editor;
