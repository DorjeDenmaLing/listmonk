// denma: Media's folders and preview, mixed into listmonk's mediaView (data in window._denmaMedia).
import { api } from './main.js';
import * as u from './utils.js';

const data = () => window._denmaMedia || { files: [], folders: [], folder_of: {}, folder: '' };

// The size of a file in words.
const niceSize = (n) => {
  if (n < 1024) return `${n} bytes`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
};

// denmaToFolder puts files just uploaded (IDs; null if failed) in the folder shown, if any.
export async function denmaToFolder(ids) {
  const folder = parseInt(data().folder, 10);
  const got = (ids || []).filter((id) => id);
  if (!folder || !got.length) return;
  await api('media', '/denma/media/folder', 'PUT', { ids: got, folder_id: folder }).catch(() => {});
}

// Uploaded PNGs saved as JPEGs: [chosen name, stored name].
const converted = [];
// Uploads already in Media: [chosen name, its name there].
const existing = [];
// Uploads that failed: "name: why".
const failed = [];

// denmaUploadFailed notes a failed upload, to show after the page reloads.
export function denmaUploadFailed(file, err) {
  failed.push(`${file.name}: ${err.message}`);
  return null;
}

// denmaUploaded is an uploaded file's ID (null if none, or already in Media), noting a PNG saved as a JPEG.
export function denmaUploaded(file, m) {
  if (!m) return null;
  const name = m.label || m.filename || '';
  if (m.existing) {
    existing.push([file.name, name]);
    return null;
  }
  if (/\.png$/i.test(file.name) && /\.jpg$/i.test(name)) converted.push([file.name, name]);
  return m.id;
}

// denmaUploadToast is the toast after an upload, naming failures and PNGs saved as JPEGs, or null.
export function denmaUploadToast() {
  const lines = [...failed];
  existing.forEach(([chosen, name]) => lines.push(chosen === name ? `${chosen} is already in Media.` : `${chosen} is already in Media, as ${name}.`));
  if (converted.length === 1) {
    lines.push(`${converted[0][0]} was saved as ${converted[0][1]}: PNGs over 1 MB are saved as JPEGs, which are much smaller.`);
  } else if (converted.length) {
    lines.push(`${converted.length} PNGs over 1 MB were saved as JPEGs, which are much smaller: ${converted.map((c) => c[1]).join(', ')}.`);
  }
  if (!lines.length) return null;
  const toast = { message: lines.join(' '), variant: failed.length ? 'danger' : 'info', duration: failed.length ? 15000 : 10000 };
  converted.length = 0;
  existing.length = 0;
  failed.length = 0;
  return toast;
}

export function denmaMedia() {
  const d = data();
  return {
    denmaFiles: d.files || [],
    denmaFolders: d.folders || [],
    folderOf: { ...(d.folder_of || {}) },
    shown: null,
    shownIndex: -1,
    shownInfo: { dims: '', size: '' },
    moved: false,
    folderEdit: { id: 0, name: '', busy: false },
    renaming: false,
    renameTo: '',
    renameBusy: false,

    niceDate: u.niceDate,
    denmaIsImage: (m) => (m.content_type || '').startsWith('image/'),
    denmaExt: (name) => (name.split('.').pop() || '').toUpperCase(),
    denmaName: (m) => m.label || m.filename,

    // ---- The preview.

    denmaPreview(e, id) {
      if (e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return; // a new tab, as asked
      const i = this.denmaFiles.findIndex((m) => m.id === id);
      if (i < 0) return;
      e.preventDefault();
      this.denmaShow(i);
      this.$refs.denmaPreview.showModal();
    },

    denmaShow(i) {
      this.renaming = false;
      this.shownIndex = i;
      this.shown = this.denmaFiles[i];
      const m = this.shown;
      const w = m.meta && m.meta.width;
      const h = m.meta && m.meta.height;
      this.shownInfo = { dims: w && h ? `${w} × ${h} px` : '', size: '' };
      // Its size from the server (S3 may not say, cross-origin).
      fetch(m.url).then((r) => (r.ok ? r.blob() : null)).then((b) => {
        if (b && b.size > 0 && this.shown === m) this.shownInfo.size = niceSize(b.size);
      }).catch(() => {});
    },

    denmaStep(by) {
      const i = this.shownIndex + by;
      if (i >= 0 && i < this.denmaFiles.length) this.denmaShow(i);
    },

    // Dimensions from the image, if the library didn't record them.
    denmaMeasure(img) {
      if (!this.shownInfo.dims && img.naturalWidth) this.shownInfo.dims = `${img.naturalWidth} × ${img.naturalHeight} px`;
    },

    denmaPreviewClosed() {
      this.shown = null;
      // Files moved out of the folder shown, and the counts, are as they were.
      if (this.moved) u.reload();
    },

    // ---- Renaming: the label only, not the file's address (cmd/denma_medianames.go).

    denmaStartRename() {
      this.renameTo = this.denmaName(this.shown);
      this.renameBusy = false;
      this.renaming = true;
    },

    async denmaRename() {
      const m = this.shown;
      this.renameBusy = true;
      let out;
      try {
        out = await api('media', `/denma/media/${m.id}/label`, 'PUT', { label: this.renameTo });
      } catch (err) {
        this.renameBusy = false;
        return;
      }
      m.label = out.label;
      this.renaming = false;
      this.moved = true;
      u.toast(`Renamed ${out.label}`);
    },

    denmaFolderOf(id) {
      return this.folderOf[id] || 0;
    },

    denmaFolderName(id) {
      const f = this.denmaFolders.find((x) => x.id === this.denmaFolderOf(id));
      return f ? `In ${f.name}` : 'Not in a folder';
    },

    async denmaMove(id, folder) {
      const to = parseInt(folder, 10) || 0;
      try {
        await api('media', '/denma/media/folder', 'PUT', { ids: [id], folder_id: to });
      } catch (err) {
        return;
      }
      this.folderOf[id] = to;
      this.moved = true;
      const f = this.denmaFolders.find((x) => x.id === to);
      u.toast(f ? `Moved to ${f.name}` : 'Taken out of its folder');
    },

    // ---- Dragging files onto folders.

    denmaDrag(e, id) {
      if (!d.manage) return;
      e.dataTransfer.setData('application/x-denma-media', String(id));
      e.dataTransfer.effectAllowed = 'move';
      document.body.classList.add('denma-media-dragging');
      e.target.addEventListener('dragend', () => document.body.classList.remove('denma-media-dragging'), { once: true });
    },

    denmaDragOver(e, folder) {
      if (!e.dataTransfer.types.includes('application/x-denma-media')) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      e.currentTarget.classList.add('denma-drop');
    },

    denmaDragLeave(e) {
      e.currentTarget.classList.remove('denma-drop');
    },

    async denmaDrop(e, folder) {
      e.currentTarget.classList.remove('denma-drop');
      document.body.classList.remove('denma-media-dragging');
      const id = parseInt(e.dataTransfer.getData('application/x-denma-media'), 10);
      if (!id || this.denmaFolderOf(id) === folder) return;
      try {
        await api('media', '/denma/media/folder', 'PUT', { ids: [id], folder_id: folder });
      } catch (err) {
        return;
      }
      const f = this.denmaFolders.find((x) => x.id === folder);
      u.reload({ message: f ? `Moved to ${f.name}` : 'Taken out of its folder' });
    },

    // ---- Folders.

    denmaFolderDialog(id, name) {
      this.folderEdit = { id: parseInt(id, 10) || 0, name: name || '', busy: false };
      this.$refs.denmaFolderDialog.showModal();
    },

    async denmaSaveFolder() {
      const f = this.folderEdit;
      f.busy = true;
      try {
        const out = f.id
          ? await api('media', `/denma/media/folders/${f.id}`, 'PUT', { name: f.name })
          : await api('media', '/denma/media/folders', 'POST', { name: f.name });
        this.$refs.denmaFolderDialog.close();
        if (f.id) {
          u.reload({ message: 'Folder renamed' });
        } else {
          // Into the new folder, to upload or move files to it.
          u.redirect(`${window.location.pathname}?folder=${out.id}`, { message: `Folder ${out.name} made` });
        }
      } catch (err) {
        f.busy = false;
      }
    },

    // Asks before a file is deleted, saying what shows it; a file in campaigns sent or on their way isn't (cmd/denma_mediause.go).
    async denmaConfirmDelete(id) {
      let uses;
      let sent;
      try {
        ({ uses, sent } = await api('media', `/denma/media/${id}/uses`));
      } catch (err) {
        return false;
      }
      if (sent && sent.length) {
        u.toast(`This file is in e-mails sent or on their way, by ${sent.join('; ')}, so it can't be deleted.`, 'error', 8000);
        return false;
      }
      if (!uses.length) return u.confirm();
      return u.confirm(`This file is shown by ${uses.join('; ')}. Deleting it leaves the image missing from them. Delete it anyway?`);
    },

    async denmaDeleteFolder(id, name) {
      if (!(await u.confirm(`Delete the folder ${name}?`))) return;
      try {
        await api('media', `/denma/media/folders/${id}`, 'DELETE');
      } catch (err) {
        return;
      }
      u.redirect(window.location.pathname, { message: `Folder ${name} deleted` });
    },
  };
}
