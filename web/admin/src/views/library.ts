import {
  albumAddPhotos,
  createAlbum,
  currentUser,
  deletePhoto,
  deletePhotos,
  listAlbums,
  listPhotos,
  photoFileURL,
  restorePhoto,
  updatePhoto,
  uploadPhotos,
  type Photo,
} from '../api';
import { banner, clear, confirmAction, emptyState, formatBytes, formatDateTime, h, openModal, pickFiles } from '../components';

const PAGE = 60;

export function renderLibrary(root: HTMLElement): void {
  const me = currentUser();
  const isAdmin = me?.role === 'admin';

  const toolbar = h('div', { class: 'toolbar' });
  const search = h('input', {
    type: 'search',
    class: 'input',
    placeholder: 'Search by filename or caption…',
    'aria-label': 'Search photographs',
  });
  const uploadBtn = h('button', { class: 'btn primary', type: 'button' }, 'Upload photographs');
  const trashBtn = h('button', {
    class: 'btn ghost',
    type: 'button',
  }, 'Recover deleted');
  // Selection mode: pick photographs in the library, then push them to
  // albums (or trash) — the Google-Photos direction, which is how
  // album building actually happens in practice.
  const selectBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Select');
  void isAdmin;
  const count = h('span', { class: 'muted' }, '');
  toolbar.append(search, uploadBtn, trashBtn, selectBtn, count);
  root.append(toolbar);

  // Floating selection bar (visible while anything is selected).
  const selBar = h('div', { class: 'sel-bar', hidden: '' });
  const selCount = h('span', { class: 'sel-count' }, '');
  const addAlbumBtn = h('button', { class: 'btn primary', type: 'button' }, 'Add to album');
  const selectAllBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Select all');
  const selDeleteBtn = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Delete');
  const clearSelBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Clear');
  selBar.append(selCount, addAlbumBtn, selectAllBtn, selDeleteBtn, clearSelBtn);
  root.append(selBar);

  let selecting = false;
  const selection = new Set<string>();
  let pagePhotos: Photo[] = [];

  const updateSelBar = (): void => {
    selBar.hidden = selection.size === 0;
    selCount.textContent = `${selection.size} selected`;
  };

  const exitSelecting = (): void => {
    selecting = false;
    selection.clear();
    selectBtn.classList.remove('active');
    grid.classList.remove('selecting');
    updateSelBar();
  };

  selectBtn.addEventListener('click', () => {
    if (deletedView) {
      banner(root, 'info', 'Selection is for live photographs — restore from the trash first.');
      return;
    }
    selecting = !selecting;
    selectBtn.classList.toggle('active', selecting);
    grid.classList.toggle('selecting', selecting);
    if (!selecting) selection.clear();
    updateSelBar();
    void render();
  });

  clearSelBtn.addEventListener('click', () => {
    selection.clear();
    updateSelBar();
    void render();
  });

  selectAllBtn.addEventListener('click', () => {
    for (const p of pagePhotos) selection.add(p.id);
    updateSelBar();
    void render();
  });

  selDeleteBtn.addEventListener('click', async () => {
    const n = selection.size;
    const yes = await confirmAction(
      `Move ${n} photograph${n === 1 ? '' : 's'} to the trash? You can restore them later.`,
      'Move to trash',
    );
    if (!yes) return;
    try {
      const res = await deletePhotos([...selection]);
      banner(root, 'success', `Moved ${res.deleted} photograph${res.deleted === 1 ? '' : 's'} to the trash.`);
      selection.clear();
      updateSelBar();
      void render();
    } catch (err) {
      banner(root, 'error', String(err));
    }
  });

  addAlbumBtn.addEventListener('click', () => openAddToAlbum());

  function openAddToAlbum(): void {
    const n = selection.size;
    const m = openModal(`Add to album — ${n} photograph${n === 1 ? '' : 's'}`);
    const listWrap = h('div', { class: 'album-pick-list' });
    listWrap.append(h('div', { class: 'loading' }, 'Loading…'));
    m.body.append(listWrap);

    const newField = h('div', { class: 'field', style: 'margin-top:1rem' });
    const newInput = h('input', { class: 'input', placeholder: 'Or create a new album…' });
    newField.append(h('label', {}, 'New album'), newInput);
    m.body.append(newField);

    const createAddBtn = h('button', { class: 'btn', type: 'button' }, 'Create & add');
    const confirmBtn = h('button', { class: 'btn primary', type: 'button' }, 'Add');
    const cancelBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Cancel');
    cancelBtn.addEventListener('click', () => m.close());
    const actions = h('div', { class: 'modal-actions' }, confirmBtn, createAddBtn, cancelBtn);
    m.body.append(actions);

    const boxTargets = new Map<HTMLInputElement, { id: string; name: string }>();
    void (async () => {
      try {
        const { albums } = await listAlbums();
        clear(listWrap);
        if (albums.length === 0) {
          listWrap.append(h('div', { class: 'muted' }, 'No albums yet — create one below.'));
          return;
        }
        for (const a of albums) {
          const cb = h('input', { type: 'checkbox' }) as HTMLInputElement;
          boxTargets.set(cb, { id: a.id, name: a.name });
          const label = h('label', { class: 'check-row' }, cb, h('span', {}, a.name + (a.shared ? ' (shared)' : '')));
          listWrap.append(label);
        }
      } catch (e) {
        clear(listWrap);
        listWrap.append(h('div', { class: 'muted' }, `Failed to load albums: ${String(e)}`));
      }
    })();

    const run = async (mode: 'checked' | 'new'): Promise<void> => {
      const photoIds = [...selection];
      let targetNames: string[] = [];
      const targets: string[] = [];
      if (mode === 'new') {
        const name = newInput.value.trim();
        if (!name) return;
        targetNames = [name];
      } else {
        for (const [b, t] of boxTargets) {
          if (b.checked) {
            targets.push(t.id);
            targetNames.push(t.name);
          }
        }
        if (targets.length === 0) {
          banner(root, 'info', 'Pick at least one album (or create one below).');
          return;
        }
      }
      confirmBtn.disabled = true;
      createAddBtn.disabled = true;
      const label0 = confirmBtn.textContent ?? '';
      confirmBtn.textContent = 'Adding…';
      try {
        if (mode === 'new') {
          const { album } = await createAlbum({ name: targetNames[0] });
          await albumAddPhotos(album.id, photoIds);
        } else {
          for (const albumId of targets) {
            await albumAddPhotos(albumId, photoIds);
          }
        }
        m.close();
        selection.clear();
        updateSelBar();
        banner(root, 'success', `Added ${n} photograph${n === 1 ? '' : 's'} to ${targets.length === 1 ? targetNames[0] : `${targets.length} albums`}.`);
        void render();
      } catch (err) {
        confirmBtn.disabled = false;
        createAddBtn.disabled = false;
        confirmBtn.textContent = label0;
        banner(root, 'error', String(err));
      }
    };

    confirmBtn.addEventListener('click', () => void run('checked'));
    createAddBtn.addEventListener('click', () => void run('new'));
  }

  const drop = h('div', {
    class: 'dropzone',
    'aria-label': 'Upload area',
  }, 'Drop images here (JPEG, PNG, WebP) or use Upload');
  root.append(drop);

  const feedback = h('div', { class: 'feedback', hidden: '' });
  root.append(feedback);

  const grid = h('div', { class: 'photo-grid' });
  root.append(grid);

  let deletedView = false;
  let searchTimer: number | undefined;

  const startUpload = async (): Promise<void> => {
    const files = await pickFiles('image/jpeg,image/png,image/webp', true);
    await doUpload(files);
  };

  const render = async (): Promise<void> => {
    if (deletedView && selecting) exitSelecting();
    pagePhotos = [];
    clear(grid);
    grid.append(h('div', { class: 'loading' }, 'Loading…'));
    try {
      const { photos, total } = await listPhotos({
        search: search.value.trim() || undefined,
        deleted: deletedView,
        limit: PAGE,
      });
      pagePhotos = photos;
      clear(grid);
      count.textContent = deletedView
        ? `${total} deleted photograph${total === 1 ? '' : 's'}`
        : `${total} photograph${total === 1 ? '' : 's'}`;
      if (photos.length === 0) {
        const q = search.value.trim();
        if (deletedView) {
          grid.append(
            emptyState({
              icon: 'trash',
              title: 'Nothing in the trash',
              body: 'Deleted photographs stay here until you restore them.',
            }),
          );
        } else if (q) {
          grid.append(
            emptyState({
              icon: 'search',
              title: 'No matches',
              body: `No photographs match “${q}”.`,
            }),
          );
        } else {
          grid.append(
            emptyState({
              icon: 'photo',
              title: 'No photographs yet',
              body: 'Upload the first batch — photographs appear in albums and on your frames.',
              action: { label: 'Upload photographs', onClick: () => void startUpload() },
            }),
          );
        }
        return;
      }
      for (const p of photos) grid.append(photoTile(p, deletedView));
    } catch (err) {
      clear(grid);
      grid.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
    }
  };

  search.addEventListener('input', () => {
    window.clearTimeout(searchTimer);
    searchTimer = window.setTimeout(() => void render(), 300);
  });

  trashBtn.addEventListener('click', () => {
    deletedView = !deletedView;
    trashBtn.classList.toggle('active', deletedView);
    drop.hidden = deletedView;
    uploadBtn.hidden = deletedView;
    search.hidden = deletedView;
    void render();
  });

  const doUpload = async (files: File[]): Promise<void> => {
    if (files.length === 0) return;
    clear(feedback);
    feedback.hidden = false;
    feedback.append(h('div', { class: 'muted' }, `Uploading ${files.length} file${files.length === 1 ? '' : 's'}…`));
    try {
      const res = await uploadPhotos(files);
      clear(feedback);
      const ok = res.created.length;
      const bad = res.failed.length;
      if (ok > 0) {
        banner(root, 'success', `Uploaded ${ok} photograph${ok === 1 ? '' : 's'}${res.created.some((c) => c.duplicate) ? ' (duplicates linked, not re-stored)' : ''}.`);
      }
      if (bad > 0) {
        const detail = h('div', { class: 'feedback' });
        for (const f of res.failed) {
          detail.append(h('div', { class: 'muted' }, `✕ ${f.name}: ${f.error}`));
        }
        feedback.replaceChildren(detail);
        feedback.hidden = false;
      }
      void render();
    } catch (err) {
      banner(root, 'error', `Upload failed: ${String(err)}`);
    }
  };

  uploadBtn.addEventListener('click', () => void startUpload());

  // Drag & drop.
  drop.addEventListener('dragover', (e) => {
    e.preventDefault();
    drop.classList.add('drag');
  });
  drop.addEventListener('dragleave', () => drop.classList.remove('drag'));
  drop.addEventListener('drop', async (e) => {
    e.preventDefault();
    drop.classList.remove('drag');
    await doUpload(Array.from(e.dataTransfer?.files ?? []));
  });

  void render();

  function photoTile(p: Photo, deleted: boolean): HTMLElement {
    const tile = h('figure', { class: 'photo-tile' });
    const selectable = selecting && !deleted;
    if (selectable && selection.has(p.id)) tile.classList.add('selected');
    const imgWrap = h('div', { class: 'photo-thumb' });
    imgWrap.append(h('img', { src: photoFileURL(p.id, 'thumb'), alt: p.original_name, loading: 'lazy' }));
    const figcaption = h('figcaption');
    figcaption.append(
      h('div', { class: 'photo-name' }, p.original_name),
      h('div', { class: 'muted photo-meta' }, `${p.width}×${p.height} · ${formatBytes(p.size_bytes)} · ${p.uploader_name ?? p.uploaded_by}`),
    );
    tile.append(imgWrap, figcaption);

    const actions = h('div', { class: 'photo-actions' });
    if (deleted) {
      const restore = h('button', { class: 'btn', type: 'button' }, 'Restore');
      restore.addEventListener('click', async (e) => {
        e.stopPropagation();
        try {
          await restorePhoto(p.id);
          banner(root, 'success', `Restored ${p.original_name}.`);
          void render();
        } catch (err) {
          banner(root, 'error', String(err));
        }
      });
      actions.append(restore);
    } else {
      const download = h('button', { class: 'btn ghost', type: 'button' }, 'Original');
      download.title = 'Download the original file';
      download.addEventListener('click', async (e) => {
        e.stopPropagation();
        window.open(photoFileURL(p.id, 'original'), '_blank');
      });
      const del = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Delete');
      del.addEventListener('click', async (e) => {
        e.stopPropagation();
        const yes = await confirmAction(`Move ${p.original_name} to the trash? You can restore it later.`, 'Move');
        if (!yes) return;
        try {
          await deletePhoto(p.id);
          banner(root, 'success', `${p.original_name} moved to the trash.`);
          void render();
        } catch (err) {
          banner(root, 'error', String(err));
        }
      });
      actions.append(download, del);
    }

    if (selectable) {
      tile.append(h('div', { class: 'photo-check' }, selection.has(p.id) ? '✓' : ''));
    }

    tile.addEventListener('click', () => {
      if (selectable) {
        const now = !selection.has(p.id);
        if (now) selection.add(p.id);
        else selection.delete(p.id);
        tile.classList.toggle('selected', now);
        const check = tile.querySelector('.photo-check');
        if (check) check.replaceChildren(now ? '✓' : '');
        updateSelBar();
        return;
      }
      openPhotoDetail(p, () => void render(), (msg, kind) => banner(root, kind, msg));
    });
    tile.append(actions);
    return tile;
  }
}

function openPhotoDetail(
  p: Photo,
  onSaved: () => void | Promise<void>,
  notify: (msg: string, kind: 'info' | 'error' | 'success') => void,
): void {
  const m = openModal(p.original_name);
  const viewer = h('div', { class: 'detail-viewer' });
  const img = h('img', { src: photoFileURL(p.id, 'display'), alt: p.original_name });
  img.addEventListener('click', () => {
    img.src = photoFileURL(p.id, img.src.endsWith('display') ? 'original' : 'display');
  });
  viewer.append(img);
  m.body.append(viewer);

  const meta = h('dl', { class: 'kv' });
  const row = (k: string, v: string): void => meta.append(h('dt', {}, k), h('dd', {}, v));
  row('Dimensions', `${p.width} × ${p.height}`);
  row('Size', formatBytes(p.size_bytes));
  row('Type', p.mime);
  row('Uploaded', formatDateTime(p.created_at));
  row('Uploaded by', p.uploader_name ?? p.uploaded_by);
  row('Capture date', p.capture_date ? formatDateTime(p.capture_date) : '—');
  row('Hash (sha256)', p.hash);
  m.body.append(meta);

  const captionRow = h('div', { class: 'field' });
  captionRow.append(
    h('label', { for: 'caption' }, 'Caption'),
    h('input', { id: 'caption', class: 'input', value: p.caption ?? '' }),
  );
  const dateRow = h('div', { class: 'field' });
  const dateInput = h('input', { id: 'capture', class: 'input', type: 'datetime-local' });
  if (p.capture_date) {
    const d = new Date(p.capture_date);
    if (!Number.isNaN(d.getTime())) {
      const pad = (n: number): string => String(n).padStart(2, '0');
      dateInput.value = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
    }
  }
  dateRow.append(h('label', { for: 'capture' }, 'Capture date (local)'), dateInput);

  const save = h('button', { class: 'btn primary', type: 'button' }, 'Save changes');
  save.addEventListener('click', async () => {
    try {
      const caption = (captionRow.querySelector('input') as HTMLInputElement).value;
      const dateVal = dateInput.value;
      await updatePhoto(p.id, {
        caption,
        capture_date: dateVal ? new Date(dateVal).toISOString() : undefined,
      });
      notify('Saved.', 'success');
      m.close();
      await onSaved();
    } catch (err) {
      notify(String(err), 'error');
    }
  });
  m.body.append(captionRow, dateRow, h('div', { class: 'modal-actions' }, save));
}
