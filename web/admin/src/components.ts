// OpenFrame admin — shared DOM helpers and small UI components.

export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Record<string, string> = {},
  ...children: Array<Node | string | null | undefined>
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v !== undefined && v !== null) node.setAttribute(k, v);
  }
  for (const c of children) {
    if (c === null || c === undefined) continue;
    node.append(c);
  }
  return node;
}

export function clear(el: HTMLElement): void {
  el.replaceChildren();
}

export function kvRow(parent: HTMLElement, k: string, v: string): void {
  parent.append(h('dt', {}, k), h('dd', {}, v));
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`;
}

export function formatDateTime(iso?: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

// --- Banner (in-view feedback) ---------------------------------------------

export function banner(el: HTMLElement, kind: 'info' | 'error' | 'success', text: string): void {
  const b = h('div', { class: `banner ${kind}`, role: 'status' }, text);
  el.prepend(b);
  if (kind === 'error') {
    b.remove();
    setTimeout(() => {
      if (el.isConnected) el.prepend(b);
    }, 0);
  }
  if (kind === 'success') {
    setTimeout(() => b.remove(), 4000);
  }
}

// --- Modal -------------------------------------------------------------------

export interface Modal {
  close: () => void;
  body: HTMLElement;
}

export function openModal(title: string, onClose?: () => void): Modal {
  const overlay = h('div', { class: 'modal-overlay', role: 'dialog', 'aria-modal': 'true', 'aria-label': title });
  const dialog = h('div', { class: 'modal' });
  const head = h('div', { class: 'modal-head' });
  head.append(
    h('h2', {}, title),
    h('button', { class: 'btn ghost', type: 'button', 'aria-label': 'Close' }, '×'),
  );
  const body = h('div', { class: 'modal-body' });
  dialog.append(head, body);
  overlay.append(dialog);
  document.body.append(overlay);

  const close = (): void => {
    overlay.remove();
    document.removeEventListener('keydown', onKey);
    onClose?.();
  };
  const onKey = (e: KeyboardEvent): void => {
    if (e.key === 'Escape') close();
  };
  document.addEventListener('keydown', onKey);
  head.querySelector('button')!.addEventListener('click', close);
  overlay.addEventListener('click', (e) => {
    if (e.target === overlay) close();
  });
  return { close, body };
}

// --- Confirm ------------------------------------------------------------------

export function confirmAction(message: string, verb = 'Delete'): Promise<boolean> {
  return new Promise((resolve) => {
    // Resolve BEFORE closing: close() fires onClose (resolve(false) —
    // the "dismissed" path), and a promise can only settle once, so the
    // affirmative must win the race.
    const m = openModal(verb, () => resolve(false));
    m.body.append(h('p', {}, message));
    const actions = h('div', { class: 'modal-actions' });
    const cancel = h('button', { class: 'btn ghost', type: 'button' }, 'Cancel');
    const ok = h('button', { class: 'btn danger', type: 'button' }, verb);
    cancel.addEventListener('click', () => m.close());
    ok.addEventListener('click', () => {
      resolve(true);
      m.close();
    });
    actions.append(cancel, ok);
    m.body.append(actions);
    ok.focus();
  });
}

// --- Temp password reveal (shown exactly once) ---------------------------------

export function revealSecret(label: string, secret: string, parent: HTMLElement): void {
  const wrap = h('div', { class: 'secret-reveal' });
  const value = h('code', {}, secret);
  const copy = h('button', { class: 'btn', type: 'button' }, 'Copy');
  copy.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(secret);
      copy.textContent = 'Copied';
      setTimeout(() => (copy.textContent = 'Copy'), 1500);
    } catch {
      copy.textContent = 'Select manually';
    }
  });
  wrap.append(
    h('div', { class: 'secret-label' }, label),
    h('div', { class: 'secret-row' }, value, copy),
    h('div', { class: 'muted' }, 'Shown once. Share it securely — it cannot be retrieved later.'),
  );
  parent.append(wrap);
}

// --- File input helper -----------------------------------------------------------

/* ---- Empty states ------------------------------------------------------ */

export type EmptyIcon =
  | 'photo'
  | 'search'
  | 'trash'
  | 'album'
  | 'frame'
  | 'list'
  | 'package';

// Stroke line-icons (24×24, currentColor) — drawn to match the UI's
// 1.5px border language, no icon library dependency.
const EMPTY_ICONS: Record<EmptyIcon, string> = {
  photo:
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2.5"/><circle cx="9" cy="9.5" r="1.8"/><path d="M3.8 17.5 9 12.8l4.2 4.2 3.4-3.4 3.6 3.6"/></svg>',
  search:
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="10.5" cy="10.5" r="6.5"/><path d="m15.5 15.5 5 5"/></svg>',
  trash:
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 7h16"/><path d="M9.5 7V4.5h5V7"/><path d="M6.5 7 7.6 19.5h8.8L17.5 7"/><path d="M10.2 11v5.5M13.8 11v5.5"/></svg>',
  album:
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3 3.5 7.8 12 12.6l8.5-4.8L12 3Z"/><path d="m3.5 12 8.5 4.8 8.5-4.8"/><path d="m3.5 16.2 8.5 4.8 8.5-4.8"/></svg>',
  frame:
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="12.5" rx="2"/><path d="M9 20.5h6M12 16.5v4"/><path d="m7 12.5 2.8-2.8 2.4 2.4 2-2 2.8 2.8"/></svg>',
  list:
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8.5 6h12M8.5 12h12M8.5 18h12"/><circle cx="4.2" cy="6" r="1" fill="currentColor" stroke="none"/><circle cx="4.2" cy="12" r="1" fill="currentColor" stroke="none"/><circle cx="4.2" cy="18" r="1" fill="currentColor" stroke="none"/></svg>',
  package:
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3 4 7.2v9.6L12 21l8-4.2V7.2L12 3Z"/><path d="m4 7.2 8 4.2 8-4.2"/><path d="M12 11.4V21"/></svg>',
};

export interface EmptyStateSpec {
  icon: EmptyIcon;
  title: string;
  body?: string;
  /** one concrete next action; omit when there is none on this page */
  action?: { label: string; onClick: () => void };
  /** tighter variant for use inside modals */
  compact?: boolean;
}

// emptyState renders the shared "nothing to show" block: a line-icon,
// a one-line title, one sentence of context, and at most one action.
// Copy rule: say what the area is and what to do next — no mood text.
export function emptyState(spec: EmptyStateSpec): HTMLElement {
  const wrap = h('div', { class: spec.compact ? 'empty compact' : 'empty' });
  const icon = h('div', { class: 'empty-icon' });
  icon.innerHTML = EMPTY_ICONS[spec.icon];
  const title = h('div', { class: 'empty-title' }, spec.title);
  wrap.append(icon, title);
  if (spec.body) wrap.append(h('p', { class: 'empty-body' }, spec.body));
  if (spec.action) {
    const btn = h('button', { class: 'btn primary', type: 'button' }, spec.action.label);
    btn.addEventListener('click', spec.action.onClick);
    wrap.append(btn);
  }
  return wrap;
}

export function pickFiles(accept: string, multiple: boolean): Promise<File[]> {
  return new Promise((resolve) => {
    const input = h('input', { type: 'file', accept, hidden: '' } as Record<string, string>);
    if (multiple) input.setAttribute('multiple', '');
    input.addEventListener('change', () => {
      const files = Array.from(input.files ?? []);
      input.remove();
      resolve(files);
    });
    document.body.append(input);
    input.click();
  });
}
