import type { APIRoute } from 'astro';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// The setup guide `open-conduit init --with claude` starts Claude Code on,
// published as plain Markdown for any agent to follow (start/agents links
// it). It is read at build time from the setup package's own copy: the
// github.com/open-conduit/extension-setup release go.mod pins, the one the
// release archive bundles, so the two never drift. The browser-session
// section is dropped: it only applies inside the web view's Setup page.
const setupModule = 'github.com/open-conduit/extension-setup';

function playbook(): string {
	const dir = execFileSync('go', ['list', '-m', '-f', '{{.Dir}}', setupModule], { cwd: resolve(process.cwd(), '..'), encoding: 'utf8' }).trim();
	return readFileSync(resolve(dir, 'playbooks/setup.md'), 'utf8');
}

export const GET: APIRoute = () => {
	const body = playbook()
		.replace(/^---\n[\s\S]*?\n---\n+/, '')
		.replace(/^## In a browser session\n[\s\S]*?(?=^## )/m, '')
		.replaceAll('{{bin}}', 'open-conduit');
	return new Response(body, { headers: { 'Content-Type': 'text/markdown; charset=utf-8' } });
};
