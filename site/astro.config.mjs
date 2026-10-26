// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// Published to GitHub Pages at https://open-conduit.github.io/open-conduit/.
export default defineConfig({
	site: 'https://open-conduit.github.io',
	base: '/open-conduit',
	integrations: [
		starlight({
			title: 'Open Conduit',
			description:
				'Run coding and support agents against GitHub issues, Slack threads, schedules and their own defect reports.',
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/open-conduit/open-conduit' },
			],
			editLink: {
				baseUrl: 'https://github.com/open-conduit/open-conduit/edit/main/site/',
			},
			sidebar: [
				{
					label: 'Start here',
					items: [
						{ label: 'What Open Conduit does', slug: 'start/overview' },
						{ label: 'Install', slug: 'start/install' },
						{ label: 'Quick start', slug: 'start/quick-start' },
					],
				},
				{
					label: 'Workflows',
					items: [
						{ label: 'GitHub engineering', slug: 'workflows/github' },
						{ label: 'Slack support', slug: 'workflows/slack' },
						{ label: 'Scheduled jobs', slug: 'workflows/scheduled-jobs' },
						{ label: 'Introspection', slug: 'workflows/introspection' },
					],
				},
				{
					label: 'Guides',
					items: [
						{ label: 'Configuration and secrets', slug: 'guides/configuration' },
						{ label: 'Run it in the background', slug: 'guides/background-service' },
						{ label: 'Watch and steer runs', slug: 'guides/operating' },
						{ label: 'Backups and recovery', slug: 'guides/backups' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'Extensions', slug: 'reference/extensions' },
						{ label: 'Platforms and files', slug: 'reference/platforms' },
						{ label: 'Getting help', slug: 'reference/help' },
					],
				},
			],
		}),
	],
});
