import { createDevice, issueDeviceCredential, listDevices, renameDevice, revokeDevice, revokeDeviceCredential, type Device } from '../api';
import { banner, clear, confirmAction, formatDateTime, h, openModal } from '../components';

export function renderFrames(root: HTMLElement): void {
  const notice = h('div', { class: 'banner' },
    'Devices sync to the frame via an issued credential. Issue a credential, copy it once, and set it on the frame (frame.device_token / OPENFRAME_FRAME_DEVICE_TOKEN). Health and remote controls arrive in Milestone 5.');
  root.append(notice);

  const toolbar = h('div', { class: 'toolbar' });
  const newBtn = h('button', { class: 'btn primary', type: 'button' }, 'Register device');
  const refreshBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Refresh');
  toolbar.append(newBtn, refreshBtn);
  root.append(toolbar);

  const tableWrap = h('div', { class: 'table-wrap' });
  root.append(tableWrap);

  const render = async (): Promise<void> => {
    clear(tableWrap);
    try {
      const { devices } = await listDevices();
      clear(tableWrap);
      if (devices.length === 0) {
        tableWrap.append(h('div', { class: 'muted' }, 'No devices registered.'));
        return;
      }
      const table = h('table', { class: 'table' });
      const thead = h('thead');
      thead.append(h('tr', {},
        h('th', {}, 'Name'), h('th', {}, 'ID'), h('th', {}, 'Albums'), h('th', {}, 'Credential'), h('th', {}, 'Status'), h('th', {}, 'Created'), h('th', {}, ''),
      ));
      const tbody = h('tbody');
      for (const d of devices) tbody.append(deviceRow(d, render));
      table.append(thead, tbody);
      tableWrap.append(table);
    } catch (err) {
      clear(tableWrap);
      tableWrap.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
    }
  };

  newBtn.addEventListener('click', () => {
    const m = openModal('Register device');
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

  function deviceRow(d: Device, rerender: () => Promise<void>): HTMLElement {
    const tr = h('tr');
    const revoked = !!d.revoked_at;
    tr.append(
      h('td', {}, d.name),
      h('td', { class: 'mono muted', title: d.id }, d.id.slice(0, 8) + '…'),
      h('td', {}, String(d.album_count)),
      h('td', {}, h('span', { class: 'pill ' + (d.has_credential ? 'ok' : 'degraded') }, d.has_credential ? 'issued' : 'none')),
      h('td', {}, h('span', { class: 'pill ' + (revoked ? 'error' : 'ok') }, revoked ? 'revoked' : 'active')),
      h('td', { class: 'muted' }, formatDateTime(d.created_at)),
    );
    const actions = h('td', { class: 'row-actions' });
    const rename = h('button', { class: 'btn ghost', type: 'button' }, 'Rename');
    rename.addEventListener('click', () => {
      const m2 = openModal('Rename device');
      const nameIn = h('input', { class: 'input', type: 'text', value: d.name });
      const errEl = h('div', { class: 'muted' });
      const save = h('button', { class: 'btn primary', type: 'button' }, 'Save');
      save.addEventListener('click', async () => {
        try {
          await renameDevice(d.id, nameIn.value.trim());
          m2.close();
          void rerender();
        } catch (e) {
          errEl.textContent = String(e);
        }
      });
      m2.body.append(h('div', { class: 'field' }, h('label', {}, 'Name'), nameIn), errEl, h('div', { class: 'modal-actions' }, save));
    });
    actions.append(rename);
    if (!revoked) {
      const issue = h('button', { class: 'btn ghost', type: 'button' }, d.has_credential ? 'Rotate credential' : 'Issue credential');
      issue.addEventListener('click', () => {
        void (async () => {
          try {
            const cred = await issueDeviceCredential(d.id);
            credentialModal(cred.token, d);
          } catch (e) {
            banner(root, 'error', String(e));
          }
        })();
      });
      actions.append(issue);
      if (d.has_credential) {
        const revokeCred = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Revoke credential');
        revokeCred.addEventListener('click', async () => {
          const yes = await confirmAction(`Revoke the credential for "${d.name}"? The frame stops syncing immediately.`, 'Revoke credential');
          if (!yes) return;
          try {
            await revokeDeviceCredential(d.id);
            banner(root, 'success', `Credential for "${d.name}" revoked.`);
            void rerender();
          } catch (e) {
            banner(root, 'error', String(e));
          }
        });
        actions.append(revokeCred);
      }
      const revoke = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Revoke');
      revoke.addEventListener('click', async () => {
        const yes = await confirmAction(`Revoke credentials for "${d.name}"? The frame will be unable to sync until re-paired (M5).`, 'Revoke');
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

  // credentialModal shows the raw token exactly once. The server stores
  // only its SHA-256, so closing this modal without copying it means
  // the token is lost (rotate to get a new one).
  function credentialModal(token: string, d: Device): void {
    const m = openModal(`Credential for ${d.name}`);
    const warn = h('div', { class: 'banner error' },
      'This token is shown only once and cannot be retrieved later. Copy it now.');
    const row = h('div', { class: 'modal-actions' });
    const tokenEl = h('input', { class: 'input mono', type: 'text', readonly: '', value: token });
    row.append(tokenEl);
    const copy = h('button', { class: 'btn primary', type: 'button' }, 'Copy');
    copy.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(token);
        copy.textContent = 'Copied';
      } catch {
        tokenEl.select();
      }
    });
    row.append(copy);
    const hint = h('div', { class: 'muted' },
      'On the frame, set frame.device_token (or OPENFRAME_FRAME_DEVICE_TOKEN) to this value and restart openframe frame.');
    m.body.append(warn, row, hint);
    tokenEl.focus();
    tokenEl.select();
  }
}
