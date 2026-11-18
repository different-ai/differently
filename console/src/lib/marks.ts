// Provider marks, bundled from the website's copies (site/public/providers) so
// the console shows them with no network access to anywhere else. They are the
// providers' trademarks, used only to identify them, and are not covered by
// this repository's license.
const files = import.meta.glob<string>('../../../site/public/providers/*.png', { eager: true, query: '?url', import: 'default' });
const byFile = new Map(Object.entries(files).map(([path, url]) => [path.slice(path.lastIndexOf('/') + 1), url]));

/** The bundled URL of a mark by its file name in the catalog (`daytona.png`). */
export const markURL = (file: string | undefined): string | undefined => (file ? byFile.get(file) : undefined);
