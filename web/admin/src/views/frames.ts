import { createDevice, listDevices, renameDevice, revokeDevice, type Device } from '../api';
import { banner, clear, confirmAction, formatDateTime, h, openModal } from '../components';

export function renderFrames(root: HTMLElement): void {
  const notice = h('div', { class: 'banner' },
    'Milestone 2 shows the device registry used for album→frame assignment. Pairing, credentials, sync controls and health appear in Milestone 5.');
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
        h('th', {}, 'Name'), h('th', {}, 'Albums'), h('th', {}, 'Status'), h('th', {}, 'Created'), h('th', {}, ''),
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
      h('td', {}, String(d.album_count)),
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
}
