// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLinksValidator from 'starlight-links-validator';

const repo = 'https://github.com/different-ai/differently';

export default defineConfig({
  site: 'https://different-ai.github.io',
  base: '/differently',
  trailingSlash: 'always',
  integrations: [
    starlight({
      title: 'Differently',
      description: 'Run VMs differently: own them. A self-hosted control plane for sandboxes and Firecracker microVMs.',
      logo: { src: './src/assets/mark.svg', alt: 'Differently' },
      favicon: '/favicon.svg',
      social: [{ icon: 'github', label: 'GitHub', href: repo }],
      editLink: { baseUrl: `${repo}/edit/pre-release/` },
      customCss: ['./src/styles/docs.css'],
      head: [
        { tag: 'link', attrs: { rel: 'preconnect', href: 'https://fonts.googleapis.com' } },
        { tag: 'link', attrs: { rel: 'preconnect', href: 'https://fonts.gstatic.com', crossorigin: true } },
        {
          tag: 'link',
          attrs: {
            rel: 'stylesheet',
            href: 'https://fonts.googleapis.com/css2?family=Bricolage+Grotesque:opsz,wght@12..96,500;12..96,700&family=IBM+Plex+Mono:wght@400;500&family=IBM+Plex+Sans:wght@400;500;600&display=swap',
          },
        },
        { tag: 'script', attrs: { type: 'module', src: '/differently/mermaid-init.js' } },
      ],
      plugins: [starlightLinksValidator({ errorOnLocalLinks: false })],
      sidebar: [
        { label: 'Start here', items: [
          { label: 'Overview', link: '/docs/' },
          { label: 'Quickstart', link: '/docs/guides/quickstart/' },
          { label: 'Organization onboarding', link: '/docs/guides/organization-onboarding/' },
          { label: 'Hybrid overflow', link: '/docs/guides/hybrid-overflow/' },
        ] },
        { label: 'Reference', items: [
          { label: 'HTTP API', link: '/docs/reference/api/' },
          { label: 'CLI', link: '/docs/reference/cli/' },
          { label: 'TypeScript SDK', link: '/docs/sdks/typescript/' },
          { label: 'Python SDK', link: '/docs/sdks/python/' },
          { label: 'Go SDK', link: '/docs/sdks/go/' },
          { label: 'Adapter specification', link: '/docs/adapters/specification/' },
        ] },
        { label: 'Deploy and operate', items: [{ autogenerate: { directory: 'docs/deploy' } }] },
        { label: 'Concepts', items: [
          { label: 'Architecture', link: '/docs/architecture/' },
          { label: 'Placement policy', link: '/docs/scheduling/placement-policy/' },
          { label: 'Storage and lifecycle', link: '/docs/storage-and-lifecycle/' },
          { label: 'Reliability and performance', link: '/docs/reliability-and-performance/' },
        ] },
        { label: 'Security', items: [{ autogenerate: { directory: 'docs/security' } }] },
        { label: 'Compare', items: [
          { label: 'Feature parity', link: '/docs/feature-parity/' },
          { label: 'Competitive landscape', link: '/docs/competitive-landscape/' },
        ] },
        { label: 'Project', items: [
          { label: 'Roadmap', link: '/docs/roadmap/' },
          { label: 'Backlog', link: '/docs/backlog/' },
          { label: 'Proofs of concept', link: '/docs/proof-of-concepts/' },
          { label: 'Acceptance scenarios', link: '/docs/acceptance-scenarios/' },
        ] },
        { label: 'Decision records', items: [{ autogenerate: { directory: 'docs/adr' } }] },
      ],
    }),
  ],
});
