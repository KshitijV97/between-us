import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { vi } from 'vitest';

import { App } from './App';

/** Renders App with a fresh, non-retrying query client per test. */
function renderApp() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <App />
    </QueryClientProvider>,
  );
}

function mockFetch(status: number, body: unknown, contentType = 'application/json') {
  return vi
    .spyOn(globalThis, 'fetch')
    .mockResolvedValue(
      new Response(JSON.stringify(body), { status, headers: { 'Content-Type': contentType } }),
    );
}

describe('App', () => {
  it('shows system info when the API responds', async () => {
    const fetchSpy = mockFetch(200, { version: 'v1.2.3', mode: 'local', profile: 'local' });
    renderApp();

    expect(await screen.findByText('v1.2.3')).toBeInTheDocument();
    expect(screen.getByText(/Local \(single user/)).toBeInTheDocument();
    expect(fetchSpy).toHaveBeenCalledWith('/api/v1/system/info', expect.anything());
  });

  it('shows the problem detail when the API fails', async () => {
    mockFetch(
      503,
      { type: 'about:blank', title: 'Service Unavailable', status: 503, detail: 'Starting up.' },
      'application/problem+json',
    );
    renderApp();

    expect(await screen.findByRole('alert')).toHaveTextContent('Starting up.');
  });
});
