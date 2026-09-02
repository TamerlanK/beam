const PROTO_V = 1;

export function createSocket({ params, onOpen, onClose, onMessage, onBinary }) {
  let ws = null;
  let delay = 1000;

  function connect() {
    const scheme = location.protocol === "https:" ? "wss" : "ws";
    const q = params ? `?${new URLSearchParams(params())}` : "";
    ws = new WebSocket(`${scheme}://${location.host}/ws${q}`);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => { delay = 1000; onOpen(); };
    ws.onmessage = (ev) => {
      if (typeof ev.data !== "string") return onBinary(ev.data);
      let env;
      try { env = JSON.parse(ev.data); } catch { return; }
      if (env.v === PROTO_V && env.type) onMessage(env.type, env.data || {});
    };
    ws.onclose = () => {
      onClose();
      setTimeout(connect, delay);
      delay = Math.min(delay * 2, 15000);
    };
  }

  connect();

  return {
    get open() { return !!ws && ws.readyState === WebSocket.OPEN; },
    send(type, data) {
      if (this.open) ws.send(JSON.stringify({ v: PROTO_V, type, data }));
    },
    sendRaw(buf) {
      if (this.open) ws.send(buf);
    },
  };
}
