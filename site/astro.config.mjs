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
						{ label: 'Point your agent here', slug: 'start/agents' },
					],
				},
				{
					label: 'Workflows',
					items: [
						{ label: 'GitHub engineering', slug: 'workflows/github' },
						{ label: 'Slack support', slug: 'workflows/slack' },
						{ label: 'Scheduled jobs', slug: 'workflows/scheduled-jobs' },
						{ label: 'Introspection', slug: 'workflows/introspection' },
						{ label: 'Human decisions', slug: 'workflows/human-decisions' },
						{ label: 'Observed sources', slug: 'workflows/observed-sources' },
						{ label: 'Fix red CI on main', slug: 'workflows/red-ci' },
						{ label: 'Slack to PR', slug: 'workflows/slack-to-pr' },
						{ label: 'Dependency upgrades', slug: 'workflows/dependency-upgrades' },
						{ label: 'Multi-repo changes', slug: 'workflows/multi-repo' },
					],
				},
				{
					label: 'Guides',
					items: [
						{ label: 'Configuration and secrets', slug: 'guides/configuration' },
						{ label: 'Give agents their own GitHub account', slug: 'guides/github-bot' },
						{ label: 'Sandbox your agents', slug: 'guides/sandboxes' },
						{ label: 'Run it in the background', slug: 'guides/background-service' },
						{ label: 'Watch and steer runs', slug: 'guides/operating' },
						{ label: 'Backups and recovery', slug: 'guides/backups' },
						{ label: 'Build your first extension', slug: 'guides/first-extension' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'Extensions', slug: 'reference/extensions' },
						{ label: 'Platforms and files', slug: 'reference/platforms' },
						{ label: 'Usage telemetry', slug: 'reference/telemetry' },
						{ label: 'Licensing', slug: 'reference/licensing' },
						{ label: 'Getting help', slug: 'reference/help' },
					],
				},
				{
					label: 'Architecture',
					collapsed: true,
					items: [
						{ label: 'About these pages', slug: 'architecture' },
						{ label: 'Core principles', slug: 'architecture/principles' },
						{ label: 'The core in one page', slug: 'architecture/core' },
						{ label: 'Performance', slug: 'architecture/performance' },
						{
							label: 'Design decisions',
							collapsed: true,
							items: [
								{ label: 'B01 Package boundaries', slug: 'architecture/packages' },
								{ label: 'B02 Storage contracts', slug: 'architecture/domain-storage' },
								{ label: 'B03 Extension contracts', slug: 'architecture/extensions' },
								{ label: 'B04 Runtime semantics', slug: 'architecture/runtime-semantics' },
								{ label: 'B05 Configuration and management', slug: 'architecture/config-management' },
								{ label: 'B06 Contracts and checks', slug: 'architecture/contracts-gate' },
								{ label: 'B07 Workspaces', slug: 'architecture/workspaces' },
								{ label: 'B08 Extension protocol', slug: 'architecture/extension-protocol' },
							],
						},
						{
							label: 'Runtime',
							collapsed: true,
							items: [
								{ label: 'SQLite and artifacts', slug: 'architecture/sqlite-artifacts' },
								{ label: 'Backup bundles', slug: 'architecture/backup-bundles' },
								{ label: 'Event ingress', slug: 'architecture/event-ingress' },
								{ label: 'Routing and Work lifecycle', slug: 'architecture/routing-lifecycle' },
								{ label: 'Execution scheduling', slug: 'architecture/execution-scheduling' },
								{ label: 'Timers and waits', slug: 'architecture/timers-waits' },
								{ label: 'Toolbelt and management', slug: 'architecture/toolbelt-management' },
								{ label: 'Shell executor', slug: 'architecture/shell-executor' },
								{ label: 'Telemetry and run history', slug: 'architecture/observability-history' },
								{ label: 'Daemon and drills', slug: 'architecture/daemon-core-gate' },
							],
						},
						{
							label: 'Workloads and integrations',
							collapsed: true,
							items: [
								{ label: 'GitHub', slug: 'architecture/github-extension' },
								{ label: 'Claude Code executor', slug: 'architecture/claude-code-executor' },
								{ label: 'Engineering tools', slug: 'architecture/engineering-tools' },
								{ label: 'Slack', slug: 'architecture/slack-extension' },
								{ label: 'Scheduled jobs', slug: 'architecture/schedule-extension' },
								{ label: 'Introspection', slug: 'architecture/issue-reporting' },
								{ label: 'Observed sources', slug: 'architecture/observe-extension' },
								{ label: 'Operator setup', slug: 'architecture/operator-setup' },
								{ label: 'Installed integrations', slug: 'architecture/installed-integrations' },
								{ label: 'Backup and recovery', slug: 'architecture/recovery' },
								{ label: 'Extension packaging', slug: 'architecture/extension-packaging' },
							],
						},
						{
							label: 'Proposals',
							collapsed: true,
							items: [
								{ label: 'Setup sessions', slug: 'architecture/setup-sessions' },
							],
						},
					],
				},
			],
		}),
	],
});
