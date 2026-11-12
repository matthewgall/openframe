import {
  createUser,
  listUsers,
  resetUserPassword,
  revokeUserSessions,
  updateUser,
  type User,
} from '../api';
import { banner, clear, confirmAction, formatDateTime, h, openModal, revealSecret } from '../components';

export function renderUsers(root: HTMLElement): void {
  const toolbar = h('div', { class: 'toolbar' });
  const newBtn = h('button', { class: 'btn primary', type: 'button' }, 'New user');
  const refreshBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Refresh');
  toolbar.append(newBtn, refreshBtn);
  root.append(toolbar);

  const tableWrap = h('div', { class: 'table-wrap' });
  root.append(tableWrap);

  const render = async (): Promise<void> => {
    clear(tableWrap);
    tableWrap.append(h('div', { class: 'muted' }, 'Loading…'));
    try {
      const { users } = await listUsers(false);
      clear(tableWrap);
      const table = h('table', { class: 'table' });
      const thead = h('thead');
      thead.append(
        h('tr', {},
          h('th', {}, 'Email'),
          h('th', {}, 'Name'),
          h('th', {}, 'Role'),
          h('th', {}, 'Status'),
          h('th', {}, 'Created'),
          h('th', {}, ''),
        ),
      );
      const tbody = h('tbody');
      for (const u of users) tbody.append(userRow(u, render));
      table.append(thead, tbody);
      tableWrap.append(table);
    } catch (err) {
      clear(tableWrap);
      tableWrap.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
    }
  };

  newBtn.addEventListener('click', async () => {
    const m = openModal('New user');
    const email = h('input', { class: 'input', type: 'email', placeholder: 'name@example.com' });
    const name = h('input', { class: 'input', type: 'text', placeholder: 'Display name (optional)' });
    const role = h('select', { class: 'input' },
      h('option', { value: 'member' }, 'Member — can view shared albums and upload'),
      h('option', { value: 'admin' }, 'Admin — full control'),
    );
    const errEl = h('div', { class: 'muted' });
    const secretHost = h('div');
    const save = h('button', { class: 'btn primary', type: 'button' }, 'Create user');
    save.addEventListener('click', async () => {
      try {
        save.disabled = true;
        const res = await createUser({
          email: email.value.trim(),
          display_name: name.value.trim(),
          role: role.value,
        });
        clear(secretHost);
        revealSecret('Temporary password (share with the user, they change it at first login):', res.temporary_password, secretHost);
      } catch (e) {
        errEl.textContent = String(e);
      } finally {
        save.disabled = false;
      }
    });
    m.body.append(
      h('div', { class: 'field' }, h('label', {}, 'Email'), email),
      h('div', { class: 'field' }, h('label', {}, 'Display name'), name),
      h('div', { class: 'field' }, h('label', {}, 'Role'), role),
      errEl,
      secretHost,
      h('div', { class: 'modal-actions' }, save),
    );
    email.focus();
  });

  refreshBtn.addEventListener('click', () => void render());
  void render();

  function userRow(u: User, rerender: () => Promise<void>): HTMLElement {
    const tr = h('tr');
    const deactivated = !!u.deactivated_at;
    tr.append(
      h('td', {}, u.email),
      h('td', {}, u.display_name),
      h('td', {}, h('span', { class: 'pill ' + (u.role === 'admin' ? 'ok' : '') }, u.role)),
      h('td', {}, h('span', { class: 'pill ' + (deactivated ? 'error' : 'ok') }, deactivated ? 'deactivated' : 'active')),
      h('td', { class: 'muted' }, formatDateTime(u.created_at)),
    );

    const actions = h('td', { class: 'row-actions' });

    const edit = h('button', { class: 'btn ghost', type: 'button' }, 'Edit');
    edit.addEventListener('click', () => {
      const m2 = openModal(`Edit ${u.email}`);
      const nameIn = h('input', { class: 'input', type: 'text', value: u.display_name });
      const roleIn = h('select', { class: 'input' },
        h('option', { value: 'member' }, 'Member'),
        h('option', { value: 'admin' }, 'Admin'),
      );
      roleIn.value = u.role;
      const errEl = h('div', { class: 'muted' });
      const save = h('button', { class: 'btn primary', type: 'button' }, 'Save');
      save.addEventListener('click', async () => {
        try {
          await updateUser(u.id, { display_name: nameIn.value.trim(), role: roleIn.value });
          m2.close();
          void rerender();
        } catch (e) {
          errEl.textContent = String(e);
        }
      });
      m2.body.append(
        h('div', { class: 'field' }, h('label', {}, 'Display name'), nameIn),
        h('div', { class: 'field' }, h('label', {}, 'Role'), roleIn),
        errEl,
        h('div', { class: 'modal-actions' }, save),
      );
    });
    actions.append(edit);

    const reset = h('button', { class: 'btn ghost', type: 'button' }, 'Reset password');
    reset.addEventListener('click', async () => {
      const yes = await confirmAction(
        `Reset the password for ${u.email}? All of their sessions are revoked and a new temporary password is generated.`,
        'Reset',
      );
      if (!yes) return;
      try {
        const res = await resetUserPassword(u.id);
        const m2 = openModal('Password reset');
        revealSecret('Temporary password for ' + u.email + ':', res.temporary_password, m2.body);
        banner(root, 'success', `Password reset for ${u.email}; sessions revoked.`);
        void rerender();
      } catch (e) {
        banner(root, 'error', String(e));
      }
    });
    actions.append(reset);

    const sessions = h('button', { class: 'btn ghost', type: 'button' }, 'Revoke sessions');
    sessions.addEventListener('click', async () => {
      try {
        const res = await revokeUserSessions(u.id);
        banner(root, 'success', `Revoked ${res.revoked} session${res.revoked === 1 ? '' : 's'} for ${u.email}.`);
      } catch (e) {
        banner(root, 'error', String(e));
      }
    });
    actions.append(sessions);

    const toggle = h('button', { class: 'btn ghost ' + (deactivated ? '' : 'danger-text'), type: 'button' },
      deactivated ? 'Reactivate' : 'Deactivate');
    toggle.addEventListener('click', async () => {
      const verb = deactivated ? 'Reactivate' : 'Deactivate';
      const yes = await confirmAction(
        deactivated
          ? `Reactivate ${u.email}? They will be able to sign in again.`
          : `Deactivate ${u.email}? They will be signed out and unable to sign in.`,
        verb,
      );
      if (!yes) return;
      try {
        await updateUser(u.id, { deactivated: !deactivated });
        void rerender();
      } catch (e) {
        banner(root, 'error', String(e));
      }
    });
    actions.append(toggle);

    tr.append(actions);
    return tr;
  }
}
