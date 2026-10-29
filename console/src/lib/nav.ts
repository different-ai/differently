// The sidebar. Each page passes its id to the layout to mark itself current.
export interface NavItem { id: string; label: string; href: string; icon: string }
export interface NavGroup { label?: string; items: NavItem[] }

export const nav: NavGroup[] = [
  { items: [
    { id: 'home', label: 'Home', href: '', icon: 'home' },
    { id: 'api-keys', label: 'API keys', href: 'api-keys/', icon: 'key' },
  ] },
  { label: 'Sandboxes', items: [
    { id: 'sandboxes', label: 'Sandboxes', href: 'sandboxes/', icon: 'box' },
    { id: 'snapshots', label: 'Snapshots', href: 'snapshots/', icon: 'layers' },
  ] },
  { label: 'Infrastructure', items: [
    { id: 'providers', label: 'Providers', href: 'providers/', icon: 'server' },
    { id: 'secrets', label: 'Secrets', href: 'secrets/', icon: 'lock' },
    { id: 'policies', label: 'Policies', href: 'policies/', icon: 'shield' },
  ] },
  { label: 'Monitoring', items: [
    { id: 'usage', label: 'Usage', href: 'usage/', icon: 'chart' },
    { id: 'audit-log', label: 'Audit log', href: 'audit-log/', icon: 'list' },
    { id: 'trust-report', label: 'Trust report', href: 'trust-report/', icon: 'check' },
  ] },
];

export const footerNav: NavItem[] = [
  { id: 'docs', label: 'Docs', href: 'https://different-ai.github.io/differently/docs/', icon: 'book' },
  { id: 'settings', label: 'Settings', href: 'settings/', icon: 'settings' },
];
