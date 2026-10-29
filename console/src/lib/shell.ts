import { client, entryPage, hasKey, mode, serverURL, session } from './connection';

/** Runs on every console page: sends people without a connection or session to sign in, and shows where the console points. */
export async function boot(): Promise<void> {
  const shell = document.getElementById('shell');
  document.getElementById('menu-toggle')?.addEventListener('click', () => shell?.classList.toggle('open'));
  // The dimmed backdrop belongs to .shell itself, so a click on it closes the menu.
  shell?.addEventListener('click', (e) => { if (e.target === shell) shell.classList.remove('open'); });
  const chip = document.getElementById('connection');
  const label = chip?.querySelector('span:last-child');
  if (mode === 'server') {
    const s = await session().catch(() => null);
    if (!s) { location.href = entryPage(); return; }
  } else if (!hasKey()) {
    location.href = entryPage();
    return;
  }
  try {
    const v = await client().version();
    chip?.classList.add('ok');
    if (label) label.textContent = mode === 'server' ? `Connected · ${v.version}` : `${new URL(serverURL()).host} · ${v.version}`;
  } catch {
    chip?.classList.add('bad');
    if (label) label.textContent = 'Server unreachable';
  }
}
