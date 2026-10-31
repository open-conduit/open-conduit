import type { APIRoute } from 'astro';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// The setup guide `open-conduit init --with claude` starts Claude Code on,
// published as plain Markdown for any agent to follow (start/agents links
// it). It is read from the binary's own copy at build time, so the two never
// drift. The browser-session section is dropped: it only applies inside the
// web view's Setup page.
export const GET: APIRoute = () => {
	const source = readFileSync(resolve(process.cwd(), '../extensions/setup/playbook/setup.md'), 'utf8');
	const body = source
		.replace(/^---\n[\s\S]*?\n---\n+/, '')
		.replace(/^## In a browser session\n[\s\S]*?(?=^## )/m, '')
		.replaceAll('{{bin}}', 'open-conduit');
	return new Response(body, { headers: { 'Content-Type': 'text/markdown; charset=utf-8' } });
};
