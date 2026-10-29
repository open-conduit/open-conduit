// Display names for sources the prototype knows; any other source (a new
// extension) shows as its own name, so nothing here is required.
const NAMES: Record<string, string> = { github: 'GitHub', slack: 'Slack', schedule: 'Schedule', introspection: 'Introspection', core: 'Core' }
export const sourceLabel = (s: string) => NAMES[s] ?? s
