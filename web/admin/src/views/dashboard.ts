import { getHealth, getStats, photoFileURL, type Stats } from '../api';
import { clear, formatBytes, formatDateTime, h, kvRow } from '../components';

export function renderDashboard(root: HTMLElement): void {
  const statsGrid = h('div', { class: 'stat-grid' });
  const cards: Array<[string, string]> = [
    ['Photos', '—'],
    ['Storage', '—'],
    ['Albums', '—'],
    ['Active users', '—'],
    ['Devices', '—'],
  ];
  const cardEls = cards.map(([label]) => {
    const c = h('div', { class: 'card stat' });
    c.append(
      h('h2', {}, label),
      h('div', { class: 'stat-value' }, '—'),
    );
    statsGrid.append(c);
    return c.querySelector('.stat-value')!;
  });

  root.append(statsGrid);

  const recentWrap = h('div', { class: 'card', style: 'margin-top:1rem' });
  recentWrap.append(h('h2', {}, 'Recent uploads'));
  const recentGrid = h('div', { class: 'photo-grid' });
  recentWrap.append(recentGrid);
  root.append(recentWrap);

  const healthCard = h('div', { class: 'card', style: 'margin-top:1rem' });
  healthCard.append(h('h2', {}, 'Server'));
  const healthKv = h('dl', { class: 'kv' });
  healthCard.append(healthKv);
  root.append(healthCard);

  const refresh = async (): Promise<void> => {
    try {
      const [stats, health] = await Promise.all([getStats(), getHealth()]);
      const s = stats as Stats;
      cardEls[0].textContent = String(s.photos.total);
      cardEls[1].textContent = formatBytes(s.photos.storage_bytes);
      cardEls[2].textContent = `${s.albums.total} (${s.albums.shared} shared)`;
      cardEls[3].textContent = String(s.users.active);
      cardEls[4].textContent = String(s.devices.total);

      clear(recentGrid);
      const recent = (s.photos.recent ?? []) as unknown as Array<{
        id: string;
        name: string;
        uploaded_by_name: string;
        uploaded_at: string;
      }>;
      if (recent.length === 0) {
        recentGrid.append(h('div', { class: 'muted' }, 'No photographs yet.'));
      }
      for (const p of recent) {
        const item = h('figure', { class: 'photo-tile' });
        item.append(
          h('img', {
            src: photoFileURL(p.id, 'thumb'),
            alt: p.name,
            loading: 'lazy',
          }),
          h('figcaption', {}, `${p.name} — ${p.uploaded_by_name ?? '?'} · ${formatDateTime(p.uploaded_at)}`),
        );
        recentGrid.append(item);
      }

      clear(healthKv);
      kvRow(healthKv, 'Status', health.status === 'ok' ? 'healthy' : 'degraded');
      kvRow(healthKv, 'Version', health.version);
      kvRow(healthKv, 'Commit', health.commit);
      kvRow(healthKv, 'Hostname', s.server.hostname);
      kvRow(healthKv, 'Database', health.db.status === 'ok' ? 'ok' : health.db.detail ?? 'error');
      kvRow(healthKv, 'Started', formatDateTime(health.started_at));
    } catch (err) {
      clear(recentGrid);
      recentGrid.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
    }
  };

  void refresh();
  const timer = setInterval(() => void refresh(), 30_000);
  const obs = new MutationObserver(() => {
    if (!document.body.contains(root)) {
      clearInterval(timer);
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });
}
