import './style.css';
import { getHealth, type Health } from './api';

// --- DOM helper --------------------------------------------------------

function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Record<string, string> = {},
  ...children: Array<Node | string>
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  for (const c of children) node.append(c);
  return node;
}

function kvRow(parent: HTMLElement, k: string, v: string): void {
  parent.append(h('dt', {}, k), h('dd', {}, v));
}

// --- Section registry ---------------------------------------------------
// Sections land in their milestone; until then they render a clear
// placeholder. The nav is final now so the information architecture is
// stable while the backend grows.

interface Section {
  id: string;
  title: string;
  milestone: string;
  render: (el: HTMLElement) => void;
}

function placeholder(el: HTMLElement, title: string, milestone: string, detail: string): void {
  const card = h('div', { class: 'card placeholder' });
  card.append(
    h('div', { class: 'milestone' }, `${title} — arrives in ${milestone}`),
    h('div', {}, detail),
  );
  el.append(card);
}

function formatUptime(sec: number): string {
  const d = Math.floor(sec / 86400);
  const hr = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}d ${hr}h ${m}m`;
  if (hr > 0) return `${hr}h ${m}m`;
  return `${m}m ${sec % 60}s`;
}

function renderDashboard(root: HTMLElement): void {
  const banner = h('div', { class: 'banner' });
  banner.textContent =
    'Milestone 1 — foundation. The server, frame agent, configuration, migrations and this UI shell are live. Photo management arrives in Milestone 2.';
  root.append(banner);

  const grid = h('div', { class: 'grid' });

  // Health card (live).
  const healthCard = h('div', { class: 'card' });
  healthCard.append(h('h2', {}, 'Server health'));
  const status = h('span', { class: 'pill' }, 'loading…');
  const healthKv = h('dl', { class: 'kv' });
  healthCard.append(status, healthKv);

  // Build card (static).
  const buildCard = h('div', { class: 'card' });
  buildCard.append(h('h2', {}, 'Build'));
  const buildKv = h('dl', { class: 'kv' });
  buildCard.append(buildKv);

  grid.append(healthCard, buildCard);
  root.append(grid);

  const refresh = async (): Promise<void> => {
    try {
      const data: Health = await getHealth();
      status.replaceChildren();
      status.className = 'pill ' + (data.status === 'ok' ? 'ok' : 'degraded');
      status.textContent = data.status === 'ok' ? 'healthy' : 'degraded';
      buildKv.replaceChildren();
      kvRow(buildKv, 'Version', data.version);
      kvRow(buildKv, 'Commit', data.commit);
      kvRow(buildKv, 'Server time', data.time);
      healthKv.replaceChildren();
      kvRow(healthKv, 'Database', data.db.status === 'ok' ? 'ok' : `error: ${data.db.detail ?? 'unknown'}`);
      kvRow(healthKv, 'Uptime', formatUptime(data.uptime_seconds));
      kvRow(healthKv, 'Started', data.started_at);
    } catch (err) {
      status.replaceChildren();
      status.className = 'pill error';
      status.textContent = 'unreachable';
      healthKv.replaceChildren();
      kvRow(healthKv, 'Error', String(err));
    }
  };

  void refresh();
  const timer = setInterval(() => void refresh(), 15_000);
  // Stop polling once the dashboard is unmounted (no unnecessary redraws).
  const obs = new MutationObserver(() => {
    if (!document.body.contains(healthCard)) {
      clearInterval(timer);
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });
}

const sections: Section[] = [
  { id: 'dashboard', title: 'Dashboard', milestone: 'M1', render: renderDashboard },
  {
    id: 'library',
    title: 'Photo Library',
    milestone: 'Milestone 2',
    render: (root) =>
      placeholder(root, 'Photo Library', 'Milestone 2', 'Uploads, derivatives, captions, search and duplicate detection.'),
  },
  {
    id: 'albums',
    title: 'Albums',
    milestone: 'Milestone 2',
    render: (root) => placeholder(root, 'Albums', 'Milestone 2', 'Shared and private albums, covers, ordering, assignment to frames.'),
  },
  {
    id: 'frames',
    title: 'Frames',
    milestone: 'Milestones 3 & 5',
    render: (root) => placeholder(root, 'Frames', 'Milestones 3 & 5', 'Registration, pairing, assignments, sync controls, health and update policies.'),
  },
  {
    id: 'users',
    title: 'Users',
    milestone: 'Milestone 2',
    render: (root) => placeholder(root, 'Users', 'Milestone 2', 'Family accounts, roles, invitations and recovery.'),
  },
  {
    id: 'releases',
    title: 'Releases',
    milestone: 'Milestone 6',
    render: (root) =>
      placeholder(root, 'Releases', 'Milestone 6', 'Signed releases, channels, rollout progress, deployments and rollbacks.'),
  },
];

// --- Shell ---------------------------------------------------------------

function main(): void {
  const app = document.getElementById('app');
  if (!app) return;
  app.setAttribute('aria-busy', 'false');
  app.replaceChildren();

  const header = h('header');
  header.append(
    h('div', { class: 'logo' }, 'Open', h('span', {}, 'Frame')),
    h('div', { class: 'env' }, 'self-hosted · offline-first'),
  );
  app.append(header);

  const layout = h('div', { class: 'layout' });
  const nav = h('nav', { 'aria-label': 'Main' });
  const main = h('main');
  layout.append(nav, main);
  app.append(layout);

  const show = (id: string): void => {
    const section = sections.find((s) => s.id === id);
    if (!section) return;
    main.replaceChildren();
    main.append(h('h1', {}, section.title));
    section.render(main);
    for (const a of nav.querySelectorAll('a')) {
      if (a.getAttribute('href') === `#${id}`) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    }
  };

  for (const s of sections) {
    nav.append(
      h('a', { href: `#${s.id}` }, s.title, h('span', { class: 'badge' }, s.milestone)),
    );
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

main();
