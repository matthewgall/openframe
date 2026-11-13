import './style.css';
import { ApiError, changePassword, currentUser, login, logout, refreshMe } from './api';
import { h } from './components';
import { renderAlbums } from './views/albums';
import { renderAudit } from './views/audit';
import { renderDashboard } from './views/dashboard';
import { renderFrames } from './views/frames';
import { renderLibrary } from './views/library';
import { renderReleases } from './views/releases';
import { renderUsers } from './views/users';

// --- Section registry ---------------------------------------------------

interface Section {
  id: string;
  title: string;
  badge: string;
  adminOnly?: boolean;
  render: (el: HTMLElement) => void;
}

const sections: Section[] = [
  { id: 'dashboard', title: 'Dashboard', badge: 'live', render: renderDashboard },
  { id: 'library', title: 'Photo Library', badge: 'M2', render: renderLibrary },
  { id: 'albums', title: 'Albums', badge: 'M2', render: renderAlbums },
  { id: 'frames', title: 'Frames', badge: 'M2 registry', adminOnly: true, render: renderFrames },
  { id: 'users', title: 'Users', badge: 'M2', adminOnly: true, render: renderUsers },
  {
    id: 'audit',
    title: 'Audit log',
    badge: 'M2',
    adminOnly: true,
    render: renderAudit,
  },
  { id: 'releases', title: 'Releases', badge: 'M6', adminOnly: true, render: renderReleases },
];

// --- Login view ------------------------------------------------------------

function renderLogin(root: HTMLElement, onAuthed: () => void): void {
  const wrap = h('div', { class: 'login-wrap' });
  const card = h('div', { class: 'card login-card' });
  card.append(h('h1', {}, 'Open', h('span', { class: 'accent' }, 'Frame'), h('div', { class: 'login-sub' }, 'Sign in to manage your frames')));

  const email = h('input', { class: 'input', type: 'email', autocomplete: 'username', placeholder: 'email' });
  const password = h('input', { class: 'input', type: 'password', autocomplete: 'current-password', placeholder: 'password' });
  const errEl = h('div', { class: 'login-error', role: 'alert' });
  const submit = h('button', { class: 'btn primary', type: 'submit' }, 'Sign in');

  const form = h('form');
  form.append(
    h('div', { class: 'field' }, h('label', { for: 'login-email' }, 'Email'), email),
    h('div', { class: 'field' }, h('label', { for: 'login-pass' }, 'Password'), password),
    errEl,
    submit,
  );
  email.id = 'login-email';
  password.id = 'login-pass';

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    errEl.textContent = '';
    submit.disabled = true;
    try {
      await login(email.value.trim(), password.value);
      onAuthed();
    } catch (err) {
      const msg = err instanceof ApiError ? err.message : 'Sign-in failed.';
      errEl.textContent = msg;
      submit.disabled = false;
      password.focus();
    }
  });

  card.append(form);
  wrap.append(card);
  root.append(wrap);
  email.focus();
}

// --- Account (password change) ---------------------------------------------

function openAccountMenu(anchor: HTMLElement, onDone: () => void): void {
  const menu = h('div', { class: 'menu', role: 'menu' });
  const me = currentUser();
  menu.append(h('div', { class: 'menu-head muted' }, me?.email ?? ''));

  const changeItem = h('button', { class: 'menu-item', type: 'button' }, 'Change password…');
  const logoutItem = h('button', { class: 'menu-item danger', type: 'button' }, 'Sign out');

  changeItem.addEventListener('click', () => {
    menu.remove();
    const m = h('div', { class: 'modal-overlay', role: 'dialog', 'aria-modal': 'true', 'aria-label': 'Change password' });
    const dlg = h('div', { class: 'modal' });
    const body = h('div', { class: 'modal-body' });
    const cur = h('input', { class: 'input', type: 'password', placeholder: 'Current password', autocomplete: 'current-password' });
    const next = h('input', { class: 'input', type: 'password', placeholder: 'New password (min 10 chars)', autocomplete: 'new-password' });
    const confirm = h('input', { class: 'input', type: 'password', placeholder: 'Repeat new password', autocomplete: 'new-password' });
    const errEl = h('div', { class: 'login-error', role: 'alert' });
    const save = h('button', { class: 'btn primary', type: 'button' }, 'Change password');
    save.addEventListener('click', async () => {
      if (next.value !== confirm.value) {
        errEl.textContent = 'New passwords do not match.';
        return;
      }
      errEl.textContent = '';
      try {
        await changePassword(cur.value, next.value);
        m.remove();
        onDone();
      } catch (e) {
        errEl.textContent = String(e);
      }
    });
    body.append(
      h('div', { class: 'field' }, h('label', {}, 'Current password'), cur),
      h('div', { class: 'field' }, h('label', {}, 'New password'), next),
      h('div', { class: 'field' }, h('label', {}, 'Repeat new password'), confirm),
      errEl,
      h('div', { class: 'modal-actions' }, save),
    );
    dlg.append(h('div', { class: 'modal-head' }, h('h2', {}, 'Change password')), body);
    m.append(dlg);
    document.body.append(m);
    cur.focus();
  });

  logoutItem.addEventListener('click', async () => {
    menu.remove();
    await logout();
    onDone();
  });

  menu.append(changeItem, logoutItem);
  anchor.append(menu);
  const away = (e: MouseEvent): void => {
    if (!menu.contains(e.target as Node)) {
      menu.remove();
      document.removeEventListener('click', away);
    }
  };
  setTimeout(() => document.addEventListener('click', away), 0);
}

// --- Shell -------------------------------------------------------------------

function renderApp(root: HTMLElement, onUnauthed: () => void): void {
  const me = currentUser();

  const header = h('header');
  header.append(
    h('div', { class: 'logo' }, 'Open', h('span', {}, 'Frame')),
    h('div', { class: 'env' }, 'self-hosted · offline-first'),
  );
  const accountWrap = h('div', { class: 'account' });
  const acctBtn = h('button', { class: 'btn ghost acct', type: 'button' },
    me?.display_name || me?.email || 'account');
  acctBtn.addEventListener('click', () => openAccountMenu(accountWrap, onUnauthed));
  accountWrap.append(acctBtn);
  header.append(accountWrap);
  root.append(header);

  const layout = h('div', { class: 'layout' });
  const nav = h('nav', { 'aria-label': 'Main' });
  const main = h('main');
  layout.append(nav, main);
  root.append(layout);

  const show = (id: string): void => {
    const section = sections.find((s) => s.id === id);
    if (!section) return;
    if (section.adminOnly && me?.role !== 'admin') {
      main.replaceChildren(h('div', { class: 'banner error' }, 'Administrator access required.'));
      return;
    }
    main.replaceChildren();
    main.append(h('h1', {}, section.title));
    section.render(main);
    for (const a of nav.querySelectorAll('a')) {
      if (a.getAttribute('href') === `#${id}`) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    }
  };

  const isAdmin = me?.role === 'admin';
  for (const s of sections) {
    if (s.adminOnly && !isAdmin) continue;
    nav.append(h('a', { href: `#${s.id}` }, s.title, h('span', { class: 'badge' }, s.badge)));
  }

  nav.addEventListener('click', (e) => {
    const a = (e.target as HTMLElement).closest('a');
    if (!a) return;
    e.preventDefault();
    const id = a.getAttribute('href')?.slice(1);
    if (id) {
      history.replaceState(null, '', `#${id}`);
      show(id);
    }
  });

  const initial = location.hash.slice(1) || 'dashboard';
  show(sections.some((s) => s.id === initial) ? initial : 'dashboard');
}

// --- Root -------------------------------------------------------------------------

function main(): void {
  const app = document.getElementById('app');
  if (!app) return;

  const boot = async (): Promise<void> => {
    app.setAttribute('aria-busy', 'true');
    app.replaceChildren(h('div', { class: 'login-wrap' }, h('div', { class: 'muted' }, 'Checking session…')));
    const ok = await refreshMe();
    app.setAttribute('aria-busy', 'false');
    app.replaceChildren();
    if (ok) renderApp(app, () => void boot());
    else renderLogin(app, () => renderApp(app, () => void boot()));
  };

  void boot();
}

main();
