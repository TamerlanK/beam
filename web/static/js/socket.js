const PROTO_V = 1;

export function createSocket({ params, onOpen, onClose, onMessage, onBinary }) {
  let ws = null;
  let delay = 1000;
  let timer = 0;
  let stopped = true;
  let closed = null;

  function dial() {
    const scheme = location.protocol === "https:" ? "wss" : "ws";
    const q = params ? `?${new URLSearchParams(params())}` : "";
    const sock = new WebSocket(`${scheme}://${location.host}/ws${q}`);
    ws = sock;
    sock.binaryType = "arraybuffer";
    sock.onopen = () => {
      if (ws === sock) {
        delay = 1000;
        onOpen();
      }
    };
    sock.onmessage = (ev) => {
      if (ws !== sock) return;
      if (typeof ev.data !== "string") return onBinary(ev.data);
      let env;
      try {
        env = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (env.v === PROTO_V && env.type) onMessage(env.type, env.data || {});
    };
    sock.onclose = () => {
      if (ws !== sock) return;
      ws = null;
      onClose(stopped);
      if (closed) {
        const r = closed;
        closed = null;
        r();
      }
      if (!stopped) {
        timer = setTimeout(dial, delay);
        delay = Math.min(delay * 2, 15000);
      }
    };
  }

  return {
    get open() {
      return !!ws && ws.readyState === WebSocket.OPEN;
    },
    connect() {
      if (!stopped) return;
      stopped = false;
      delay = 1000;
      dial();
    },
    close() {
      stopped = true;
      clearTimeout(timer);
      timer = 0;
      if (!ws) return Promise.resolve();
      return new Promise((r) => {
        closed = r;
        ws.close();
        setTimeout(r, 1000);
      });
    },
    send(type, data) {
      if (this.open) ws.send(JSON.stringify({ v: PROTO_V, type, data }));
    },
    sendRaw(buf) {
      if (this.open) ws.send(buf);
    },
  };
}
