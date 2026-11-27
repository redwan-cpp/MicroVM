// player.js: the replay page (sandboxctl ui --replay, and the exported
// file). It reads the recorded session from the page's JSON data block
// and feeds its events to app.js with their original timing. It never
// connects anywhere and runs nothing. The RECORDING banner is part of
// replay.html; nothing here can remove it.
'use strict';

(function () {
  const block = document.getElementById('session');
  let data;
  try {
    data = JSON.parse(block ? block.textContent : '');
  } catch (_) {
    data = null;
  }
  const when = document.getElementById('rec-when');
  const where = document.getElementById('rec-where');
  const speedLabel = document.getElementById('rec-speed');
  if (!data || !data.meta || !data.meta.info) {
    when.textContent = 'The recording in this page could not be read.';
    return;
  }
  const meta = data.meta;
  const info = meta.info;
  const events = data.events || [];
  const end = data.end || null;

  // The banner: when and where it was made, and in which mode.
  const made = new Date(meta.created_at);
  const madeText = isNaN(made.getTime()) ? 'at an unknown time' : made.toLocaleString() + ' (' + made.toISOString().slice(0, 16).replace('T', ' ') + ' UTC)';
  const hostKind = info.host_kind === 'virtual' ? 'inside a VM' : info.host_kind === 'physical' ? 'a physical machine' : 'an unknown kind of host';
  const mode = info.mode === 'jailer' ? 'jailer mode' : 'rootless mode (fallback)';
  MVM.setText(when, 'Made ' + madeText);
  MVM.setText(where, ' on Linux ' + info.arch + ', ' + hostKind + ', ' + mode + '.');

  // Split the events into the scenarios ("segments") and what came
  // before the first one.
  const intro = [];
  const segs = [];
  for (const te of events) {
    if (!te || !te.event) continue;
    if (te.event.type === 'scenario') {
      segs.push({ sc: te.event.scenario, start: te.t, events: [te] });
    } else if (segs.length) {
      segs[segs.length - 1].events.push(te);
    } else {
      intro.push(te);
    }
  }

  // The player's clock: milliseconds into the segment, moving at the
  // chosen speed, standing still while paused.
  let vnow = 0;
  let speed = 1;
  let playing = false;
  let current = null;
  let idx = 0;
  let queue = [];
  let lastReal = performance.now();
  let ready = false; // the controls below exist

  MVM.init({
    replay: true,
    scenarios: MVM.scenarios,
    now: () => vnow,
    onScenario: (body) => {
      const seg = segs.find((s) => s.sc.id === body.scenario);
      if (seg) {
        queue = [];
        play(seg);
      }
    },
    onControls: () => {
      if (ready) buttons();
    },
  });

  const d = MVM.dom();
  const el = MVM.el;

  // The player's controls.
  const row = el('div', 'player-row');
  const all = el('button', 'btn btn-primary', 'Play all');
  all.type = 'button';
  all.addEventListener('click', () => {
    queue = segs.slice(1);
    if (segs.length) play(segs[0]);
  });
  row.appendChild(all);
  const pause = el('button', 'btn', 'Pause');
  pause.type = 'button';
  pause.addEventListener('click', () => {
    if (!current) return;
    playing = !playing;
    lastReal = performance.now();
    buttons();
  });
  row.appendChild(pause);
  const label = el('label', null, 'speed ');
  const sel = el('select');
  for (const x of [1, 2, 4]) {
    const o = el('option', null, x + '×');
    o.value = String(x);
    sel.appendChild(o);
  }
  sel.addEventListener('change', () => {
    speed = Number(sel.value) || 1;
    buttons();
  });
  label.appendChild(sel);
  row.appendChild(label);
  d.extra.appendChild(row);
  const list = el('div', 'recorded-list');
  segs.forEach((s, k) => {
    const title = s.sc.id === 'custom' ? 'Custom: ' + ((s.sc.argv || [])[0] || []).slice(2).join(' ') : s.sc.title;
    const b = el('button', 'btn btn-small', k + 1 + '. ' + title);
    b.type = 'button';
    b.addEventListener('click', () => {
      queue = [];
      play(s);
    });
    list.appendChild(b);
  });
  if (!segs.length) list.appendChild(el('span', 'empty', 'This recording holds no scenario.'));
  d.extra.appendChild(list);

  ready = true;

  function buttons() {
    for (const [id, b] of d.scenarioButtons) {
      const has = segs.some((s) => s.sc.id === id);
      b.disabled = !has;
      b.title = has ? 'play the recorded scenario' : 'not in this recording';
    }
    pause.disabled = !current;
    pause.textContent = playing || !current ? 'Pause' : 'Resume';
    const sped = speed !== 1 ? 'Playing at ' + speed + '× speed.' : '';
    const gaps = queue.length ? ' Pauses between scenarios shortened to 2 s.' : '';
    speedLabel.textContent = sped ? ' ' + sped : '';
    if (current) {
      MVM.setStatus((playing ? 'Playing' : 'Paused') + ' the recording: ' + current.sc.title + '. ' + sped + gaps);
    }
  }

  function play(seg) {
    MVM.reset();
    current = seg;
    idx = 0;
    vnow = 0;
    playing = true;
    lastReal = performance.now();
    buttons();
    step();
  }

  // step applies every event of the segment that is due.
  function step() {
    if (!current) return;
    const ev = current.events;
    while (idx < ev.length && ev[idx].t - current.start <= vnow) {
      MVM.apply(ev[idx].event);
      idx++;
    }
    if (idx >= ev.length) {
      const done = current;
      current = null;
      playing = false;
      buttons();
      MVM.setStatus('Played: ' + done.sc.title + ' (recorded).');
      if (queue.length) {
        const next = queue.shift();
        setTimeout(() => {
          if (!current) play(next);
        }, 2000);
      }
    }
  }

  setInterval(() => {
    const r = performance.now();
    if (playing) {
      vnow += (r - lastReal) * speed;
      step();
    }
    lastReal = r;
  }, 40);

  // What the page shows before anything plays: the daemon as it was,
  // the panels before the first scenario, every recorded restore in the
  // timing panel, and the audit chain's verification at the end.
  MVM.apply({ type: 'info', info: info });
  for (const te of intro) MVM.apply(te.event);
  for (const s of segs) {
    for (const te of s.events) {
      if (te.event.type === 'result') MVM.addRecordedResult(te.event.result, s.sc.class);
    }
  }
  if (end && end.verify) {
    MVM.renderVerify(end.verify, true);
  } else {
    MVM.setStatus('This recording has no end line: sandboxctl ui stopped before it could verify the audit log.', true);
  }
  if (end && end.stopped) MVM.setStatus(end.stopped, true);

  // Every recorded run's audit record, to check against the audit log on
  // the machine that made the recording (sandboxctl audit verify).
  const refs = [];
  for (const s of segs) {
    for (const te of s.events) {
      if (te.event.type === 'end') for (const a of te.event.end.audit || []) refs.push(a);
    }
  }
  const auditPanel = document.getElementById('p-audit');
  if (auditPanel && refs.length) {
    auditPanel.appendChild(el('h2', null, 'This recording\'s runs in the audit log'));
    auditPanel.appendChild(el('p', 'caption', 'Each run\'s record hash, as found in the audit log right after the run. On the machine that made the recording, sandboxctl audit verify checks the chain, and the hash is in it.'));
    const ul = el('ul', 'chain');
    for (const a of refs) {
      const li = el('li');
      li.appendChild(el('span', 'mono', a.run_id ? a.run_id.slice(0, 10) + '…' : '-'));
      const hs = el('span', 'hashes', a.hash ? 'hash ' + a.hash : 'no record: ' + (a.missing || '?'));
      li.appendChild(hs);
      ul.appendChild(li);
    }
    auditPanel.appendChild(ul);
  }
  buttons();
  MVM.setStatus(segs.length ? 'Recorded: ' + segs.length + (segs.length === 1 ? ' scenario' : ' scenarios') + '. Click one, or Play all.' : 'Nothing to play.');
})();
