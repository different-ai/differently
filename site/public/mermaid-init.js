// Renders ```mermaid blocks (emitted as <pre class="mermaid">) on docs pages.
const blocks = document.querySelectorAll('pre.mermaid');
if (blocks.length) {
  const { default: mermaid } = await import('https://cdn.jsdelivr.net/npm/mermaid@11.4.1/dist/mermaid.esm.min.mjs');
  const sources = [...blocks].map((b) => b.textContent);
  const render = async () => {
    const dark = document.documentElement.dataset.theme === 'dark';
    mermaid.initialize({ startOnLoad: false, theme: dark ? 'dark' : 'neutral', fontFamily: 'IBM Plex Sans, sans-serif' });
    blocks.forEach((b, i) => { b.removeAttribute('data-processed'); b.textContent = sources[i]; });
    await mermaid.run({ nodes: [...blocks] });
  };
  await render();
  new MutationObserver(render).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
}
