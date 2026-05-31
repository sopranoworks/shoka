import React, { useState, useEffect, useRef } from 'react';
import ReactMarkdown from 'react-markdown';
import { Save, Eye, EyeOff } from 'lucide-react';
import { useWebSocket } from './WebSocketContext';
import './Editor.css';

interface EditorProps {
  namespace: string;
  projectName: string;
  filePath: string;
  onClose?: () => void;
}

// externalChange tracks an out-of-band change to the open file, surfaced as a
// non-blocking banner. The displayed content is never overwritten without the
// user's consent (directive §6.2 Case B / §6.4).
type ExternalChange = null | 'updated' | 'deleted';

const Editor: React.FC<EditorProps> = ({ namespace, projectName, filePath, onClose }) => {
  const [content, setContent] = useState('');
  const [isPreviewVisible, setIsPreviewVisible] = useState(true);
  const [isSaving, setIsSaving] = useState(false);
  const [externalChange, setExternalChange] = useState<ExternalChange>(null);
  const { socket, connected } = useWebSocket();
  const debounceTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Reset the banner whenever the open file changes (a fresh load is authoritative).
  useEffect(() => {
    setExternalChange(null);
  }, [namespace, projectName, filePath]);

  useEffect(() => {
    if (connected && socket && filePath) {
      const handleMessage = (event: MessageEvent) => {
        try {
          const msg = JSON.parse(event.data);

          if (msg.type === 'READ_FILE' && msg.payload?.path === filePath) {
            setContent(msg.payload.content || '');
          } else if (msg.type === 'SAVE_ACK' && msg.payload?.path === filePath) {
            setIsSaving(false);
          } else if (msg.type === 'ERROR') {
            console.error('Server error:', msg.payload?.message);
            setIsSaving(false);
          } else if (msg.type === 'NOTIFY') {
            // The open file changed under us. Inform the user with a banner but
            // never silently overwrite their content (directive §6.2 Case B).
            // Multiple events collapse into one banner; Reload always fetches the
            // latest state regardless of how many arrived.
            const ev = msg.payload;
            if (ev && ev.target === `${namespace}/${projectName}` && ev.path === filePath) {
              if (ev.kind === 'file.write') {
                setExternalChange('updated');
              } else if (ev.kind === 'file.delete') {
                setExternalChange('deleted');
              }
            }
          }
        } catch (err) {
          console.error('Failed to parse WebSocket message:', err);
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

  const reloadFromServer = () => {
    setExternalChange(null);
    if (socket && connected) {
      socket.send(JSON.stringify({
        type: 'READ_FILE',
        payload: { namespace, projectName, path: filePath }
      }));
    }
  };

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
    }
  };

  return (
    <div className="editor-container">
      {externalChange && (
        <div
          role="status"
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: '12px',
            padding: '8px 16px',
            background: externalChange === 'deleted' ? '#ffebe9' : '#fff8c5',
            borderBottom: '1px solid #d0d7de',
            fontSize: '13px',
          }}
        >
          <span style={{ flex: 1 }}>
            {externalChange === 'deleted'
              ? 'This file was deleted by another client.'
              : 'This file was updated by another client.'}
          </span>
          {externalChange === 'deleted' ? (
            <button onClick={() => { setExternalChange(null); onClose?.(); }}>Close</button>
          ) : (
            <>
              <button onClick={reloadFromServer} style={{ fontWeight: 600 }}>Reload</button>
              <button
                onClick={() => setExternalChange(null)}
                title="Dismiss"
                style={{ background: 'none', border: 'none', cursor: 'pointer', fontSize: '16px', lineHeight: 1 }}
              >
                ×
              </button>
            </>
          )}
        </div>
      )}
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
