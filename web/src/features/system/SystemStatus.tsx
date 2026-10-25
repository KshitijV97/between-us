/**
 * Shows whether the browser can reach the API and which build/mode it runs.
 * This is the M0 smoke test for the full path browser → proxy → Go server.
 */
import { useQuery } from '@tanstack/react-query';

import { api } from '../../api/client';

export function SystemStatus() {
  const { data, error, isPending } = useQuery({
    queryKey: ['system', 'info'],
    queryFn: ({ signal }) => api.systemInfo(signal),
  });

  if (isPending) {
    return <p role="status">Connecting to the API…</p>;
  }
  if (error) {
    return (
      <p role="alert" className="status status--error">
        Cannot reach the API: {error.message}. Is <code>betweenus serve</code> running?
      </p>
    );
  }
  return (
    <dl className="status" aria-label="System information">
      <div>
        <dt>API</dt>
        <dd>connected</dd>
      </div>
      <div>
        <dt>Version</dt>
        <dd>{data.version}</dd>
      </div>
      <div>
        <dt>Mode</dt>
        <dd>{data.mode === 'local' ? 'Local (single user, data stays here)' : 'Hosted'}</dd>
      </div>
      <div>
        <dt>Profile</dt>
        <dd>{data.profile}</dd>
      </div>
    </dl>
  );
}
