// app.js: draws every panel of the sandboxctl ui page from events. The
// live page (live.js) feeds it the server's event stream; the replay page
// (player.js) feeds it a recording. Both call MVM.apply(event) with the
// same events, so there is one renderer for both.
//
// Everything a guest wrote, and every other text from the server, is
// shown only through textContent, after clean() removed control and
// bidirectional-override characters. This file never parses HTML.
'use strict';

(function () {
  // ---- Text ----------------------------------------------------------

  // Terminal escape sequences (colors, cursor moves, titles) and every
  // other control character except newline and tab, plus the characters
  // that reorder text on screen.
  const OSC = /\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?/g;
  const CSI = /\x1b\[[0-?]*[ -/]*[@-~]/g;
  const CTRL = /[\x00-\x08\x0b-\x1f\x7f-\x9f‎‏‪-‮⁦-⁩]/g;

  function clean(s) {
    return String(s === undefined || s === null ? '' : s).replace(OSC, '').replace(CSI, '').replace(CTRL, '');
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = clean(text);
    return e;
  }

  function setText(e, text) {
    e.textContent = clean(text);
  }

  function clear(e) {
    while (e.firstChild) e.removeChild(e.firstChild);
  }

  function fmtMs(ms) {
    if (!(ms >= 0)) return '-';
    if (ms < 10) return ms.toFixed(1) + ' ms';
    if (ms < 1000) return Math.round(ms) + ' ms';
    if (ms < 60000) return (ms / 1000).toFixed(2) + ' s';
    return (ms / 60000).toFixed(1) + ' min';
  }

  function fmtUs(us) {
    return fmtMs(Number(us) / 1000);
  }

  function fmtBytes(b) {
    b = Number(b);
    if (!(b >= 0)) return '-';
    if (b < 1024) return b + ' B';
    if (b < 1024 * 1024) return (b / 1024).toFixed(1) + ' KiB';
    if (b < 1024 * 1024 * 1024) return (b / 1048576).toFixed(1) + ' MiB';
    return (b / 1073741824).toFixed(2) + ' GiB';
  }

  function shortHash(h) {
    return h ? String(h).slice(0, 10) + '…' : '-';
  }

  function clock(iso) {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '-';
    const p = (n) => String(n).padStart(2, '0');
    return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  function pct(x) {
    return Math.max(0, Math.min(100, x)) + '%';
  }

  // ---- State -----------------------------------------------------------

  const PHASES = ['PENDING', 'RESTORING', 'MOUNTING', 'RUNNING', 'STOPPING'];
  const PHASE_LABEL = { PENDING: 'queued', RESTORING: 'restore', MOUNTING: 'mount', RUNNING: 'run', STOPPING: 'stop' };
  const LANE_KEEP = 64 * 1024; // characters of output each lane keeps

  const S = {
    opts: null,
    info: null,
    scenario: null, // the newest scenario event
    lanes: [],
    running: false,
    ranOnce: false, // a scenario has ended on this page
    results: new Map(), // run ID -> result, every run this page saw end (timing panel)
    host: null,
    lastVerify: null,
    dom: {},
  };

  function now() {
    return S.opts && S.opts.now ? S.opts.now() : performance.now();
  }

  // ---- Layout ----------------------------------------------------------

  function panel(id, title, caption) {
    const p = el('section', 'panel');
    p.id = id;
    const h = el('h2', null, title);
    p.appendChild(h);
    const c = el('p', 'caption', caption);
    p.appendChild(c);
    return { root: p, title: h, caption: c };
  }

  function build() {
    const app = document.getElementById('app');
    const d = S.dom;
    const replay = S.opts.replay;
    const rec = replay ? ' (recorded)' : '';

    // Scenarios.
    const sp = panel('p-scenarios', 'Scenarios', replay
      ? 'The scenarios of this recording. Clicking one plays it back with its original timing; nothing runs.'
      : 'Click a scenario: sandboxd restores fresh clones and runs it. The page itself runs nothing.');
    d.buttons = el('div', 'buttons');
    d.scenarioButtons = new Map();
    for (const sc of S.opts.scenarios) {
      const b = el('button', 'btn', sc.title);
      b.type = 'button';
      b.addEventListener('click', () => S.opts.onScenario({ scenario: sc.id }));
      d.scenarioButtons.set(sc.id, b);
      d.buttons.appendChild(b);
    }
    sp.root.appendChild(d.buttons);
    d.extra = el('div');
    sp.root.appendChild(d.extra);

    const form = el('form', 'custom');
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      if (replay) return;
      S.opts.onScenario({
        scenario: 'custom',
        line: d.line.value,
        clones: Number(d.clones.value) || 1,
        class: d.klass.value,
        timeout_ms: Math.round((Number(d.timeout.value) || 30) * 1000),
      });
    });
    d.line = el('input', 'line');
    d.line.type = 'text';
    d.line.maxLength = 4096;
    d.line.placeholder = replay ? 'Replay: only the recorded scenarios play' : 'a shell line, run with sh -c in each clone, e.g. uname -a; cat /proc/cpuinfo | head -5';
    d.line.setAttribute('aria-label', 'Custom command line');
    form.appendChild(d.line);
    const lc = el('label', null, 'clones');
    d.clones = el('input');
    d.clones.type = 'number';
    d.clones.min = '1';
    d.clones.max = '8';
    d.clones.value = '1';
    lc.appendChild(d.clones);
    form.appendChild(lc);
    const lk = el('label', null, 'class');
    d.klass = el('select');
    lk.appendChild(d.klass);
    form.appendChild(lk);
    const lt = el('label', null, 'timeout (s)');
    d.timeout = el('input');
    d.timeout.type = 'number';
    d.timeout.min = '1';
    d.timeout.max = '600';
    d.timeout.value = '30';
    lt.appendChild(d.timeout);
    form.appendChild(lt);
    d.runCustom = el('button', 'btn btn-primary', 'Run');
    d.runCustom.type = 'submit';
    form.appendChild(d.runCustom);
    if (replay) {
      for (const x of [d.line, d.clones, d.klass, d.timeout, d.runCustom]) x.disabled = true;
    }
    sp.root.appendChild(form);

    const st = el('div', 'status');
    d.status = el('span', null, 'Waiting for sandboxd…');
    st.appendChild(d.status);
    d.stop = el('button', 'btn btn-stop btn-small', 'Stop');
    d.stop.type = 'button';
    d.stop.disabled = true;
    d.stop.addEventListener('click', () => S.opts.onStop && S.opts.onStop());
    if (!replay) st.appendChild(d.stop);
    d.error = el('span', 'err');
    st.appendChild(d.error);
    sp.root.appendChild(st);
    d.now = el('p', 'now');
    sp.root.appendChild(d.now);
    app.appendChild(sp.root);

    // Lanes.
    const lp = panel('p-lanes', 'Clones', 'One lane per clone: its phases as sandboxd timed them, its output (shown as text only), and the outcome the host judged. Each clone is restored from the same snapshot and destroyed afterwards.');
    d.lanes = el('div', 'lanes');
    d.lanes.appendChild(el('p', 'empty', 'No scenario yet.'));
    lp.root.appendChild(d.lanes);
    app.appendChild(lp.root);

    // Host and audit.
    const g1 = el('div', 'grid2');
    const hp = panel('p-host', 'Host' + rec, 'As reported by sandboxd, every half second: its Firecracker processes, run folders, jails and cgroups right now, and each clone\'s memory. RSS counts a shared page once per clone; PSS splits it between them, so the clones\' PSS adds up to less than their RSS when they share the snapshot\'s memory.');
    d.hostTitle = hp.title;
    d.hostBody = el('div');
    hp.root.appendChild(d.hostBody);
    g1.appendChild(hp.root);
    const ap = panel('p-audit', 'Audit log' + rec, 'One record per run, written before the run is reported done. Each record holds the SHA-256 of the one before (prev → hash), so changing, removing or reordering a record breaks the chain. Deleting the newest records, or rewriting the whole file, is not detected.');
    d.auditList = el('ul', 'chain');
    ap.root.appendChild(d.auditList);
    const vr = el('div', 'verify');
    d.verifyBtn = el('button', 'btn btn-small', replay ? 'Verify chain (recorded result)' : 'Verify chain');
    d.verifyBtn.type = 'button';
    d.verifyBtn.disabled = replay;
    d.verifyBtn.addEventListener('click', () => S.opts.onVerify && S.opts.onVerify());
    vr.appendChild(d.verifyBtn);
    d.verdict = el('span', null, replay ? '' : 'not verified yet on this page');
    vr.appendChild(d.verdict);
    ap.root.appendChild(vr);
    g1.appendChild(ap.root);
    app.appendChild(g1);

    // Timing and history.
    const g2 = el('div', 'grid2');
    const tp = panel('p-timing', 'Timing' + rec, replay
      ? 'Every restore in this recording, as sandboxd measured it, next to the cold boot that make snapshots measured on the same machine (InstanceStart to the agent\'s first answer).'
      : 'This page\'s restores, as sandboxd measured them, next to the cold boot that make snapshots measured on this machine (InstanceStart to the agent\'s first answer).');
    d.timingTitle = tp.title;
    d.timing = el('div');
    tp.root.appendChild(d.timing);
    g2.appendChild(tp.root);
    const rp = panel('p-history', 'Recent runs' + rec, 'What sandboxctl ps --all shows: every run sandboxd remembers (the last 100), also those started from the command line, with the caller\'s UID from the socket.');
    d.history = el('div');
    rp.root.appendChild(d.history);
    g2.appendChild(rp.root);
    app.appendChild(g2);

    renderTiming();
    renderHost();
    updateControls();
  }

  // ---- Header ----------------------------------------------------------

  function renderInfo() {
    const f = document.getElementById('facts');
    if (!f || !S.info) return;
    const i = S.info;
    clear(f);
    const fact = (label, value, cls) => {
      const s = el('span', null, label + ' ');
      const b = el('b', cls, value);
      s.appendChild(b);
      f.appendChild(s);
    };
    const mode = i.mode === 'jailer'
      ? el('span', 'badge badge-mode-jailer', 'jailer mode')
      : el('span', 'badge badge-mode-rootless', 'rootless mode (fallback)');
    mode.title = i.mode === 'jailer'
      ? 'Each Firecracker runs in its own chroot, as its own UID, in its own cgroup (ADR-0005).'
      : 'Firecracker runs as the user, without the jailer\'s chroot, UID drop and cgroups (ADR-0005). Fine for a demo, not for hostile code.';
    f.appendChild(mode);
    const hk = i.host_kind === 'virtual' ? 'inside a VM (nested virtualization)' : i.host_kind === 'physical' ? 'physical machine' : 'host kind unknown';
    const hb = el('span', 'badge' + (i.host_kind === 'virtual' ? ' badge-host-virtual' : ''), 'Linux ' + i.arch + ', ' + hk);
    hb.title = i.host_detail || '';
    f.appendChild(hb);
    fact('Firecracker', i.firecracker_version);
    const cls = (i.classes || [])[0];
    if (cls) {
      fact('guest kernel', cls.kernel_version || '?');
      fact('agent', (cls.agent_version || '?').replace(/^sandbox-agent /, ''));
    }
    fact('classes', (i.classes || []).map((c) => c.name + ' (' + c.vcpus + ' vCPU, ' + c.memory_mib + ' MiB)').join(', ') || 'none built');
    fact('at most', i.max_concurrent + ' clones at once');
    fact('sandboxd', (i.daemon_version || '').replace(/^sandboxd /, ''));

    // The custom form follows the daemon's limits.
    const d = S.dom;
    const keep = d.klass.value;
    clear(d.klass);
    for (const c of i.classes || []) {
      const o = el('option', null, c.name);
      o.value = c.name;
      d.klass.appendChild(o);
    }
    if (keep) d.klass.value = keep;
    d.clones.max = String(Math.max(1, i.max_clones || 1));
    d.hostTitle.textContent = clean((i.host_kind === 'virtual' ? 'Host: the Linux VM' : 'Host') + (S.opts.replay ? ' (recorded)' : ''));
    renderTiming();
    updateControls();
  }

  function updateControls() {
    const d = S.dom;
    if (!d.buttons) return;
    const idle = !S.running && !!S.info;
    if (!S.opts.replay) {
      for (const [, b] of d.scenarioButtons) b.disabled = !idle;
      d.runCustom.disabled = !idle;
      d.stop.disabled = !S.running;
    }
    if (S.opts.onControls) S.opts.onControls(idle);
  }

  // ---- Lanes -----------------------------------------------------------

  function newLane(i, argv) {
    const l = {
      index: i,
      runId: '',
      states: [], // {state, ms}
      openAt: 0, // now() when the newest state arrived
      result: null,
      spans: [], // the output, oldest first
      chars: 0,
      trimmed: false,
    };
    const root = el('div', 'lane');
    const head = el('div', 'lane-head');
    head.appendChild(el('span', null, 'clone ' + (i + 1)));
    l.idEl = el('span', 'mono', 'waiting for a run ID');
    head.appendChild(l.idEl);
    root.appendChild(head);
    l.bar = el('div', 'phasebar');
    l.segs = {};
    for (const p of PHASES) {
      const s = el('span', 'seg seg-' + p);
      s.style.width = '0';
      s.style.left = '0';
      s.title = PHASE_LABEL[p];
      l.segs[p] = s;
      l.bar.appendChild(s);
    }
    root.appendChild(l.bar);
    l.legend = el('div', 'legend');
    root.appendChild(l.legend);
    l.term = el('pre', 'term');
    l.term.setAttribute('aria-label', 'output of clone ' + (i + 1));
    root.appendChild(l.term);
    const foot = el('div', 'lane-foot');
    l.chip = el('span', 'chip', 'pending');
    foot.appendChild(l.chip);
    l.timings = el('span');
    foot.appendChild(l.timings);
    l.audit = el('span', 'mono');
    foot.appendChild(l.audit);
    l.detail = el('span', 'detail');
    foot.appendChild(l.detail);
    root.appendChild(foot);
    l.root = root;
    l.cmd = argv;
    return l;
  }

  function startScenario(sc) {
    S.scenario = sc;
    S.running = true;
    S.lanes = [];
    const d = S.dom;
    clear(d.lanes);
    for (let i = 0; i < sc.clones; i++) {
      const l = newLane(i, (sc.argv || [])[i] || []);
      S.lanes.push(l);
      d.lanes.appendChild(l.root);
    }
    setText(d.status, 'Running: ' + sc.title + ' (' + sc.clones + (sc.clones === 1 ? ' clone' : ' clones') + ', class ' + sc.class + ', timeout ' + fmtMs(sc.timeout_ms) + ', output cap ' + fmtBytes(sc.max_output) + ')');
    setText(d.error, '');
    clear(d.now);
    d.now.appendChild(el('span', null, sc.caption + ' '));
    const cmds = new Set((sc.argv || []).map((a) => a.join(' ')));
    const shown = sc.argv && sc.argv.length > 1 && cmds.size > 1 ? (sc.argv[0] || []).join(' ') + '   (and the same with each clone\'s number)' : ((sc.argv || [])[0] || []).join(' ');
    d.now.appendChild(el('code', null, shown));
    updateControls();
  }

  function lane(i) {
    return S.lanes[i] || null;
  }

  function onState(st) {
    const l = lane(st.lane);
    if (!l || !S.scenario || st.seg !== S.scenario.seg) return;
    if (st.run_id && !l.runId) {
      l.runId = st.run_id;
      setText(l.idEl, st.run_id);
    }
    l.states.push({ state: st.state, ms: Number(st.ms) || 0 });
    l.openAt = now();
    if (st.state !== 'DONE' && st.state !== 'FAILED') {
      setText(l.chip, st.state.toLowerCase());
      l.chip.className = 'chip';
    }
    if (st.detail) setText(l.detail, st.detail);
    renderLane(l);
  }

  // phases returns a lane's phases: [{state, start, end, open}].
  function phases(l, t) {
    const out = [];
    for (let k = 0; k < l.states.length; k++) {
      const s = l.states[k];
      if (s.state === 'DONE' || s.state === 'FAILED') break;
      const next = l.states[k + 1];
      const open = !next;
      const end = next ? next.ms : s.ms + Math.max(0, t - l.openAt);
      out.push({ state: s.state, start: s.ms, end: end, open: open && !l.result });
    }
    return out;
  }

  function scaleMax(t) {
    let m = 1;
    for (const l of S.lanes) {
      const ps = phases(l, t);
      if (ps.length) m = Math.max(m, ps[ps.length - 1].end);
    }
    return m;
  }

  function renderLane(l, t, max) {
    t = t === undefined ? now() : t;
    max = max || scaleMax(t);
    const ps = phases(l, t);
    for (const p of PHASES) {
      l.segs[p].style.width = '0';
    }
    clear(l.legend);
    for (const p of ps) {
      const s = l.segs[p.state];
      if (!s) continue;
      s.style.left = pct((p.start / max) * 100);
      s.style.width = pct(((p.end - p.start) / max) * 100);
      const item = el('span', 'k-' + p.state + (p.open ? ' cur' : ''), PHASE_LABEL[p.state] + ' ' + fmtMs(p.end - p.start));
      l.legend.appendChild(item);
    }
  }

  function renderLanes() {
    if (!S.lanes.length) return;
    const t = now();
    const max = scaleMax(t);
    for (const l of S.lanes) renderLane(l, t, max);
  }

  function onOutput(o) {
    const l = lane(o.lane);
    if (!l || !S.scenario || o.seg !== S.scenario.seg) return;
    if (o.gap && !l.trimmed) markTrimmed(l);
    const text = clean(o.data);
    if (!text) return;
    const atBottom = l.term.scrollHeight - l.term.scrollTop - l.term.clientHeight < 40;
    const span = el('span', o.stream === 'stderr' ? 'e' : null);
    span.textContent = text;
    l.term.appendChild(span);
    l.spans.push(span);
    l.chars += text.length;
    // Keep only the newest LANE_KEEP characters: a flood or a fork bomb
    // must not grow the page without end.
    while (l.chars > LANE_KEEP && l.spans.length) {
      const first = l.spans[0];
      const n = first.textContent.length;
      const over = l.chars - LANE_KEEP;
      if (n <= over) {
        l.term.removeChild(first);
        l.spans.shift();
        l.chars -= n;
      } else {
        first.textContent = first.textContent.slice(over);
        l.chars -= over;
      }
      if (!l.trimmed) markTrimmed(l);
    }
    if (atBottom) l.term.scrollTop = l.term.scrollHeight;
  }

  function markTrimmed(l) {
    l.trimmed = true;
    const m = el('span', 'trimmed', '… earlier output trimmed (the page keeps the newest 64 KiB of each clone)');
    l.term.insertBefore(m, l.term.firstChild);
  }

  function outcomeText(r) {
    switch (r.outcome) {
      case 'EXITED':
        return r.exit_code < 0 ? 'killed by signal ' + r.signal : 'exited ' + r.exit_code;
      case 'TIMED_OUT':
        return 'timed out';
      case 'OUTPUT_TRUNCATED':
        return 'output capped';
      case 'FAILED':
        return 'failed';
    }
    return String(r.outcome || '?').toLowerCase();
  }

  function onResult(r) {
    const l = lane(r.lane);
    if (!l || !S.scenario || r.seg !== S.scenario.seg) return;
    l.result = r;
    if (r.run_id && !l.runId) {
      l.runId = r.run_id;
      setText(l.idEl, r.run_id);
    }
    let cls = 'chip chip-fail';
    if (r.outcome === 'EXITED') cls = r.exit_code === 0 ? 'chip chip-ok' : 'chip chip-exit';
    if (r.outcome === 'TIMED_OUT') cls = 'chip chip-timeout';
    if (r.outcome === 'OUTPUT_TRUNCATED') cls = 'chip chip-cap';
    l.chip.className = cls;
    setText(l.chip, outcomeText(r));
    const t = r.timings_us || {};
    let line = 'restore ' + fmtUs(t.restore) + ' (LoadSnapshot ' + fmtUs(t.load_snapshot) + ')';
    if (r.outcome === 'OUTPUT_TRUNCATED') line += ', ' + fmtBytes(r.output_bytes) + ' passed on';
    if (r.outcome === 'TIMED_OUT' && S.scenario) line += ', after ' + fmtMs(S.scenario.timeout_ms);
    setText(l.timings, line + ', ' + r.mode);
    if (r.error) setText(l.detail, r.error);
    if (r.run_id) S.results.set(r.run_id, { r: r, klass: S.scenario.class });
    renderLane(l);
    renderTiming();
  }

  function onEnd(e) {
    if (!S.scenario || e.seg !== S.scenario.seg) return;
    S.running = false;
    S.ranOnce = true;
    for (const a of e.audit || []) {
      const l = lane(a.lane);
      if (!l) continue;
      setText(l.audit, a.hash ? 'audit record ' + shortHash(a.hash) : 'audit: ' + (a.missing || '?'));
      if (a.hash) l.audit.title = a.hash;
    }
    const d = S.dom;
    setText(d.status, 'Done: ' + S.scenario.title);
    if (e.error) setText(d.error, e.error);
    renderLanes();
    renderHost();
    updateControls();
  }

  // ---- Host ------------------------------------------------------------

  function countBox(label, c) {
    const box = el('div', 'count');
    const bad = c && c.error;
    box.appendChild(el('span', 'n', bad ? 'n/a' : String(c ? c.n : '-')));
    box.appendChild(el('span', 'what', label));
    if (bad) box.appendChild(el('span', 'why', c.error));
    return box;
  }

  function renderHost() {
    const d = S.dom;
    if (!d.hostBody) return;
    clear(d.hostBody);
    const h = S.host;
    if (!h) {
      d.hostBody.appendChild(el('p', 'empty', 'Waiting for sandboxd…'));
      return;
    }
    if (h.error) {
      d.hostBody.appendChild(el('p', 'warn', 'n/a: ' + h.error));
      return;
    }
    const counts = el('div', 'counts');
    counts.appendChild(countBox('Firecracker processes', h.processes));
    counts.appendChild(countBox('run folders', h.run_dirs));
    counts.appendChild(countBox('jails', h.jail_dirs));
    counts.appendChild(countBox('clone cgroups', h.cgroups));
    d.hostBody.appendChild(counts);

    const all = [h.processes, h.run_dirs, h.jail_dirs, h.cgroups];
    const known = all.every((c) => c && !c.error);
    const zero = known && all.every((c) => c.n === 0) && (h.clones || []).length === 0;
    if (!S.running && S.ranOnce) {
      if (zero) {
        d.hostBody.appendChild(el('div', 'clean', '✓ 0 VMs, 0 folders left, as reported by sandboxd'));
      } else if (known) {
        d.hostBody.appendChild(el('div', 'dirty', 'Still there after the runs: see the counts above'));
      }
    }

    const clones = h.clones || [];
    if (clones.length) {
      let rss = 0;
      let pss = 0;
      let unknown = 0;
      const tbl = el('table');
      const hr = el('tr');
      for (const c of ['run', 'Firecracker PID', 'jailed UID', 'RSS', 'PSS']) hr.appendChild(el('th', c === 'RSS' || c === 'PSS' ? 'num' : null, c));
      tbl.appendChild(hr);
      for (const c of clones) {
        const tr = el('tr');
        tr.appendChild(el('td', 'mono', c.run_id));
        tr.appendChild(el('td', 'num', String(c.pid)));
        tr.appendChild(el('td', 'num', c.jailed_uid ? String(c.jailed_uid) : '-'));
        if (c.memory_error) {
          const td = el('td', 'warn', 'n/a: ' + c.memory_error);
          td.colSpan = 2;
          tr.appendChild(td);
          unknown++;
        } else {
          tr.appendChild(el('td', 'num', fmtBytes(c.rss)));
          tr.appendChild(el('td', 'num', fmtBytes(c.pss)));
          rss += Number(c.rss);
          pss += Number(c.pss);
        }
        tbl.appendChild(tr);
      }
      if (rss > 0) {
        const bars = el('div', 'membars');
        const row = (label, v, cls) => {
          const r = el('div', 'membar');
          r.appendChild(el('span', null, label));
          const bar = el('div', 'bar');
          const fill = el('div', 'fill ' + cls);
          fill.style.width = pct((v / rss) * 100);
          bar.appendChild(fill);
          r.appendChild(bar);
          r.appendChild(el('span', 'num', fmtBytes(v)));
          return r;
        };
        bars.appendChild(row('Σ RSS', rss, ''));
        bars.appendChild(row('Σ PSS', pss, 'fill-pss'));
        d.hostBody.appendChild(bars);
        if (unknown) d.hostBody.appendChild(el('p', 'caption', unknown + ' clone(s) could not be read; they are not in the sums.'));
      }
      d.hostBody.appendChild(tbl);
    }
  }

  // ---- Audit -----------------------------------------------------------

  function renderAudit(a) {
    const d = S.dom;
    clear(d.auditList);
    if (a.error) {
      d.auditList.appendChild(el('li', 'warn', a.error));
      return;
    }
    const recs = a.records || [];
    if (!recs.length) {
      d.auditList.appendChild(el('li', 'empty', 'No records yet.'));
      return;
    }
    for (let k = recs.length - 1; k >= 0; k--) {
      const r = recs[k];
      const li = el('li');
      li.appendChild(el('span', null, clock(r.time)));
      const right = el('span');
      const what = r.kind === 'run' ? r.run_id + ' ' + (r.outcome || '') : r.kind === 'rotation' ? 'new chain (rotation)' : 'a line that is not a record';
      right.appendChild(el('span', null, what));
      right.appendChild(el('br'));
      const hs = el('span', 'hashes', 'prev ' + shortHash(r.prev_hash) + ' → hash ' + shortHash(r.hash));
      hs.title = 'prev_hash ' + clean(r.prev_hash) + '\nhash ' + clean(r.hash);
      right.appendChild(hs);
      const before = recs[k - 1];
      if (before) {
        const ok = before.hash === r.prev_hash;
        right.appendChild(el('span', ok ? 'link-ok' : 'link-bad', ok ? '  ✓ links to the record below' : '  ✗ does not link to the record below'));
      }
      li.appendChild(right);
      d.auditList.appendChild(li);
    }
  }

  function renderVerify(v, recorded) {
    const d = S.dom;
    S.lastVerify = v;
    clear(d.verdict);
    const tag = recorded ? ' (recorded)' : '';
    if (v.ok) {
      d.verdict.appendChild(el('span', 'verdict-ok', '✓ chain OK: ' + v.records + ' records, newest hash ' + shortHash(v.head) + tag));
    } else {
      const where = v.line ? 'broken at line ' + v.line + ': ' : '';
      d.verdict.appendChild(el('span', 'verdict-bad', '✗ ' + where + (v.reason || 'not verified') + tag));
    }
  }

  // ---- Timing ----------------------------------------------------------

  const STACK = [
    ['prepare', 'st-prepare', 'prepare (hard links)'],
    ['scratch', 'st-scratch', 'scratch disk copy'],
    ['process_start', 'st-process', 'Firecracker start'],
    ['load_snapshot', 'st-load', 'LoadSnapshot'],
    ['agent_ready', 'st-agent', 'agent ready'],
    ['de_twin', 'st-detwin', 'de-twin (clock, RNG)'],
  ];

  function median(xs) {
    const s = xs.slice().sort((a, b) => a - b);
    return s.length ? s[Math.floor((s.length - 1) / 2)] : 0;
  }

  function renderTiming() {
    const d = S.dom;
    if (!d.timing) return;
    clear(d.timing);
    const i = S.info;
    if (i && i.host_kind === 'virtual') {
      d.timing.appendChild(el('p', 'warn', 'Measured inside a VM (nested virtualization): not the numbers of a physical machine.'));
    }
    // The restores, by class.
    const byClass = new Map();
    for (const { r, klass } of S.results.values()) {
      if (!r.timings_us || !(Number(r.timings_us.restore) > 0)) continue;
      if (!byClass.has(klass)) byClass.set(klass, []);
      byClass.get(klass).push(r);
    }
    const rows = [];
    let max = 1;
    for (const [klass, rs] of byClass) {
      const totals = rs.map((r) => Number(r.timings_us.restore));
      const med = median(totals);
      const pick = rs.find((r) => Number(r.timings_us.restore) === med) || rs[0];
      rows.push({ label: 'restore, ' + klass + ' (' + pick.mode + ')', t: pick.timings_us, total: med });
      rows.push({ note: klass + ': median of ' + rs.length + (rs.length === 1 ? ' restore' : ' restores') + '; min ' + fmtUs(Math.min(...totals)) + ', max ' + fmtUs(Math.max(...totals)) });
      max = Math.max(max, Math.max(...totals));
    }
    if (!byClass.size) d.timing.appendChild(el('p', 'empty', 'No restore yet.'));
    for (const c of (i && i.classes) || []) {
      if (Number(c.cold_boot_us) > 0) {
        rows.push({ label: 'cold boot, ' + c.name, cold: Number(c.cold_boot_us) });
        max = Math.max(max, Number(c.cold_boot_us));
      }
    }
    for (const r of rows) {
      if (r.note) {
        d.timing.appendChild(el('p', 'caption', r.note));
        continue;
      }
      const row = el('div', 'timing-row');
      row.appendChild(el('span', null, r.label));
      const stack = el('div', 'stack');
      if (r.cold) {
        const sp = el('span', 'st-cold');
        sp.style.width = pct((r.cold / max) * 100);
        sp.title = 'cold boot ' + fmtUs(r.cold);
        stack.appendChild(sp);
      } else {
        for (const [k, cls, name] of STACK) {
          const sp = el('span', cls);
          sp.style.width = pct((Number(r.t[k]) / max) * 100);
          sp.title = name + ' ' + fmtUs(r.t[k]);
          stack.appendChild(sp);
        }
      }
      row.appendChild(stack);
      row.appendChild(el('span', 'num', fmtUs(r.cold || r.total)));
      d.timing.appendChild(row);
    }
    const lg = el('div', 'stack-legend');
    for (const [, cls, name] of STACK.concat([['', 'st-cold', 'cold boot: InstanceStart to the agent']])) {
      const item = el('span');
      item.appendChild(el('i', 'sw ' + cls));
      item.appendChild(document.createTextNode(clean(name)));
      lg.appendChild(item);
    }
    d.timing.appendChild(lg);
  }

  // ---- History ---------------------------------------------------------

  function renderHistory(h) {
    const d = S.dom;
    clear(d.history);
    if (h.error) {
      d.history.appendChild(el('p', 'warn', 'n/a: ' + h.error));
      return;
    }
    const runs = (h.runs || []).slice(-12).reverse();
    if (!runs.length) {
      d.history.appendChild(el('p', 'empty', 'No runs yet.'));
      return;
    }
    const tbl = el('table');
    const hr = el('tr');
    for (const c of ['run', 'state', 'outcome', 'mode', 'uid', 'started', 'command']) hr.appendChild(el('th', null, c));
    tbl.appendChild(hr);
    for (const r of runs) {
      const tr = el('tr');
      tr.appendChild(el('td', 'mono', r.run_id));
      tr.appendChild(el('td', null, r.state));
      tr.appendChild(el('td', null, r.outcome));
      tr.appendChild(el('td', null, r.mode + (r.jailed_uid ? ' as ' + r.jailed_uid : '')));
      tr.appendChild(el('td', 'num', String(r.caller_uid)));
      tr.appendChild(el('td', null, clock(r.created)));
      const cmd = String(r.command || '');
      tr.appendChild(el('td', 'cmd', cmd.length > 70 ? cmd.slice(0, 67) + '…' : cmd));
      tbl.appendChild(tr);
    }
    d.history.appendChild(tbl);
  }

  // ---- Events ----------------------------------------------------------

  function apply(ev) {
    if (!ev || typeof ev !== 'object') return;
    switch (ev.type) {
      case 'info':
        if (ev.info) {
          S.info = ev.info;
          renderInfo();
          if (!S.running && !S.scenario) setText(S.dom.status, 'Ready.');
        }
        break;
      case 'scenario':
        if (ev.scenario) startScenario(ev.scenario);
        break;
      case 'state':
        if (ev.state) onState(ev.state);
        break;
      case 'output':
        if (ev.output) onOutput(ev.output);
        break;
      case 'result':
        if (ev.result) onResult(ev.result);
        break;
      case 'end':
        if (ev.end) onEnd(ev.end);
        break;
      case 'host':
        if (ev.host) {
          S.host = ev.host;
          renderHost();
        }
        break;
      case 'audit':
        if (ev.audit) renderAudit(ev.audit);
        break;
      case 'verify':
        if (ev.verify) renderVerify(ev.verify, S.opts.replay);
        break;
      case 'history':
        if (ev.history) renderHistory(ev.history);
        break;
      case 'notice':
        if (ev.notice) setText(S.dom.error, ev.notice.text);
        break;
      default:
      // Unknown types are ignored: an older page and a newer server.
    }
  }

  // reset clears the lanes (the player, before it plays a scenario).
  function reset() {
    S.scenario = null;
    S.lanes = [];
    S.running = false;
    clear(S.dom.lanes);
    S.dom.lanes.appendChild(el('p', 'empty', 'No scenario yet.'));
    clear(S.dom.now);
    setText(S.dom.error, '');
    updateControls();
  }

  function init(opts) {
    S.opts = opts;
    build();
    // Grow the open phases smoothly; cheap, and paused in a hidden tab.
    setInterval(() => {
      if (S.running) renderLanes();
    }, 100);
  }

  window.MVM = {
    init: init,
    apply: apply,
    reset: reset,
    clean: clean,
    el: el,
    setText: setText,
    clear: clear,
    fmtMs: fmtMs,
    setStatus: (text, isError) => setText(isError ? S.dom.error : S.dom.status, text),
    dom: () => S.dom,
    renderVerify: renderVerify,
    // addRecordedResult puts a recorded run into the timing panel (the
    // player shows every restore of a recording at once).
    addRecordedResult: (r, klass) => {
      if (r && r.run_id) S.results.set(r.run_id, { r: r, klass: klass });
      renderTiming();
    },
    scenarios: [
      { id: 'demo1', title: 'Three clones, private /work' },
      { id: 'hostile', title: 'Hostile code stays inside' },
      { id: 'forkbomb', title: 'Fork bomb is stopped' },
      { id: 'flood', title: 'Output flood hits the cap' },
    ],
  };
})();
