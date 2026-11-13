import {
  approveRelease,
  deleteRelease,
  listReleases,
  pauseRelease,
  setReleaseRollout,
  uploadRelease,
  type ReleaseInfo,
  type ReleasePlatform,
  type ReleaseStatus,
} from '../api';
import { banner, clear, confirmAction, formatBytes, formatDateTime, h, openModal } from '../components';

// Releases view (M6, spec §14, ADR-0007): upload signed releases,
// approve/pause serving, expand staged rollouts, and delete. The
// server stores the signed metadata verbatim; frames verify it
// against their pinned keys — this UI never sees the signing key.

function statusMeta(s: ReleaseStatus): { cls: string; label: string } {
  switch (s) {
    case 'approved':
      return { cls: 'ok', label: 'approved' };
    case 'staged':
      return { cls: 'degraded', label: 'staged' };
    case 'paused':
      return { cls: 'error', label: 'paused' };
    default:
      return { cls: '', label: s };
  }
}

function platformText(p: ReleasePlatform): string {
  return `${p.channel} · ${p.os}/${p.arch}`;
}

export function renderReleases(root: HTMLElement): void {
  const uploadWrap = h('div', { class: 'card' });
  root.append(uploadWrap);
  const tableWrap = h('div', { class: 'table-wrap', style: 'margin-top:1rem' });
  root.append(tableWrap);

  renderUpload();
  void renderTable();

  // --- Upload ------------------------------------------------------------
  function renderUpload(): void {
    clear(uploadWrap);
    const errEl = h('div', { class: 'login-error', role: 'alert' });
    const okEl = h('div', { class: 'muted' });
    const submit = h('button', { class: 'btn primary', type: 'button' }, 'Upload (staged)');

    const version = h('input', { class: 'input', type: 'text', value: '' });
    const channel = h('select', { class: 'input' });
    for (const o of ['stable', 'beta', 'development']) channel.append(h('option', { value: o }, o));
    channel.value = 'stable';
    const osIn = h('input', { class: 'input', type: 'text', value: 'linux' });
    const arch = h('select', { class: 'input' });
    for (const o of ['arm64', 'amd64']) arch.append(h('option', { value: o }, o));
    arch.value = 'arm64';
    const rollout = h('input', { class: 'input', type: 'number', value: '100' });
    const metadata = h('textarea', {
      class: 'input',
      rows: '6',
      spellcheck: 'false',
    });
    metadata.placeholder = '{ "version": "1.4.0", "channel": "stable", "os": "linux", ..., "signature": "..." }';
    const file = h('input', { class: 'input', type: 'file' });

    submit.addEventListener('click', async () => {
      errEl.textContent = '';
      okEl.textContent = '';
      const md = metadata.value.trim();
      if (!md || !file.files || file.files.length === 0) {
        errEl.textContent = 'Both the signed metadata JSON and the artifact file are required.';
        return;
      }
      try {
        JSON.parse(md);
      } catch {
        errEl.textContent = 'Metadata is not valid JSON.';
        return;
      }
      submit.disabled = true;
      try {
        const rel = await uploadRelease(md, file.files[0]);
        okEl.textContent = `Uploaded ${rel.version} (${rel.channel}, ${rel.os}/${rel.arch}) as staged.`;
        metadata.value = '';
        file.value = '';
        void renderTable();
      } catch (e) {
        errEl.textContent = e instanceof Error ? e.message : String(e);
      } finally {
        submit.disabled = false;
      }
    });

    const grid = h('div', { class: 'form-grid' });
    grid.append(
      h('div', { class: 'field' }, h('label', {}, 'Version'), version),
      h('div', { class: 'field' }, h('label', {}, 'Channel'), channel),
      h('div', { class: 'field' }, h('label', {}, 'OS'), osIn),
      h('div', { class: 'field' }, h('label', {}, 'Arch'), arch),
      h('div', { class: 'field' }, h('label', {}, 'Rollout %'), rollout),
    );
    uploadWrap.append(
      h('h2', {}, 'Upload a signed release'),
      h('div', { class: 'muted', style: 'margin:.25rem 0 .75rem' },
        'Paste the complete signed release JSON produced by the release tooling (including ',
        h('code', {}, 'signature'),
        ' and signer key id) and attach the release binary. Uploads land as ',
        h('strong', {}, 'staged'),
        ' — invisible to frames until approved.',
      ),
      grid,
      h('div', { class: 'field' }, h('label', {}, 'Signed metadata (JSON)'), metadata),
      h('div', { class: 'field' }, h('label', {}, 'Artifact (binary)'), file),
      errEl,
      okEl,
      h('div', { class: 'row' }, submit),
    );
  }

  // --- Table -------------------------------------------------------------
  async function renderTable(): Promise<void> {
    clear(tableWrap);
    let releases: ReleaseInfo[];
    try {
      releases = await listReleases();
    } catch (err) {
      tableWrap.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
      return;
    }
    if (releases.length === 0) {
      tableWrap.append(h('div', { class: 'muted' }, 'No releases uploaded yet. Upload a signed release above.'));
      return;
    }
    const table = h('table', { class: 'table' });
    const thead = h('thead');
    thead.append(
      h('tr', {},
        h('th', {}, 'Version'),
        h('th', {}, 'Platform'),
        h('th', {}, 'Status'),
        h('th', {}, 'Rollout'),
        h('th', {}, 'Signer'),
        h('th', {}, 'Expires'),
        h('th', {}, 'Artifact'),
        h('th', {}, ''),
      ),
    );
    const tbody = h('tbody');
    for (const r of releases) tbody.append(releaseRow(r, renderTable));
    table.append(thead, tbody);
    tableWrap.append(table);
  }

  function releaseRow(r: ReleaseInfo, rerender: () => Promise<void>): HTMLTableRowElement {
    const tr = h('tr');
    const p: ReleasePlatform = { channel: r.channel, os: r.os, arch: r.arch };
    const sm = statusMeta(r.status);
    const statusPill = h('span', { class: `pill ${sm.cls}` },
      sm.label + (r.is_latest ? ' · latest' : '') + (r.is_rollback ? ' · rollback' : ''));
    tr.append(h('td', {}, h('strong', {}, r.version)));
    tr.append(h('td', {}, platformText(p)));
    tr.append(h('td', {}, statusPill));
    tr.append(h('td', {}, `${r.rollout_percent}%${r.rollout_devices.length ? ` (+${r.rollout_devices.length} named)` : ''}`));
    tr.append(h('td', { class: 'muted' }, r.signer_key_id || '—'));
    tr.append(h('td', {}, formatDateTime(r.expires_at)));
    tr.append(h('td', {}, r.has_artifact ? formatBytes(r.artifact_size) : h('span', { class: 'pill error' }, 'missing')));

    const actions = h('div', { class: 'row-actions' });
    if (r.status === 'staged') {
      const approve = h('button', { class: 'btn primary', type: 'button' }, 'Approve');
      approve.addEventListener('click', async () => {
        const ok = await confirmAction(
          `Approve ${r.version} for ${platformText(p)}? It immediately becomes the latest release for this platform.`,
          'Approve',
        );
        if (!ok) return;
        try {
          await approveRelease(p, r.version);
          banner(root, 'success', `Approved ${r.version}.`);
        } catch (e) {
          banner(root, 'error', e instanceof Error ? e.message : String(e));
        }
        void rerender();
      });
      actions.append(approve);
    }
    if (r.status === 'approved') {
      const pause = h('button', { class: 'btn', type: 'button' }, 'Pause');
      pause.addEventListener('click', async () => {
        const ok = await confirmAction(
          `Pause ${r.version}? Frames will stop receiving it and the latest pointer moves to the next approved release.`,
          'Pause',
        );
        if (!ok) return;
        try {
          await pauseRelease(p, r.version);
          banner(root, 'success', `Paused ${r.version}.`);
        } catch (e) {
          banner(root, 'error', e instanceof Error ? e.message : String(e));
        }
        void rerender();
      });
      actions.append(pause);
    }
    const expand = h('button', { class: 'btn ghost', type: 'button' }, 'Rollout…');
    expand.addEventListener('click', () => openRollout(r, rerender));
    actions.append(expand);
    if (r.status !== 'approved') {
      const del = h('button', { class: 'btn danger', type: 'button' }, 'Delete');
      del.addEventListener('click', async () => {
        const ok = await confirmAction(
          `Delete ${r.version} (${platformText(p)}) and its artifact? This cannot be undone.`,
        );
        if (!ok) return;
        try {
          await deleteRelease(p, r.version);
          banner(root, 'success', `Deleted ${r.version}.`);
        } catch (e) {
          banner(root, 'error', e instanceof Error ? e.message : String(e));
        }
        void rerender();
      });
      actions.append(del);
    }
    tr.append(h('td', {}, actions));
    return tr;
  }

  // --- Rollout modal -------------------------------------------------------
  function openRollout(r: ReleaseInfo, rerender: () => Promise<void>): void {
    const m = openModal(`Rollout — ${r.version}`);
    const p: ReleasePlatform = { channel: r.channel, os: r.os, arch: r.arch };
    const pct = h('input', { class: 'input', type: 'number', min: '0', max: '100', value: String(r.rollout_percent) });
    const devices = h('textarea', {
      class: 'input',
      rows: '3',
      spellcheck: 'false',
      value: r.rollout_devices.join('\n'),
      placeholder: 'one device id per line (always included)',
    });
    const errEl = h('div', { class: 'login-error', role: 'alert' });
    const save = h('button', { class: 'btn primary', type: 'button' }, 'Save');
    m.body.append(
      h('div', { class: 'field' }, h('label', {}, 'Percentage (0–100)'), pct),
      h('div', { class: 'field' }, h('label', {}, 'Named devices (allow-list)'), devices),
      h('div', { class: 'muted', style: 'margin-bottom:.75rem' },
        'The serving rollout is an unsigned operator override: it changes ',
        h('em', {}, 'who'),
        ' receives this release, never ',
        h('em', {}, 'what'),
        ' — the artifact and its signed digest are fixed.',
      ),
      errEl,
      h('div', { class: 'row' }, save),
    );
    save.addEventListener('click', async () => {
      const percent = Number(pct.value);
      if (!Number.isInteger(percent) || percent < 0 || percent > 100) {
        errEl.textContent = 'Percentage must be an integer 0–100.';
        return;
      }
      const list = devices.value.split('\n').map((s) => s.trim()).filter(Boolean);
      save.disabled = true;
      errEl.textContent = '';
      try {
        await setReleaseRollout(p, r.version, percent, list);
        m.close();
        banner(root, 'success', `Rollout for ${r.version} set to ${percent}%.`);
        void rerender();
      } catch (e) {
        errEl.textContent = e instanceof Error ? e.message : String(e);
        save.disabled = false;
      }
    });
  }
}
