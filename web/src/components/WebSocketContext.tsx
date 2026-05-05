import React, { createContext, useContext, useEffect, useState, useRef } from 'react';

interface WebSocketContextType {
  socket: WebSocket | null;
  connected: boolean;
}

const WebSocketContext = createContext<WebSocketContextType>({
  socket: null,
  connected: false,
});

export const useWebSocket = () => useContext(WebSocketContext);

export const WebSocketProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [socket, setSocket] = useState<WebSocket | null>(null);
  const [connected, setConnected] = useState(false);
  const socketRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const host = window.location.host;
    const ws = new WebSocket(`${protocol}//${host}/ws/ui`);

    ws.onopen = () => {
      console.log('WebSocket connected');
      setConnected(true);
    };

    ws.onclose = () => {
      console.log('WebSocket disconnected');
      setConnected(false);
    };

    setSocket(ws);
    socketRef.current = ws;

    return () => {
      ws.close();
    };
  }, []);

  return (
    <WebSocketContext.Provider value={{ socket, connected }}>
      {children}
    </WebSocketContext.Provider>
  );
};
