import {
  albumAddPhotos,
  currentUser,
  albumAssignDevice,
  albumRemovePhoto,
  albumSetCover,
  albumUnassignDevice,
  createAlbum,
  deleteAlbum,
  getAlbum,
  listAlbums,
  listDevices,
  listPhotos,
  photoFileURL,
  updateAlbum,
  type Album,
  type Photo,
} from '../api';
import { banner, clear, confirmAction, emptyState, formatDateTime, h, openModal } from '../components';

export function renderAlbums(root: HTMLElement): void {
  const toolbar = h('div', { class: 'toolbar' });
  const newBtn = h('button', { class: 'btn primary', type: 'button' }, 'New album');
  toolbar.append(newBtn);
  root.append(toolbar);

  const grid = h('div', { class: 'album-grid' });
  root.append(grid);

  const render = async (): Promise<void> => {
    clear(grid);
    grid.append(h('div', { class: 'loading' }, 'Loading…'));
    try {
      const { albums } = await listAlbums();
      clear(grid);
      if (albums.length === 0) {
        grid.append(
          emptyState({
            icon: 'album',
            title: 'No albums yet',
            body: 'Group photographs into albums, then assign albums to your frames.',
            action: { label: 'New album', onClick: startCreateAlbum },
          }),
        );
        return;
      }
      for (const a of albums) grid.append(albumCard(a));
    } catch (err) {
      clear(grid);
      grid.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
    }
  };

  const startCreateAlbum = (): void => {
    const m = openModal('New album');
    const name = h('input', { class: 'input', type: 'text', placeholder: 'Album name' });
    const desc = h('input', { class: 'input', type: 'text', placeholder: 'Description (optional)' });
    const shared = h('input', { type: 'checkbox', checked: '' });
    const errEl = h('div', { class: 'muted' });
    const save = h('button', { class: 'btn primary', type: 'button' }, 'Create');
    save.addEventListener('click', async () => {
      try {
        await createAlbum({
          name: name.value.trim(),
          description: desc.value.trim(),
          shared: shared.checked,
        });
        m.close();
        void render();
      } catch (e) {
        errEl.textContent = String(e);
      }
    });
    m.body.append(
      h('div', { class: 'field' }, h('label', {}, 'Name'), name),
      h('div', { class: 'field' }, h('label', {}, 'Description'), desc),
      h('div', { class: 'field check' }, shared, h('label', {}, 'Shared with all members (off = private to you)')),
      errEl,
      h('div', { class: 'modal-actions' }, save),
    );
    name.focus();
  };

  newBtn.addEventListener('click', startCreateAlbum);

  void render();

  function albumCard(a: Album): HTMLElement {
    const card = h('div', { class: 'card album-card' });
    const cover = a.cover_thumb_url
      ? h('img', { class: 'album-cover', src: a.cover_thumb_url, alt: '' })
      : h('div', { class: 'album-cover empty' }, 'No cover');
    const head = h('div', { class: 'album-head' });
    head.append(
      h('h3', { class: 'album-name' }, a.name),
      h('div', { class: 'album-meta' },
        h('span', { class: 'pill ' + (a.shared ? 'ok' : 'warn') }, a.shared ? 'shared' : 'private'),
        ` ${a.photo_count} photo${a.photo_count === 1 ? '' : 's'} · by ${a.creator_name ?? '?'} · ${formatDateTime(a.updated_at)}`,
      ),
    );
    card.append(cover, head);
    // The whole card is the affordance — it opens the album's page.
    card.addEventListener('click', () => {
      location.hash = `album/${a.id}`;
    });
    return card;
  }
}


// ---------------------------------------------------------------------------
// Album detail page (#album/{id}) — a real page, not a modal: the photo
// grid gets the same language as the library, and there is room for the
// picker, the frame assignments and the settings to breathe.
// ---------------------------------------------------------------------------

export function renderAlbumDetail(root: HTMLElement, albumId: string): void {
  const me = currentUser();
  const isAdmin = me?.role === 'admin';
  let album: Album | null = null;
  let canManage = false;
  let currentPhotoIds: string[] = [];

  const back = h('a', { href: '#albums', class: 'back-link' }, '← Albums');
  const head = h('div', { class: 'album-page-head' });
  head.append(h('div', { class: 'loading' }, 'Loading…'));
  const gridWrap = h('div', { style: 'margin-top:1rem' }, h('div', { class: 'loading' }, 'Loading photographs…'));
  const framesCard = h('div', { class: 'card', style: 'margin-top:1rem' });
  root.append(back, head, gridWrap);

  const notify = (msg: string, kind: 'info' | 'error' | 'success'): void => banner(root, kind, msg);

  // --- photo viewer -------------------------------------------------------
  function openViewer(pid: string): void {
    const m = openModal('Photograph');
    const img = h('img', { src: photoFileURL(pid, 'display'), alt: '' });
    img.addEventListener('click', () => {
      img.src = photoFileURL(pid, img.src.includes('display') ? 'original' : 'display');
    });
    m.body.append(h('div', { class: 'detail-viewer' }, img));
  }

  // --- one photo tile inside the album (library language) -----------------
  function albumPhotoTile(pid: string): HTMLElement {
    const tile = h('figure', { class: 'photo-tile' });
    tile.append(
      h('div', { class: 'photo-thumb' }, h('img', { src: photoFileURL(pid, 'thumb'), alt: '', loading: 'lazy' })),
    );
    const actions = h('div', { class: 'photo-actions' });
    const view = h('button', { class: 'btn ghost', type: 'button' }, 'View');
    view.addEventListener('click', (e) => {
      e.stopPropagation();
      openViewer(pid);
    });
    actions.append(view);
    if (canManage) {
      const cover = h('button', { class: 'btn ghost', type: 'button' }, album?.cover_photo_id === pid ? 'Cover ✓' : 'Cover');
      cover.addEventListener('click', async (e) => {
        e.stopPropagation();
        try {
          await albumSetCover(album!.id, pid);
          album!.cover_photo_id = pid;
          notify('Cover updated.', 'success');
          void load();
        } catch (err) {
          notify(String(err), 'error');
        }
      });
      const remove = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Remove');
      remove.addEventListener('click', async (e) => {
        e.stopPropagation();
        try {
          await albumRemovePhoto(album!.id, pid);
          notify('Photograph removed from album.', 'success');
          void load();
        } catch (err) {
          notify(String(err), 'error');
        }
      });
      actions.append(cover, remove);
    }
    tile.append(actions);
    tile.addEventListener('click', () => openViewer(pid));
    return tile;
  }

  function buildGrid(photoIds: string[]): HTMLElement {
    const grid = h('div', { class: 'photo-grid' });
    if (photoIds.length === 0) {
      grid.append(
        emptyState({
          icon: 'photo',
          title: 'No photographs in this album yet',
          body: canManage
            ? 'Add photographs from the library — they will show up on assigned frames.'
            : 'Photographs added by the album owner will appear here.',
          action: canManage ? { label: 'Add photographs', onClick: () => openPicker(() => void load()) } : undefined,
        }),
      );
      return grid;
    }
    for (const pid of photoIds) grid.append(albumPhotoTile(pid));
    return grid;
  }

  // --- add-from-library picker (grid + search, batch add) ------------------
  function openPicker(onDone: () => void): void {
    const m = openModal(`Add to “${album?.name ?? 'album'}”`);
    const search = h('input', { class: 'input', type: 'search', placeholder: 'Filter by filename…', 'aria-label': 'Filter photographs' });
    const grid = h('div', { class: 'photo-grid picker-grid' });
    const countEl = h('span', { class: 'sel-count' }, '0 selected');
    const selAllBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Select all');
    const addBtn = h('button', { class: 'btn primary', type: 'button' }, 'Add');
    const cancelBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Cancel');
    cancelBtn.addEventListener('click', () => m.close());

    const selection = new Set<string>();
    let candidates: Photo[] = [];
    let libraryEmpty = false;

    const visible = (): Photo[] => {
      const q = search.value.trim().toLowerCase();
      return candidates.filter((p) => !q || p.original_name.toLowerCase().includes(q));
    };

    const renderTiles = (): void => {
      clear(grid);
      const vis = visible();
      if (vis.length === 0) {
        grid.append(
          emptyState({
            icon: candidates.length === 0 ? 'photo' : 'search',
            compact: true,
            title: candidates.length === 0 ? 'Nothing to add' : 'No matches',
            body: candidates.length === 0
              ? libraryEmpty
                ? 'No photographs in the library yet — upload some first.'
                : 'All photographs are already in this album.'
              : `No photographs match “${search.value.trim()}”.`,
          }),
        );
        return;
      }
      for (const p of vis) {
        const tile = h('figure', { class: 'photo-tile' });
        if (selection.has(p.id)) tile.classList.add('selected');
        tile.append(
          h('div', { class: 'photo-thumb' }, h('img', { src: photoFileURL(p.id, 'thumb'), alt: p.original_name, loading: 'lazy' })),
          h('div', { class: 'photo-check' }, selection.has(p.id) ? '✓' : ''),
        );
        tile.addEventListener('click', () => {
          if (selection.has(p.id)) selection.delete(p.id);
          else selection.add(p.id);
          countEl.textContent = `${selection.size} selected`;
          renderTiles();
        });
        grid.append(tile);
      }
    };

    grid.append(h('div', { class: 'loading' }, 'Loading…'));
    void (async () => {
      try {
        const { photos } = await listPhotos({ limit: 500 });
        libraryEmpty = photos.length === 0;
        const inAlbum = new Set(currentPhotoIds);
        candidates = photos.filter((p) => !inAlbum.has(p.id));
        renderTiles();
      } catch (e) {
        clear(grid);
        grid.append(h('div', { class: 'muted' }, `Failed to load: ${String(e)}`));
      }
    })();

    search.addEventListener('input', renderTiles);
    selAllBtn.addEventListener('click', () => {
      const vis = visible();
      const all = vis.length > 0 && vis.every((p) => selection.has(p.id));
      for (const p of vis) {
        if (all) selection.delete(p.id);
        else selection.add(p.id);
      }
      countEl.textContent = `${selection.size} selected`;
      renderTiles();
    });

    m.body.append(
      search,
      grid,
      h('div', { class: 'modal-actions' }, countEl, selAllBtn, addBtn, cancelBtn),
    );

    addBtn.addEventListener('click', async () => {
      if (selection.size === 0) return;
      addBtn.disabled = true;
      const label = addBtn.textContent ?? '';
      addBtn.textContent = 'Adding…';
      try {
        const res = await albumAddPhotos(album!.id, [...selection]);
        m.close();
        notify(`Added ${res.added} photograph${res.added === 1 ? '' : 's'} to the album.`, 'success');
        onDone();
      } catch (e) {
        addBtn.disabled = false;
        addBtn.textContent = label;
        notify(String(e), 'error');
      }
    });
  }

  // --- settings -------------------------------------------------------------
  function openEdit(): void {
    if (!album) return;
    const m = openModal('Edit album');
    const name = h('input', { class: 'input', type: 'text', value: album.name });
    const desc = h('input', { class: 'input', type: 'text', value: album.description ?? '', placeholder: 'Description (optional)' });
    const shared = h('input', { type: 'checkbox' });
    shared.checked = album.shared;
    const errEl = h('div', { class: 'muted' });
    const save = h('button', { class: 'btn primary', type: 'button' }, 'Save');
    save.addEventListener('click', async () => {
      try {
        await updateAlbum(album!.id, {
          name: name.value.trim(),
          description: desc.value.trim(),
          shared: shared.checked,
        });
        m.close();
        notify('Album saved.', 'success');
        void load();
      } catch (e) {
        errEl.textContent = String(e);
      }
    });
    m.body.append(
      h('div', { class: 'field' }, h('label', {}, 'Name'), name),
      h('div', { class: 'field' }, h('label', {}, 'Description'), desc),
      h('div', { class: 'field check' }, shared, h('label', {}, 'Shared with all members (off = private to you)')),
      errEl,
      h('div', { class: 'modal-actions' }, save),
    );
  }

  async function delAlbum(): Promise<void> {
    const yes = await confirmAction(
      `Delete album “${album?.name}”? Photographs are not removed — only the album and its frame assignments.`,
      'Delete',
    );
    if (!yes) return;
    try {
      await deleteAlbum(album!.id);
      notify('Album deleted.', 'success');
      location.hash = 'albums';
    } catch (e) {
      notify(String(e), 'error');
    }
  }

  // --- frame assignments (admin) ---------------------------------------------
  async function buildFramesCard(assignedIds: string[]): Promise<void> {
    framesCard.replaceChildren(h('h2', {}, 'Assigned frames'));
    let devices: Array<{ id: string; name: string; revoked_at?: string }> = [];
    try {
      const { devices: d } = await listDevices();
      devices = d;
    } catch {
      // Non-fatal: the assignment controls still work by id.
    }
    if (assignedIds.length === 0) {
      framesCard.append(
        h('div', { class: 'muted' }, 'Not assigned to any frame yet — a frame only shows albums assigned to it.'),
      );
    }
    const chips = h('div', { class: 'frame-chips' });
    for (const did of assignedIds) {
      const dev = devices.find((d) => d.id === did);
      const chip = h('span', { class: 'frame-chip' }, dev?.name ?? did);
      const x = h('button', { class: 'chip-x', type: 'button', 'aria-label': `Unassign ${dev?.name ?? did}` }, '×');
      x.addEventListener('click', async () => {
        try {
          await albumUnassignDevice(album!.id, did);
          void load();
        } catch (e) {
          notify(String(e), 'error');
        }
      });
      chip.append(x);
      chips.append(chip);
    }
    if (chips.childElementCount > 0) framesCard.append(chips);

    const row = h('div', { class: 'row', style: 'margin-top:0.75rem' });
    const devSel = h('select', { class: 'input', 'aria-label': 'Frame to assign' });
    const unassigned = devices.filter((d) => !d.revoked_at && !assignedIds.includes(d.id));
    if (unassigned.length === 0) {
      devSel.append(h('option', {}, 'No other active frames'));
      devSel.disabled = true;
    }
    for (const d of unassigned) devSel.append(h('option', { value: d.id }, d.name));
    const assignBtn = h('button', { class: 'btn', type: 'button' }, 'Assign frame');
    assignBtn.addEventListener('click', async () => {
      if (!devSel.value) return;
      try {
        await albumAssignDevice(album!.id, devSel.value);
        void load();
      } catch (e) {
        notify(String(e), 'error');
      }
    });
    row.append(devSel, assignBtn);
    framesCard.append(row);
  }

  // --- load --------------------------------------------------------------------
  async function load(): Promise<void> {
    try {
      const detail = await getAlbum(albumId);
      album = detail.album;
      canManage = isAdmin || me?.id === album.created_by || album.shared;
      currentPhotoIds = detail.photos ?? [];

      // Header.
      const actions = h('div', { class: 'album-head-actions' });
      if (canManage) {
        const add = h('button', { class: 'btn primary', type: 'button' }, 'Add photographs');
        add.addEventListener('click', () => openPicker(() => void load()));
        actions.append(add);
      }
      if (isAdmin || me?.id === album.created_by) {
        const edit = h('button', { class: 'btn ghost', type: 'button' }, 'Edit');
        edit.addEventListener('click', openEdit);
        const del = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Delete');
        del.addEventListener('click', () => void delAlbum());
        actions.append(edit, del);
      }
      head.replaceChildren(
        album.cover_thumb_url
          ? h('img', { class: 'album-cover-lg', src: album.cover_thumb_url, alt: '' })
          : h('div', { class: 'album-cover-lg empty' }),
        h('div', {},
          h('h1', { class: 'album-title' }, album.name),
          h('div', { class: 'album-meta' },
            h('span', { class: 'pill ' + (album.shared ? 'ok' : 'warn') }, album.shared ? 'shared' : 'private'),
            ` · ${currentPhotoIds.length} photo${currentPhotoIds.length === 1 ? '' : 's'} · by ${album.creator_name ?? 'unknown'} · updated ${formatDateTime(album.updated_at)}`),
          actions,
        ),
      );

      gridWrap.replaceChildren(buildGrid(currentPhotoIds));

      if (isAdmin) {
        if (!root.contains(framesCard)) root.append(framesCard);
        void buildFramesCard(detail.devices ?? []);
      }
    } catch (e) {
      head.replaceChildren(h('div', { class: 'banner error' }, `Could not load album: ${String(e)}`));
      gridWrap.replaceChildren();
    }
  }

  void load();
}
