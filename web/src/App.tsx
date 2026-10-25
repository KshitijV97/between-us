/**
 * Application shell. M0 renders a landing page with the API status; routing,
 * the import flow and the dashboard arrive in M3 (docs/10-roadmap.md).
 */
import { SystemStatus } from './features/system/SystemStatus';

export function App() {
  return (
    <main className="shell">
      <header>
        <h1>Between Us</h1>
        <p className="tagline">
          Understand a two-person conversation: metrics, patterns, and mood over time, from a
          WhatsApp or Telegram export. Everything stays on this machine in local mode.
        </p>
      </header>
      <section aria-labelledby="status-heading">
        <h2 id="status-heading">System status</h2>
        <SystemStatus />
      </section>
    </main>
  );
}
