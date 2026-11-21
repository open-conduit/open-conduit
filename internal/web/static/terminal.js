// The terminal page: one WebSocket to the view, which relays it to the
// daemon. Binary messages are the terminal's bytes; text messages are control.
(function () {
  const screen = document.getElementById('terminal');
  if (!screen || !window.Terminal) return;
  const status = document.getElementById('terminal-status');
  const detach = document.getElementById('terminal-detach');
  const term = new window.Terminal({
    cursorBlink: true,
    fontFamily: "'IBM Plex Mono', ui-monospace, 'SF Mono', Menlo, monospace",
    fontSize: 13,
    scrollback: 5000,
    theme: { background: '#101216', foreground: '#ebe8e1' },
  });
  const fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(screen);
  fit.fit();

  const query = new URLSearchParams({ id: screen.dataset.work, cols: term.cols, rows: term.rows });
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const socket = new WebSocket(scheme + '//' + location.host + '/work/terminal/stream?' + query,
    ['open-conduit.terminal', 'csrf.' + screen.dataset.csrf]);
  socket.binaryType = 'arraybuffer';
  const encoder = new TextEncoder();
  let ended = '';

  const send = (data) => { if (socket.readyState === WebSocket.OPEN) socket.send(data); };
  socket.onopen = () => {
    status.textContent = 'Attached. The Work is held until you detach.';
    detach.disabled = false;
    term.focus();
  };
  socket.onmessage = (e) => {
    if (typeof e.data !== 'string') {
      term.write(new Uint8Array(e.data));
      return;
    }
    const m = JSON.parse(e.data);
    if (m.type === 'exit') ended = 'The session ended (exit ' + m.code + '). The hold is released.';
    if (m.type === 'error') ended = m.message;
  };
  socket.onclose = () => {
    status.textContent = ended || 'Detached. The hold is released.';
    detach.disabled = true;
    term.options.disableStdin = true;
  };
  detach.addEventListener('click', () => socket.close(1000));
  term.onData((d) => send(encoder.encode(d)));
  term.onBinary((d) => send(Uint8Array.from(d, (c) => c.charCodeAt(0))));
  term.onResize(({ cols, rows }) => send(JSON.stringify({ type: 'resize', cols, rows })));
  new ResizeObserver(() => fit.fit()).observe(screen);
})();
