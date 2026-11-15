import {
  approvePairing,
  createDevice,
  getDevice,
  issueDeviceCredential,
  listDevices,
  listPairingPending,
  putDeviceConfig,
  rejectPairing,
  renameDevice,
  requestDeviceSync,
  revokeDevice,
  revokeDeviceCredential,
  type Device,
  type DeviceDetail,
  type DeviceStatus,
  type DeviceWithHealth,
  type FrameConfig,
  type PairingChallenge,
} from '../api';
import { banner, clear, confirmAction, emptyState, formatBytes, formatDateTime, h, kvRow, openModal, revealSecret, timeAgo } from '../components';

// statusMeta maps the server-derived device status to a pill class and
// label (spec §13: the UI must distinguish these states).
function statusMeta(s: DeviceStatus): { cls: string; label: string } {
  switch (s) {
    case 'online': return { cls: 'ok', label: 'online' };
    case 'recently_seen': return { cls: 'degraded', label: 'recently seen' };
    case 'offline': return { cls: '', label: 'offline' };
    case 'sync_error': return { cls: 'error', label: 'sync error' };
    case 'updating': return { cls: 'degraded', label: 'updating' };
    case 'update_failed': return { cls: 'error', label: 'update failed' };
    case 'rollback': return { cls: 'degraded', label: 'rollback' };
    default: return { cls: '', label: s };
  }
}

export function renderFrames(root: HTMLElement): void {
  const toolbar = h('div', { class: 'toolbar' });
  const newBtn = h('button', { class: 'btn primary', type: 'button' }, 'Register device');
  const refreshBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Refresh');
  // Revoked frames are terminal (no further credentials possible); they
  // hide by default so the page answers "what is my fleet", with the
  // toggle to audit the retired ones.
  let showRevoked = false;
  const showRevokedBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Show revoked');
  showRevokedBtn.addEventListener('click', () => {
    showRevoked = !showRevoked;
    showRevokedBtn.classList.toggle('active', showRevoked);
    void render();
  });
  toolbar.append(newBtn, refreshBtn, showRevokedBtn);
  root.append(toolbar);

  const pairingWrap = h('div', { class: 'card', style: 'margin-top:1rem' });
  root.append(pairingWrap);
  const devicesWrap = h('div', { class: 'table-wrap', style: 'margin-top:1rem' });
  root.append(devicesWrap);

  const render = async (): Promise<void> => {
    void renderPairing();
    clear(devicesWrap);
    try {
      const { devices } = await listDevices();
      clear(devicesWrap);
      if (devices.length === 0) {
        devicesWrap.append(
          emptyState({
            icon: 'frame',
            title: 'No frames yet',
            body: 'When a new frame starts up it appears under “Pending pairing” — approve it with the code shown on its screen.',
          }),
        );
        return;
      }
      const visible = showRevoked ? devices : devices.filter((d) => !d.revoked_at);
      if (visible.length === 0) {
        devicesWrap.append(
          emptyState({
            icon: 'frame',
            title: 'No active frames',
            body: `${devices.length} frame(s) are revoked (terminal — they can never sync again). Use “Show revoked” to see them.`,
          }),
        );
        return;
      }
      const table = h('table', { class: 'table' });
      const thead = h('thead');
      thead.append(h('tr', {},
        h('th', {}, 'Name'),
        h('th', {}, 'Status'),
        h('th', {}, 'Last seen'),
        h('th', {}, 'Version'),
        h('th', {}, 'Albums'),
        h('th', {}, 'Credential'),
        h('th', {}, 'Created'),
        h('th', {}, ''),
      ));
      const tbody = h('tbody');
      for (const d of visible) tbody.append(deviceRow(d, render));
      table.append(thead, tbody);
      devicesWrap.append(table);
    } catch (err) {
      clear(devicesWrap);
      devicesWrap.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
    }
  };

  // --- Pending pairing (M5, spec §12) ---------------------------------
  // A new frame displays a code; the administrator types the code here
  // to approve. The code proves the admin can see the physical frame.
  async function renderPairing(): Promise<void> {
    clear(pairingWrap);
    let challenges: PairingChallenge[] = [];
    try {
      ({ challenges } = await listPairingPending());
    } catch {
      // Pairing list is secondary; the device table still renders.
      pairingWrap.append(h('h2', {}, 'Pending pairing'), h('div', { class: 'muted' }, 'Could not load pending pairing requests.'));
      return;
    }
    if (challenges.length === 0) {
      pairingWrap.append(h('h2', {}, 'Pending pairing'),
        h('div', { class: 'muted' }, 'None — a new frame appears here (with a code on its screen) after its first boot.'));
      return;
    }
    pairingWrap.append(h('h2', {}, 'Pending pairing'));
    for (const c of challenges) pairingWrap.append(pairingRow(c, renderPairing));
  }

  function pairingRow(c: PairingChallenge, rerender: () => Promise<void>): HTMLElement {
    const row = h('div', { class: 'pairing-row' });
    const left = h('div', { class: 'pairing-info' });
    left.append(
      h('div', {}, h('strong', {}, c.device_name || 'Frame')),
      h('div', { class: 'muted mono' }, `${c.source_ip} · requested ${formatDateTime(c.created_at)} · expires ${formatDateTime(c.expires_at)}`),
    );
    const right = h('div', { class: 'pairing-actions' });
    const codeIn = h('input', {
      class: 'input mono pairing-code', type: 'text', placeholder: 'code on the frame',
      maxlength: '8', 'aria-label': `Pairing code for ${c.device_name}`,
    });
    const approve = h('button', { class: 'btn primary', type: 'button' }, 'Approve');
    const reject = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Reject');
    const errEl = h('div', { class: 'muted' });
    approve.addEventListener('click', async () => {
      try {
        const approval = await approvePairing(c.id, codeIn.value.trim());
        if (approval.credential) {
          // The frame claims the credential itself on its next poll;
          // reveal it here as the one-time record (manual fallback).
          const m = openModal(`Credential for ${c.device_name}`);
          revealSecret('Device credential (shown once)', approval.credential, m.body);
          m.body.append(h('div', { class: 'muted' },
            'The frame receives this credential automatically — the modal is the one-time record.'));
        }
        banner(root, 'success', `Paired "${c.device_name}".`);
        void rerender();
        void render();
      } catch (e) {
        errEl.textContent = String(e);
      }
    });
    reject.addEventListener('click', async () => {
      try {
        await rejectPairing(c.id);
        banner(root, 'info', `Rejected pairing for "${c.device_name}".`);
        void rerender();
      } catch (e) {
        errEl.textContent = String(e);
      }
    });
    right.append(codeIn, approve, reject, errEl);
    row.append(left, right);
    return row;
  }

  // --- Devices ----------------------------------------------------------
  newBtn.addEventListener('click', () => {
    const m = openModal('Register device (manual)');
    m.body.append(h('div', { class: 'muted' },
      'Manual registration for existing frames. New frames should use the pairing flow: boot the frame and approve it under "Pending pairing".'));
    const name = h('input', { class: 'input', type: 'text', placeholder: 'e.g. Living room (Pi 3B+)' });
    const errEl = h('div', { class: 'muted' });
    const save = h('button', { class: 'btn primary', type: 'button' }, 'Register');
    save.addEventListener('click', async () => {
      try {
        await createDevice(name.value.trim());
        m.close();
        void render();
      } catch (e) {
        errEl.textContent = String(e);
      }
    });
    m.body.append(h('div', { class: 'field' }, h('label', {}, 'Device name'), name), errEl, h('div', { class: 'modal-actions' }, save));
    name.focus();
  });

  refreshBtn.addEventListener('click', () => void render());
  void render();

  function deviceRow(d: DeviceWithHealth, rerender: () => Promise<void>): HTMLElement {
    const tr = h('tr');
    const revoked = !!d.revoked_at;
    const meta = statusMeta(d.status);
    const lastSeen = d.last_heartbeat_at ?? d.health?.last_sync_at;
    const version = d.health?.version ?? '—';
    // "recently seen" spans up to 24h; show the real age so a frame
    // down for hours doesn't read as freshly active.
    const statusLabel = d.status === 'recently_seen' && lastSeen
      ? `seen ${timeAgo(lastSeen)}`
      : meta.label;
    tr.append(
      h('td', {}, d.name),
      h('td', {}, h('span', { class: 'pill ' + meta.cls }, revoked ? 'revoked' : statusLabel)),
      h('td', { class: 'muted' }, formatDateTime(lastSeen)),
      h('td', { class: 'muted mono' }, version),
      h('td', {}, String(d.album_count)),
      h('td', {}, h('span', { class: 'pill ' + (d.has_credential ? 'ok' : 'degraded') }, d.has_credential ? 'issued' : 'none')),
      h('td', { class: 'muted' }, formatDateTime(d.created_at)),
    );
    const actions = h('td', { class: 'row-actions' });
    const manage = h('button', { class: 'btn ghost', type: 'button' }, 'Manage');
    manage.addEventListener('click', () => void openManage(d, rerender));
    actions.append(manage);
    if (!revoked) {
      const issue = h('button', { class: 'btn ghost', type: 'button' }, d.has_credential ? 'Rotate' : 'Issue credential');
      issue.addEventListener('click', () => {
        void (async () => {
          try {
            // Rotating an EXISTING credential is destructive: the frame's
            // current credential dies immediately and the new one is shown
            // only once — miss it and the frame is orphaned until someone
            // physically reaches it. Confirm; first-time issue doesn't
            // break anything and needs no gate.
            if (d.has_credential) {
              const yes = await confirmAction(
                `Rotate the credential for "${d.name}"? Its current credential stops working immediately — the frame stops syncing until you re-pair it with the new one. The new credential is shown only once.`,
                'Rotate credential',
              );
              if (!yes) return;
            }
            const cred = await issueDeviceCredential(d.id);
            credentialModal(cred.token, d);
          } catch (e) {
            banner(root, 'error', String(e));
          }
        })();
      });
      actions.append(issue);
      const revoke = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Revoke');
      revoke.addEventListener('click', async () => {
        const yes = await confirmAction(`Revoke "${d.name}"? Its credentials stop working immediately; re-pair to restore it.`, 'Revoke device');
        if (!yes) return;
        try {
          await revokeDevice(d.id);
          banner(root, 'success', `Device "${d.name}" revoked.`);
          void rerender();
        } catch (e) {
          banner(root, 'error', String(e));
        }
      });
      actions.append(revoke);
    }
    tr.append(actions);
    return tr;
  }

  // --- Manage (detail) panel --------------------------------------------
  async function openManage(d: DeviceWithHealth, rerender: () => Promise<void>): Promise<void> {
    const m = openModal(`Manage — ${d.name}`);
    const errEl = h('div', { class: 'muted' });
    const loading = h('div', { class: 'muted' }, 'Loading…');
    m.body.append(errEl, loading);

    try {
      const detail: DeviceDetail = await getDevice(d.id);
      loading.remove();
      m.body.append(healthPanel(detail), actionsPanel(detail, () => { m.close(); void rerender(); }));
      m.body.append(configPanel(detail, () => { m.close(); void rerender(); }));
    } catch (e) {
      loading.remove();
      errEl.textContent = String(e);
    }
  }

  function healthPanel(detail: DeviceDetail): HTMLElement {
    const wrap = h('div', { class: 'manage-section' });
    wrap.append(h('h3', {}, 'Health'));
    const kv = h('dl', { class: 'kv' });
    const h2 = detail.health;
    if (!h2) {
      kv.append(h('dd', { style: 'grid-column: 1 / -1' }, 'No health report yet — the frame has not reported since pairing.'));
    } else {
      kvRow(kv, 'Version', `${h2.version} (up ${formatDuration(h2.uptime_seconds)})`);
      kvRow(kv, 'Display', `${h2.display_driver} · ${h2.display_status}${h2.cpu_temp_c != null ? ` · ${h2.cpu_temp_c.toFixed(1)} °C` : ''}`);
      kvRow(kv, 'Last sync', h2.last_sync_ok
        ? `${formatDateTime(h2.last_sync_at)} (ok)`
        : `${formatDateTime(h2.last_sync_at)} (failed)`);
      kvRow(kv, 'Manifest', `rev ${h2.manifest_revision}, ${h2.pending_downloads} pending download(s)`);
      kvRow(kv, 'Storage', `${formatBytes(h2.storage_used)} / ${formatBytes(h2.storage_total)} (cache ${formatBytes(h2.cache_bytes)})`);
      kvRow(kv, 'Memory', `${h2.mem_used_mb} / ${h2.mem_total_mb} MB`);
      kvRow(kv, 'Update state', h2.update_state);
      if (h2.error_count_24h > 0 && h2.recent_errors.length > 0) {
        const errs = h('dd', {}, h2.recent_errors.map((e) => h('div', { class: 'muted' }, '· ' + e)).join(', '));
        kv.append(h('dt', {}, 'Recent errors'), errs);
      }
    }
    wrap.append(kv);
    return wrap;
  }

  function actionsPanel(detail: DeviceDetail, done: () => void): HTMLElement {
    const wrap = h('div', { class: 'manage-section' });
    wrap.append(h('h3', {}, 'Actions'));
    const row = h('div', { class: 'modal-actions', style: 'justify-content: flex-start' });
    const nameIn = h('input', { class: 'input', type: 'text', value: detail.device.name });
    const saveName = h('button', { class: 'btn ghost', type: 'button' }, 'Rename');
    saveName.addEventListener('click', async () => {
      try {
        await renameDevice(detail.device.id, nameIn.value.trim());
        banner(document.body, 'success', 'Renamed.');
        done();
      } catch (e) {
        banner(document.body, 'error', String(e));
      }
    });
    const syncBtn = h('button', { class: 'btn primary', type: 'button' }, 'Trigger sync now');
    syncBtn.addEventListener('click', async () => {
      try {
        await requestDeviceSync(detail.device.id);
        banner(document.body, 'success', 'Sync requested — the frame picks it up within ~30s.');
      } catch (e) {
        banner(document.body, 'error', String(e));
      }
    });
    const revokeCred = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Revoke credential');
    revokeCred.addEventListener('click', async () => {
      const yes = await confirmAction(`Revoke the credential for "${detail.device.name}"? The frame stops syncing immediately.`, 'Revoke credential');
      if (!yes) return;
      try {
        await revokeDeviceCredential(detail.device.id);
        banner(document.body, 'success', 'Credential revoked.');
        done();
      } catch (e) {
        banner(document.body, 'error', String(e));
      }
    });
    row.append(nameIn, saveName, syncBtn, revokeCred);
    wrap.append(row);
    return wrap;
  }

  // presetSelect builds a "human label" interval selector: common
  // presets with readable names, plus "Custom…" which reveals a
  // seconds input. 0 = frame default (left unset).
  function presetSelect(value: number, presets: Array<[number, string]>): {
    el: HTMLDivElement;
    get: () => number;
  } {
    const sel = h('select', { class: 'input' });
    sel.append(h('option', { value: '0' }, 'Frame default'));
    for (const [sec, label] of presets) sel.append(h('option', { value: String(sec) }, label));
    sel.append(h('option', { value: '-1' }, 'Custom…'));
    let init = '-1';
    if (value === 0) init = '0';
    else if (presets.some(([s]) => s === value)) init = String(value);
    sel.value = init;

    const customIn = h('input', { class: 'input', type: 'number', min: '2', max: '86400', value: value ? String(value) : '' });
    const customAttrs: Record<string, string> = { class: 'field custom-sec' };
    if (init === '-1') customAttrs.hidden = '';
    const customWrap = h('div', customAttrs,
      h('label', { class: 'muted' }, 'seconds (2–86400)'), customIn);
    sel.addEventListener('change', () => {
      if (sel.value === '-1') customWrap.removeAttribute('hidden');
      else customWrap.setAttribute('hidden', '');
    });
    const el = h('div', {}, sel, customWrap);
    const get = (): number => {
      const v = parseInt(sel.value, 10);
      if (v !== -1) return v;
      const c = parseInt(customIn.value, 10);
      return Number.isFinite(c) ? c : 0;
    };
    return { el, get };
  }

  function configPanel(detail: DeviceDetail, done: () => void): HTMLElement {
    const wrap = h('div', { class: 'manage-section' });
    wrap.append(h('h3', {}, 'Frame configuration'),
      h('div', { class: 'muted' }, 'Pushed to the frame with every sync (currently version ' + detail.config.version + '). "Frame default" means the frame uses its own setting — what you choose on its touchscreen wins.'));
    const c = detail.config.config;

    const group = (title: string, ...items: HTMLElement[]): HTMLElement =>
      h('div', { class: 'config-group' }, h('h4', {}, title), ...items);
    const sel = (value: string, opts: Array<[string, string]>): HTMLSelectElement => {
      const s = h('select', { class: 'input' });
      for (const [v, label] of opts) s.append(h('option', { value: v }, label));
      s.value = value;
      return s;
    };

    // --- Slideshow ---
    const SLIDESHOW_PRESETS: Array<[number, string]> = [
      [10, '10 seconds'], [30, '30 seconds'], [60, '1 minute'], [120, '2 minutes'],
      [300, '5 minutes'], [600, '10 minutes'], [900, '15 minutes'],
      [1800, '30 minutes'], [3600, '1 hour'],
    ];
    const interval = presetSelect(c.slideshow_interval_sec, SLIDESHOW_PRESETS);
    const transSel = sel(c.transition || '', [
      ['', 'Frame default'], ['none', 'None (hard cut)'],
      ['crossfade', 'Crossfade (soft)'], ['slide', 'Slide (pan)'],
    ]);
    const briDefault = h('input', { class: 'input', type: 'checkbox' });
    briDefault.checked = c.brightness == null;
    const briRange = h('input', { class: 'input range', type: 'range', min: '0', max: '100', step: '1' });
    briRange.value = String(c.brightness ?? 75);
    const briVal = h('span', { class: 'muted', style: 'min-width:4rem;text-align:right' });
    const briSync = (): void => {
      briRange.disabled = briDefault.checked;
      briVal.textContent = briDefault.checked ? 'frame default' : `${briRange.value}%`;
    };
    briDefault.addEventListener('change', briSync);
    briRange.addEventListener('input', briSync);
    briSync();
    const brightnessField = h('div', { class: 'field' },
      h('label', {}, 'Brightness'),
      h('div', { style: 'display:flex;align-items:center;gap:.6rem' }, briRange, briVal),
      h('label', { class: 'check-label' }, briDefault, h('span', { class: 'muted' }, 'use the frame\u2019s own brightness')));

    // --- Sleep schedule ---
    const sleepOff = h('input', { class: 'input', type: 'time', value: c.sleep_on || '' });
    const sleepOn = h('input', { class: 'input', type: 'time', value: c.sleep_off || '' });
    const sleepGroup = group('Sleep schedule',
      h('div', { class: 'form-grid' },
        field('Screen off (sleep)', sleepOff),
        field('Screen on (wake)', sleepOn)),
      h('div', { class: 'muted', style: 'margin-top:.4rem' }, 'Leave both blank to keep the frame\u2019s own schedule.'));

    // --- Display ---
    const oriSel = sel(c.orientation || '', [
      ['', 'Frame default'], ['normal', 'Normal'], ['rotate90', '90° clockwise'],
      ['rotate180', '180° (upside-down)'], ['rotate270', '270° clockwise'],
    ]);
    const clockBox = h('input', { class: 'input', type: 'checkbox', value: '1' });
    if (c.show_clock === true) clockBox.checked = true;
    const capBox = h('input', { class: 'input', type: 'checkbox', value: '1' });
    if (c.show_captions === true) capBox.checked = true;

    // --- Maintenance ---
    const SYNC_PRESETS: Array<[number, string]> = [
      [30, '30 seconds'], [60, '1 minute'], [300, '5 minutes'],
      [900, '15 minutes'], [1800, '30 minutes'], [3600, '1 hour'],
    ];
    const syncPoll = presetSelect(c.sync_interval_sec, SYNC_PRESETS);
    const chanSel = sel(c.update_channel || '', [
      ['', 'Frame default (stable)'], ['stable', 'Stable'],
      ['beta', 'Beta'], ['development', 'Development (first to get updates)'],
    ]);

    const errEl = h('div', { class: 'muted' });
    const save = h('button', { class: 'btn primary', type: 'button' }, 'Save configuration');
    save.addEventListener('click', async () => {
      const cfg: FrameConfig = {
        slideshow_interval_sec: interval.get(),
        transition: transSel.value,
        transition_duration_ms: c.transition_duration_ms,
        sync_interval_sec: syncPoll.get(),
        sleep_on: sleepOff.value,
        sleep_off: sleepOn.value,
        brightness: briDefault.checked ? null : parseInt(briRange.value, 10),
        orientation: oriSel.value,
        show_clock: triState(c.show_clock, clockBox.checked),
        show_captions: triState(c.show_captions, capBox.checked),
        update_channel: chanSel.value,
      };
      try {
        const res = await putDeviceConfig(detail.device.id, cfg);
        banner(document.body, 'success', `Saved (config v${res.config.version}) — the frame applies it on its next sync poll.`);
        done();
      } catch (e) {
        errEl.textContent = String(e);
      }
    });

    wrap.append(
      group('Slideshow',
        h('div', { class: 'form-grid' },
          field('Photo interval', interval.el),
          field('Transition', transSel)),
        brightnessField),
      sleepGroup,
      group('Display',
        h('div', { class: 'form-grid' },
          field('Screen orientation', oriSel),
          field('Clock', checkboxRow(clockBox, 'show the clock, top right'))),
        field('Captions', checkboxRow(capBox, 'show the photograph\u2019s caption'))),
      group('Maintenance',
        h('div', { class: 'form-grid' },
          field('Sync poll', syncPoll.el),
          field('Update channel', chanSel))),
      errEl,
      h('div', { class: 'modal-actions' }, save),
    );
    return wrap;
  }

  function field(label: string, input: HTMLElement): HTMLElement {
    return h('div', { class: 'field' }, h('label', {}, label), input);
  }

  function checkboxRow(box: HTMLInputElement, hint: string): HTMLElement {
    const lab = h('label', { class: 'check-label' }, box, h('span', { class: 'muted' }, hint));
    return lab;
  }

  // triState maps a tri-value setting (unset / on / off) to the wire
  // shape: null keeps the frame default.
  function triState(current: boolean | null, checked: boolean): boolean | null {
    if (current === null) return checked ? true : null;
    return checked;
  }
}

function formatDuration(sec: number): string {
  if (sec < 3600) return `${Math.floor(sec / 60)}m`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`;
  return `${Math.floor(sec / 86400)}d ${Math.floor((sec % 86400) / 3600)}h`;
}

// credentialModal shows the raw token exactly once (the server stores
// only its SHA-256).
function credentialModal(token: string, d: Device): void {
  const m = openModal(`Credential for ${d.name}`);
  const warn = h('div', { class: 'banner error' },
    'This token is shown only once and cannot be retrieved later. Copy it now.');
  m.body.append(warn);
  revealSecret('Device credential', token, m.body);
  const hint = h('div', { class: 'muted' },
    'For manual provisioning: set frame.device_token (or OPENFRAME_FRAME_DEVICE_TOKEN) on the frame to this value. Frames that paired through the console receive their credential automatically.');
  m.body.append(hint);
}
