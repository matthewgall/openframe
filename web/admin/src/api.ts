// Minimal typed API client. Grows with the /api/v1 endpoints in M2+.

export interface Health {
  status: 'ok' | 'degraded';
  version: string;
  commit: string;
  started_at: string;
  uptime_seconds: number;
  time: string;
  db: { status: 'ok' | 'error'; detail?: string };
}

export async function getHealth(): Promise<Health> {
  const res = await fetch('/healthz', { headers: { Accept: 'application/json' } });
  if (!res.ok) {
    throw new Error(`health endpoint returned HTTP ${res.status}`);
  }
  return (await res.json()) as Health;
}
