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
    const m = openModal(verb, () => resolve(false));
    m.body.append(h('p', {}, message));
    const actions = h('div', { class: 'modal-actions' });
    const cancel = h('button', { class: 'btn ghost', type: 'button' }, 'Cancel');
    const ok = h('button', { class: 'btn danger', type: 'button' }, verb);
    cancel.addEventListener('click', () => m.close());
    ok.addEventListener('click', () => {
      m.close();
      resolve(true);
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
