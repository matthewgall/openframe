import { listAudit } from '../api';
import { clear, formatDateTime, h } from '../components';

export function renderAudit(root: HTMLElement): void {
  const toolbar = h('div', { class: 'toolbar' });
  const refreshBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Refresh');
  toolbar.append(refreshBtn, h('span', { class: 'muted' }, 'Most recent 200 entries.'));
  root.append(toolbar);

  const wrap = h('div', { class: 'table-wrap' });
  root.append(wrap);

  const render = async (): Promise<void> => {
    clear(wrap);
    try {
      const { entries } = await listAudit(200);
      clear(wrap);
      if (entries.length === 0) {
        wrap.append(h('div', { class: 'muted' }, 'No audit entries yet.'));
        return;
      }
      const table = h('table', { class: 'table' });
      table.append(h('thead', {}, h('tr', {},
        h('th', {}, 'When'), h('th', {}, 'Action'), h('th', {}, 'Actor'), h('th', {}, 'Target'), h('th', {}, 'IP'), h('th', {}, 'Detail'),
      )));
      const tbody = h('tbody');
      for (const e of entries) {
        let detail = e.detail;
        try {
          const parsed = JSON.parse(detail);
          detail = JSON.stringify(parsed);
        } catch {
          /* keep raw */
        }
        tbody.append(h('tr', {},
          h('td', { class: 'muted' }, formatDateTime(e.created_at)),
          h('td', {}, h('code', {}, e.action)),
          h('td', {}, e.actor_name ?? e.actor_user_id ?? 'system'),
          h('td', { class: 'muted' }, [e.target_type, e.target_id].filter(Boolean).join(' / ')),
          h('td', { class: 'muted' }, e.ip ?? '—'),
          h('td', { class: 'muted detail' }, detail),
        ));
      }
      table.append(tbody);
      wrap.append(table);
    } catch (err) {
      clear(wrap);
      wrap.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
    }
  };

  refreshBtn.addEventListener('click', () => void render());
  void render();
}
