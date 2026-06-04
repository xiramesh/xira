import { defaultXiraBaseURL } from "./api/xiraClient";

const featureAreas = [
  "conversation",
  "activity",
  "run-inspector",
  "agents",
  "sessions",
  "entrypoints"
];

export function XiraGardenApp() {
  return (
    <main className="app-shell">
      <header className="topbar">
        <div>
          <span className="eyebrow">Xira GUI</span>
          <h1>XiraGarden</h1>
        </div>
        <code>{defaultXiraBaseURL}</code>
      </header>
      <section className="layout-note">
        <h2>Runtime client boundary</h2>
        <p>
          XiraGarden is a GUI client for <code>xira serve</code>. It talks to the
          runtime through HTTP and WebSocket APIs, keeping the Go core private to
          <code> apps/xira</code>.
        </p>
      </section>
      <section className="feature-grid" aria-label="Planned XiraGarden areas">
        {featureAreas.map((area) => (
          <article key={area}>
            <h3>{area}</h3>
          </article>
        ))}
      </section>
    </main>
  );
}
