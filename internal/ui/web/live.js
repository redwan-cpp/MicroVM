// live.js: the live page. It feeds app.js from the server's event stream
// and sends the buttons' requests. Only this page says LIVE (live.html).
'use strict';

(function () {
  const conn = document.getElementById('conn');

  // post sends one request to the server: JSON, same origin, with the
  // page's cookie. The server checks Origin and the cookie on it.
  function post(path, body) {
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      credentials: 'same-origin',
      cache: 'no-store',
    }).then((r) =>
      r.json().catch(() => ({})).then((j) => {
        if (!r.ok) throw new Error(j.error || r.status + ' ' + r.statusText);
        return j;
      }),
    );
  }

  function failed(err) {
    MVM.setStatus(String(err && err.message ? err.message : err), true);
  }

  MVM.init({
    replay: false,
    scenarios: MVM.scenarios,
    now: () => performance.now(),
    onScenario: (body) => {
      MVM.setStatus('', true);
      post('/api/run', body).catch(failed);
    },
    onStop: () => post('/api/stop', {}).catch(failed),
    onVerify: () => post('/api/verify', {}).catch(failed),
  });

  const es = new EventSource('/api/events');
  es.onopen = () => {
    conn.textContent = 'connected';
    conn.className = 'conn';
  };
  es.onerror = () => {
    conn.textContent = 'connection lost, reconnecting…';
    conn.className = 'conn bad';
  };
  es.onmessage = (e) => {
    let ev;
    try {
      ev = JSON.parse(e.data);
    } catch (_) {
      return;
    }
    MVM.apply(ev);
  };
})();
