import {
  albumAddPhoto,
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
  restoreAlbum,
  updateAlbum,
  type Album,
} from '../api';
import { banner, clear, confirmAction, formatDateTime, h, openModal } from '../components';

export function renderAlbums(root: HTMLElement): void {
  const toolbar = h('div', { class: 'toolbar' });
  const newBtn = h('button', { class: 'btn primary', type: 'button' }, 'New album');
  toolbar.append(newBtn);
  root.append(toolbar);

  const grid = h('div', { class: 'album-grid' });
  root.append(grid);

  const render = async (): Promise<void> => {
    clear(grid);
    grid.append(h('div', { class: 'muted' }, 'Loading…'));
    try {
      const { albums } = await listAlbums();
      clear(grid);
      if (albums.length === 0) {
        grid.append(h('div', { class: 'muted' }, 'No albums yet.'));
        return;
      }
      for (const a of albums) grid.append(albumCard(a, render));
    } catch (err) {
      clear(grid);
      grid.append(h('div', { class: 'muted' }, `Failed to load: ${String(err)}`));
    }
  };

  newBtn.addEventListener('click', () => {
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
  });

  void render();

  function albumCard(a: Album, rerender: () => Promise<void>): HTMLElement {
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

    const actions = h('div', { class: 'photo-actions' });
    const manage = h('button', { class: 'btn', type: 'button' }, 'Manage');
    manage.addEventListener('click', () =>
      openAlbumDetail(a, async () => {
        await rerender();
      }, (msg, kind) => banner(root, kind, msg)));
    void rerender;
    actions.append(manage);
    card.append(actions);
    return card;
  }
}

function openAlbumDetail(
  a: Album,
  rerender: () => Promise<void>,
  notify: (msg: string, kind: 'info' | 'error' | 'success') => void,
): void {
  const me = currentUser();
  const isAdmin = me?.role === 'admin';
  const isCreator = me?.id === a.created_by;
  const canManage = isAdmin || isCreator || a.shared;

  const m = openModal(a.name, () => void rerender());
  const info = h('div', { class: 'card' });
  const nameInput = h('input', { class: 'input', type: 'text', value: a.name });
  const descInput = h('input', { class: 'input', type: 'text', value: a.description ?? '', placeholder: 'Description' });
  const sharedBox = h('input', { type: 'checkbox' });
  if (a.shared) sharedBox.checked = true;
  const settings = h('div', { class: 'album-settings' });
  settings.append(
    h('div', { class: 'field' }, h('label', {}, 'Name'), nameInput),
    h('div', { class: 'field' }, h('label', {}, 'Description'), descInput),
    h('div', { class: 'field check' }, sharedBox, h('label', {}, 'Shared with all members')),
  );
  const saveBtn = h('button', { class: 'btn primary', type: 'button' }, 'Save settings');
  saveBtn.addEventListener('click', async () => {
    try {
      await updateAlbum(a.id, {
        name: nameInput.value.trim(),
        description: descInput.value.trim(),
        shared: sharedBox.checked,
      });
      notify('Album saved.', 'success');
    } catch (e) {
      notify(String(e), 'error');
    }
  });
  if (isAdmin || isCreator) {
    settings.append(h('div', { class: 'modal-actions' }, saveBtn));
  }

  const delBtn = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Delete album');
  delBtn.addEventListener('click', async () => {
    const yes = await confirmAction(
      `Delete album "${a.name}"? Photographs are not removed — only the album and its assignments.`,
      'Delete',
    );
    if (!yes) return;
    try {
      await deleteAlbum(a.id);
      notify('Album deleted (recoverable).', 'success');
      m.close();
      await rerender();
    } catch (e) {
      notify(String(e), 'error');
    }
  });
  const restoreBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Restore album');
  restoreBtn.addEventListener('click', async () => {
    try {
      await restoreAlbum(a.id);
      notify('Album restored.', 'success');
      m.close();
      await rerender();
    } catch (e) {
      notify(String(e), 'error');
    }
  });
  info.append(settings, h('div', { class: 'modal-actions' }, delBtn, restoreBtn));
  m.body.append(info);

  // Photos in the album.
  const listWrap = h('div', { class: 'card', style: 'margin-top:1rem' });
  listWrap.append(h('h2', {}, 'Photographs'));
  const photosWrap = h('div', { class: 'album-photos' });
  listWrap.append(photosWrap);
  m.body.append(listWrap);

  const addWrap = h('div', { class: 'card', style: 'margin-top:1rem' });
  addWrap.append(h('h2', {}, 'Add photographs'));
  const picker = h('select', { class: 'input' });
  const addBtn = h('button', { class: 'btn', type: 'button' }, 'Add');
  if (canManage) {
    addWrap.append(picker, addBtn);
    m.body.append(addWrap);
  }

  // Frame assignment (admin).
  const frameWrap = h('div', { class: 'card', style: 'margin-top:1rem' });
  frameWrap.append(h('h2', {}, 'Assigned frames'));
  const framesList = h('div');
  frameWrap.append(framesList);
  const devSel = h('select', { class: 'input' });
  const assignBtn = h('button', { class: 'btn', type: 'button' }, 'Assign');
  const unSel = h('select', { class: 'input' });
  const unassignBtn = h('button', { class: 'btn ghost', type: 'button' }, 'Unassign');
  if (isAdmin) {
    frameWrap.append(h('div', { class: 'row' }, devSel, assignBtn, unSel, unassignBtn));
    m.body.append(frameWrap);

    assignBtn.addEventListener('click', async () => {
      if (!devSel.value) return;
      try {
        await albumAssignDevice(a.id, devSel.value);
        notify('Frame assigned.', 'success');
        void load();
      } catch (e) {
        notify(String(e), 'error');
      }
    });
    unassignBtn.addEventListener('click', async () => {
      if (!unSel.value) return;
      try {
        await albumUnassignDevice(a.id, unSel.value);
        notify('Frame unassigned.', 'success');
        void load();
      } catch (e) {
        notify(String(e), 'error');
      }
    });
  }

  const load = async (): Promise<void> => {
    try {
      const detail = await getAlbum(a.id);
      const photos = detail.photos;

      clear(photosWrap);
      if (photos.length === 0) {
        photosWrap.append(h('div', { class: 'muted' }, 'No photographs in this album.'));
      }
      for (const pid of photos) {
        const row = h('div', { class: 'album-photo-row' });
        row.append(h('img', { src: photoFileURL(pid, 'thumb'), alt: '', width: '72', height: '72' }));
        if (canManage) {
          const setCover = h('button', { class: 'btn ghost', type: 'button' }, a.cover_photo_id === pid ? 'Cover ✓' : 'Cover');
          setCover.addEventListener('click', async () => {
            try {
              await albumSetCover(a.id, pid);
              a.cover_photo_id = pid;
              notify('Cover updated.', 'success');
              void load();
            } catch (e) {
              notify(String(e), 'error');
            }
          });
          const remove = h('button', { class: 'btn ghost danger-text', type: 'button' }, 'Remove');
          remove.addEventListener('click', async () => {
            try {
              await albumRemovePhoto(a.id, pid);
              notify('Photograph removed from album.', 'success');
              void load();
            } catch (e) {
              notify(String(e), 'error');
            }
          });
          row.append(setCover, remove);
        }
        photosWrap.append(row);
      }

      // Picker of live photos not yet in the album.
      if (canManage) {
        const { photos: all } = await listPhotos({ limit: 500 });
        clear(picker);
        const options = all.filter((p) => !photos.includes(p.id));
        if (options.length === 0) {
          picker.append(h('option', {}, 'All photographs are in this album'));
          picker.disabled = true;
        }
        for (const p of options) {
          picker.append(h('option', { value: p.id }, `${p.original_name} (${p.width}×${p.height})`));
        }
      }

      // Frames.
      if (isAdmin) {
        const { devices } = await listDevices();
        clear(framesList);
        const assigned = detail.devices;
        if (assigned.length === 0) {
          framesList.append(h('div', { class: 'muted' }, 'Not assigned to any frame yet.'));
        }
        for (const did of assigned) {
          const dev = devices.find((d) => d.id === did);
          framesList.append(h('div', {}, `🖥 ${dev?.name ?? did}`));
        }
        clear(devSel);
        clear(unSel);
        for (const d of devices) {
          devSel.append(h('option', { value: d.id }, d.name + (d.revoked_at ? ' (revoked)' : '')));
          if (assigned.includes(d.id)) {
            unSel.append(h('option', { value: d.id }, d.name));
          }
        }
      }
    } catch (e) {
      clear(photosWrap);
      photosWrap.append(h('div', { class: 'muted' }, `Failed to load: ${String(e)}`));
    }
  };

  addBtn.addEventListener('click', async () => {
    if (!picker.value) return;
    try {
      await albumAddPhoto(a.id, picker.value);
      notify('Photograph added to album.', 'success');
      void load();
    } catch (e) {
      notify(String(e), 'error');
    }
  });

  void load();
}
