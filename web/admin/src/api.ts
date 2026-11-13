// OpenFrame admin — typed API client.
//
// All calls go to /api/v1 with the session cookie (SameSite=Lax) and
// the CSRF synchronizer token on mutating requests. The client keeps
// the current identity + csrf in memory after login.

export interface Health {
  status: string;
  version: string;
  commit: string;
  started_at: string;
  uptime_seconds: number;
  time: string;
  db: { status: string; detail?: string };
}

export interface User {
  id: string;
  email: string;
  display_name: string;
  role: 'admin' | 'member';
  created_at: string;
  deactivated_at?: string;
}

export interface Photo {
  id: string;
  hash: string;
  original_name: string;
  mime: string;
  width: number;
  height: number;
  size_bytes: number;
  capture_date?: string;
  caption: string;
  uploaded_by: string;
  uploader_name?: string;
  created_at: string;
  deleted_at?: string;
  albums?: string[];
}

export interface Album {
  id: string;
  name: string;
  description: string;
  shared: boolean;
  cover_photo_id?: string;
  cover_thumb_url?: string;
  photo_count: number;
  sort_order: number;
  created_by: string;
  creator_name?: string;
  created_at: string;
  updated_at: string;
  deleted_at?: string;
}

export interface Device {
  id: string;
  name: string;
  created_at: string;
  revoked_at?: string;
  album_count: number;
  has_credential: boolean;
}

export interface DeviceCredential {
  token: string;
  issued_at: string;
}

// --- M5: health, status, per-frame config, pairing ---------------------------

export interface DeviceHealth {
  version: string;
  uptime_seconds: number;
  last_sync_at: string;
  last_sync_ok: boolean;
  manifest_revision: number;
  pending_downloads: number;
  storage_total: number;
  storage_used: number;
  cache_bytes: number;
  mem_total_mb: number;
  mem_used_mb: number;
  cpu_temp_c: number | null;
  display_driver: string;
  display_status: string;
  error_count_24h: number;
  recent_errors: string[];
  update_state: string;
}

export type DeviceStatus =
  | 'online'
  | 'recently_seen'
  | 'offline'
  | 'sync_error'
  | 'updating'
  | 'update_failed'
  | 'rollback';

export interface DeviceWithHealth extends Device {
  last_heartbeat_at?: string;
  sync_request_seq: number;
  health?: DeviceHealth | null;
  status: DeviceStatus;
}

export interface FrameConfig {
  slideshow_interval_sec: number;
  transition: string; // none | crossfade | slide
  transition_duration_ms: number;
  sync_interval_sec: number;
  sleep_on: string; // "HH:MM" or ""
  sleep_off: string; // "HH:MM" or ""
  brightness: number | null; // 0..100
  orientation: string; // normal | rotate90 | rotate180 | rotate270
  show_clock: boolean | null;
  show_captions: boolean | null;
  update_channel: string; // M6: stable | beta | development | '' (unset)
}

export interface FrameConfigDoc {
  config: FrameConfig;
  version: number;
  updated_at: string;
}

export interface DeviceDetail {
  device: Device;
  health: DeviceHealth | null;
  last_heartbeat_at?: string;
  status: DeviceStatus;
  sync_request_seq: number;
  config: FrameConfigDoc;
}

export interface PairingChallenge {
  id: string;
  device_id: string;
  device_name: string;
  source_ip: string;
  status: string;
  created_at: string;
  expires_at: string;
}

export interface PairingApproval {
  device?: Device;
  credential: string;
}

export interface AuditEntry {
  id: number;
  actor_user_id?: string;
  actor_name?: string;
  action: string;
  target_type?: string;
  target_id?: string;
  detail: string;
  ip?: string;
  created_at: string;
}

export interface Stats {
  photos: { total: number; deleted: number; storage_bytes: number; recent: Photo[] & { name?: string }[] };
  albums: { total: number; shared: number };
  users: { active: number };
  devices: { total: number };
  server: { version: string; hostname: string };
}

export interface Me {
  user: { id: string; email: string; display_name: string; role: string };
  session?: { id: string; expires_at: string };
}

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

interface Session {
  user: { id: string; email: string; display_name: string; role: string } | null;
  csrf: string | null;
}

const session: Session = { user: null, csrf: null };

export function currentUser(): Session['user'] {
  return session.user;
}

export function isAuthed(): boolean {
  return session.user !== null;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const opts: RequestInit = { method, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.body = JSON.stringify(body);
    opts.headers = { 'Content-Type': 'application/json' };
  }
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS' && session.csrf) {
    opts.headers = { ...(opts.headers as Record<string, string>), 'X-CSRF-Token': session.csrf };
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }
  if (!res.ok) {
    const errBody = data as { error?: string; message?: string } | null;
    throw new ApiError(
      res.status,
      errBody?.error ?? 'unknown',
      errBody?.message ?? `HTTP ${res.status}`,
    );
  }
  return data as T;
}

// --- Auth ---------------------------------------------------------------

export async function login(email: string, password: string): Promise<void> {
  const res = await fetch('/api/v1/auth/login', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  });
  const text = await res.text();
  let data: any = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      /* ignore */
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, data?.error ?? 'login_failed', data?.message ?? 'login failed');
  }
  session.user = data.user;
  session.csrf = data.csrf_token;
}

export async function logout(): Promise<void> {
  try {
    await request('/api/v1/auth/logout', 'POST', {});
  } finally {
    session.user = null;
    session.csrf = null;
  }
}

export async function refreshMe(): Promise<boolean> {
  try {
    const me: Me = await request('GET', '/api/v1/auth/me');
    session.user = me.user;
    // CSRF is derived server-side per session; fetch the current one.
    const csrf: { csrf_token: string } = await request('GET', '/api/v1/auth/csrf');
    session.csrf = csrf.csrf_token;
    return true;
  } catch {
    session.user = null;
    session.csrf = null;
    return false;
  }
}

export function changePassword(current: string, next: string): Promise<unknown> {
  return request('POST', '/api/v1/auth/password', {
    current_password: current,
    new_password: next,
  });
}

// --- Users ----------------------------------------------------------------

export function listUsers(activeOnly = false): Promise<{ users: User[] }> {
  return request('GET', `/api/v1/users${activeOnly ? '?active=1' : ''}`);
}

export function createUser(body: {
  email: string;
  display_name?: string;
  role?: string;
}): Promise<{ user: User; temporary_password: string }> {
  return request('POST', '/api/v1/users', body);
}

export function updateUser(
  id: string,
  body: { display_name?: string; role?: string; deactivated?: boolean },
): Promise<{ user: User }> {
  return request('PATCH', `/api/v1/users/${id}`, body);
}

export function resetUserPassword(id: string): Promise<{ temporary_password: string }> {
  return request('POST', `/api/v1/users/${id}/password-reset`, {});
}

export function revokeUserSessions(id: string): Promise<{ revoked: number }> {
  return request('POST', `/api/v1/users/${id}/sessions/revoke`, {});
}

// --- Photos -----------------------------------------------------------------

export interface UploadResult {
  created: { photo: Photo; duplicate: boolean }[];
  failed: { name: string; error: string; code?: string }[];
}

export async function uploadPhotos(files: File[]): Promise<UploadResult> {
  const form = new FormData();
  for (const f of files) form.append('files', f, f.name);
  const opts: RequestInit = { method: 'POST', credentials: 'same-origin', body: form };
  if (session.csrf) {
    opts.headers = { 'X-CSRF-Token': session.csrf };
  }
  const res = await fetch('/api/v1/photos/upload', opts);
  const data = (await res.json()) as UploadResult;
  if (!res.ok && !(res.status === 422 && data.created?.length)) {
    const errBody = data as any;
    throw new ApiError(res.status, errBody?.code ?? 'upload_failed', errBody?.error ?? `HTTP ${res.status}`);
  }
  return data;
}

export function listPhotos(params: {
  search?: string;
  album?: string;
  deleted?: boolean;
  limit?: number;
  offset?: number;
}): Promise<{ photos: Photo[]; total: number }> {
  const q = new URLSearchParams();
  if (params.search) q.set('search', params.search);
  if (params.album) q.set('album', params.album);
  if (params.deleted) q.set('deleted', '1');
  if (params.limit) q.set('limit', String(params.limit));
  if (params.offset) q.set('offset', String(params.offset));
  const qs = q.toString();
  return request('GET', `/api/v1/photos${qs ? `?${qs}` : ''}`);
}

export function updatePhoto(id: string, body: { caption?: string; capture_date?: string }): Promise<{ photo: Photo }> {
  return request('PATCH', `/api/v1/photos/${id}`, body);
}

export function deletePhoto(id: string): Promise<unknown> {
  return request('DELETE', `/api/v1/photos/${id}`);
}

export function restorePhoto(id: string): Promise<unknown> {
  return request('POST', `/api/v1/photos/${id}/restore`, {});
}

export function photoFileURL(id: string, variant: 'original' | 'display' | 'thumb' | 'portrait' | 'landscape' = 'display'): string {
  return `/api/v1/photos/${id}/file?variant=${variant}`;
}

// --- Albums -------------------------------------------------------------------

export function listAlbums(): Promise<{ albums: Album[] }> {
  return request('GET', '/api/v1/albums');
}

export function getAlbum(id: string): Promise<{
  album: Album;
  photos: string[];
  devices: string[];
}> {
  return request('GET', `/api/v1/albums/${id}`);
}

export function createAlbum(body: { name: string; description?: string; shared?: boolean }): Promise<{ album: Album }> {
  return request('POST', '/api/v1/albums', body);
}

export function updateAlbum(
  id: string,
  body: { name?: string; description?: string; shared?: boolean; sort_order?: number },
): Promise<{ album: Album }> {
  return request('PATCH', `/api/v1/albums/${id}`, body);
}

export function deleteAlbum(id: string): Promise<unknown> {
  return request('DELETE', `/api/v1/albums/${id}`);
}

export function restoreAlbum(id: string): Promise<unknown> {
  return request('POST', `/api/v1/albums/${id}/restore`, {});
}

export function albumAddPhoto(id: string, photoId: string): Promise<unknown> {
  return request('POST', `/api/v1/albums/${id}/photos`, { photo_id: photoId });
}

export function albumRemovePhoto(id: string, photoId: string): Promise<unknown> {
  return request('DELETE', `/api/v1/albums/${id}/photos/${photoId}`);
}

export function albumSetCover(id: string, photoId: string): Promise<{ album: Album }> {
  return request('POST', `/api/v1/albums/${id}/cover`, { photo_id: photoId });
}

export function albumAssignDevice(id: string, deviceId: string): Promise<unknown> {
  return request('POST', `/api/v1/albums/${id}/frames/${deviceId}`, {});
}

export function albumUnassignDevice(id: string, deviceId: string): Promise<unknown> {
  return request('DELETE', `/api/v1/albums/${id}/frames/${deviceId}`, {});
}

// --- Devices -----------------------------------------------------------------

export function listDevices(): Promise<{ devices: DeviceWithHealth[] }> {
  return request('GET', '/api/v1/devices');
}

export function getDevice(id: string): Promise<DeviceDetail> {
  return request('GET', `/api/v1/devices/${id}`);
}

export function getDeviceConfig(id: string): Promise<{ config: FrameConfigDoc }> {
  return request('GET', `/api/v1/devices/${id}/config`);
}

export function putDeviceConfig(id: string, cfg: FrameConfig): Promise<{ config: FrameConfigDoc }> {
  return request('PUT', `/api/v1/devices/${id}/config`, cfg);
}

export function requestDeviceSync(id: string): Promise<{ status: string; sync_request_seq: number }> {
  return request('POST', `/api/v1/devices/${id}/sync`, {});
}

// --- Pairing (M5, spec §12) ---------------------------------------------------

export function listPairingPending(): Promise<{ challenges: PairingChallenge[] }> {
  return request('GET', '/api/v1/pairing/pending');
}

export function approvePairing(id: string, code: string): Promise<PairingApproval> {
  return request('POST', `/api/v1/pairing/${id}/approve`, { code });
}

export function rejectPairing(id: string): Promise<{ status: string }> {
  return request('POST', `/api/v1/pairing/${id}/reject`, {});
}

export function createDevice(name: string): Promise<{ device: Device }> {
  return request('POST', '/api/v1/devices', { name });
}

export function issueDeviceCredential(id: string): Promise<DeviceCredential> {
  return request('POST', `/api/v1/devices/${id}/credential`);
}

export function revokeDeviceCredential(id: string): Promise<void> {
  return request('DELETE', `/api/v1/devices/${id}/credential`);
}

export function renameDevice(id: string, name: string): Promise<{ device: Device }> {
  return request('PATCH', `/api/v1/devices/${id}`, { name });
}

export function revokeDevice(id: string): Promise<unknown> {
  return request('POST', `/api/v1/devices/${id}/revoke`, {});
}

// --- Audit & stats -------------------------------------------------------------

export function listAudit(limit = 200): Promise<{ entries: AuditEntry[] }> {
  return request('GET', `/api/v1/audit?limit=${limit}`);
}

export function getStats(): Promise<Stats> {
  return request('GET', '/api/v1/stats');
}

export function getHealth(): Promise<Health> {
  return request('GET', '/healthz');
}

// --- Releases (M6, spec §14) -------------------------------------------------

export type ReleaseStatus = 'staged' | 'approved' | 'paused';

export interface ReleaseInfo {
  version: string;
  channel: string;
  os: string;
  arch: string;
  artifact_sha256: string;
  artifact_size: number;
  status: ReleaseStatus;
  is_latest: boolean;
  rollout_percent: number;
  rollout_devices: string[];
  released_at: string;
  expires_at: string;
  min_protocol_version: number;
  min_schema_version: number;
  is_rollback: boolean;
  signer_key_id: string;
  has_artifact: boolean;
  created_at: string;
  updated_at: string;
}

export interface ReleasePlatform {
  channel: string;
  os: string;
  arch: string;
}

function releaseQ(p: ReleasePlatform): string {
  return `?channel=${encodeURIComponent(p.channel)}&os=${encodeURIComponent(p.os)}&arch=${encodeURIComponent(p.arch)}`;
}

export function listReleases(): Promise<ReleaseInfo[]> {
  return request('GET', '/api/v1/releases');
}

// uploadRelease posts multipart: 'metadata' (signed JSON text) +
// 'artifact' (file). Custom fetch because request() is JSON-only.
export async function uploadRelease(metadataJson: string, artifact: Blob): Promise<ReleaseInfo> {
  const fd = new FormData();
  fd.append('metadata', metadataJson);
  fd.append('artifact', artifact, 'openframe');
  const opts: RequestInit = { method: 'POST', credentials: 'same-origin', body: fd };
  if (session.csrf) {
    opts.headers = { 'X-CSRF-Token': session.csrf };
  }
  const res = await fetch('/api/v1/releases', opts);
  const text = await res.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }
  if (!res.ok) {
    const errBody = data as { error?: string; message?: string } | null;
    throw new ApiError(res.status, errBody?.error ?? 'upload_failed', errBody?.message ?? `HTTP ${res.status}`);
  }
  return data as ReleaseInfo;
}

export function approveRelease(p: ReleasePlatform, version: string): Promise<ReleaseInfo> {
  return request('POST', `/api/v1/releases/${encodeURIComponent(version)}/approve${releaseQ(p)}`, {});
}

export function pauseRelease(p: ReleasePlatform, version: string): Promise<ReleaseInfo> {
  return request('POST', `/api/v1/releases/${encodeURIComponent(version)}/pause${releaseQ(p)}`, {});
}

export function setReleaseRollout(p: ReleasePlatform, version: string, percent: number, devices: string[]): Promise<ReleaseInfo> {
  return request('PUT', `/api/v1/releases/${encodeURIComponent(version)}/rollout${releaseQ(p)}`, { percent, devices });
}

export function deleteRelease(p: ReleasePlatform, version: string): Promise<void> {
  return request('DELETE', `/api/v1/releases/${encodeURIComponent(version)}${releaseQ(p)}`, {});
}
