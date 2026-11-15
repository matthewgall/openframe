import { getHealth, getStats, listDevices, photoFileURL, type DeviceStatus, type Stats } from '../api';
import { clear, emptyState, formatBytes, formatDateTime, h, kvRow } from '../components';

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
    const value = h('div', { class: 'stat-value' }, '—');
    const sub = h('div', { class: 'stat-sub', hidden: '' });
    c.append(h('h2', {}, label), value, sub);
    statsGrid.append(c);
    return { value, sub };
  });

  root.append(statsGrid);

  const recentWrap = h('div', { class: 'card', style: 'margin-top:1rem' });
  recentWrap.append(h('h2', {}, 'Recent uploads'));
  const recentGrid = h('div', { class: 'photo-grid' });
  recentWrap.append(recentGrid);
  root.append(recentWrap);

  const framesCard = h('div', { class: 'card', style: 'margin-top:1rem' });
  framesCard.append(h('h2', {}, 'Frames'));
  const framesKv = h('dl', { class: 'kv' });
  framesCard.append(framesKv);
  root.append(framesCard);

  const healthCard = h('div', { class: 'card', style: 'margin-top:1rem' });
  healthCard.append(h('h2', {}, 'Server'));
  const healthKv = h('dl', { class: 'kv' });
  healthCard.append(healthKv);
  root.append(healthCard);

  const refresh = async (): Promise<void> => {
    try {
      const [stats, health, dev] = await Promise.all([getStats(), getHealth(), listDevices().catch(() => null)]);
      const s = stats as Stats;
      // Headline numbers are LIVE resources; trash/revoked counts are
      // sub-lines so a glance answers "what works right now".
      cardEls[0].value.textContent = String(s.photos.total);
      cardEls[0].sub.textContent = s.photos.deleted > 0 ? `${s.photos.deleted} in trash` : '';
      cardEls[0].sub.hidden = s.photos.deleted === 0;
      cardEls[1].value.textContent = formatBytes(s.photos.storage_bytes);
      cardEls[2].value.textContent = `${s.albums.total} (${s.albums.shared} shared)`;
      cardEls[3].value.textContent = String(s.users.active);
      cardEls[4].value.textContent = String(s.devices.total);
      cardEls[4].sub.textContent = (s.devices.revoked ?? 0) > 0 ? `${s.devices.revoked} revoked` : '';
      cardEls[4].sub.hidden = (s.devices.revoked ?? 0) === 0;

      clear(framesKv);
      if (dev) {
        const active = dev.devices.filter((d) => !d.revoked_at);
        const counts = new Map<DeviceStatus, number>();
        for (const d of active) counts.set(d.status, (counts.get(d.status) ?? 0) + 1);
        if (active.length === 0) {
          kvRow(framesKv, 'Registered', '0 — pair your first frame (Frames → Pending pairing)');
        } else {
          const order: DeviceStatus[] = ['online', 'sync_error', 'updating', 'update_failed', 'rollback', 'recently_seen', 'offline'];
          const parts = order.filter((k) => counts.get(k)).map((k) => `${counts.get(k)} ${k.replace('_', ' ')}`);
          kvRow(framesKv, 'Status', parts.join(' · '));
        }
        // No Total/Revoked rows here: the Devices stat card already
        // carries the live count and revoked sub-line, and the status
        // line above is itself the total.
      }

      clear(recentGrid);
      const recent = (s.photos.recent ?? []) as unknown as Array<{
        id: string;
        name: string;
        uploaded_by_name: string;
        uploaded_at: string;
      }>;
      if (recent.length === 0) {
        recentGrid.append(
          emptyState({
            icon: 'photo',
            compact: true,
            title: 'No photographs yet',
            body: 'Recent uploads appear here.',
          }),
        );
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
