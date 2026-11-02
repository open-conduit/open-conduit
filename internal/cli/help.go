package cli

import (
	"fmt"
	"io"
	"strings"
)

// helpEntry is one command's lines in the help: cmd is the command word
// they belong to, text the usage and its notes as printed (each line
// already indented by two spaces).
type helpEntry struct {
	cmd, text string
}

// helpGroup is one heading of the global help.
type helpGroup struct {
	title   string
	entries []helpEntry
}

// helpGroups is the command reference: `open-conduit --help` prints every
// group, and `open-conduit COMMAND --help` the entries for COMMAND from
// whichever groups list it.
var helpGroups = []helpGroup{
	{"Set up", []helpEntry{
		{"init", `  init                       list the setups the registered extensions offer
  init TYPE [flags] [--config PATH] [--force] [--dry-run]   run one (for example init github --repo OWNER/NAME, init slack)
  init --with claude         start Claude Code with the setup guide`},
		{"config", `  config check [--file F]    validate the config file offline (no daemon needed)
  config path | config show [--file F]
  config get POINTER | config set POINTER JSON | config unset POINTER   [--file F] (local file, validated before saving)
  config include add [FOLDER] --parent SCOPE | config include remove [FOLDER]   [--file F] (compose a folder's .open-conduit/config.json)
  config edit [--file F]     full-screen editor with the adapters' documented fields, validated before saving
  config validate FILE       validate against the running daemon's registrations (needs the daemon)
  config apply FILE [--generation N] [--key K]   make FILE the running daemon's next generation (needs the daemon;
                             --generation applies only if N is still current, for scripted compare-and-swap)`},
		{"secret", `  secret set NAME [--store keyring|file] | secret path   (value read from stdin, never an argument)`},
		{"adapters", `  adapters [TYPE] [--json]   the registered integrations and the fields each takes`},
		{"serve", `  serve [--config PATH] [--toolbelt-dir DIR] [--lifecycle-log PATH] [--poll-interval 1s] [--lease-duration 1m] [--retention-interval 10m] [--sweep-interval 6h]
        [--webhook-addr 127.0.0.1:8787] [--tunnel ngrok|cloudflared|static] [--tunnel-domain D] [--tunnel-name N] [--tunnel-hostname H] [--public-url URL]
                             (override the config's settings.webhooks for this run)
        [--no-tunnel] [--dry-run] [--claude-usage] [--no-telemetry]
        (--webhook-addr takes any free local port; 8787 is only an example)
        (--no-telemetry, OPEN_CONDUIT_NO_TELEMETRY=1 or DO_NOT_TRACK=1 turn off the anonymous usage reports)
        [--web] [--web-addr 127.0.0.1:7420] [--web-host open-conduit.localhost]
                             also serve the web view from the daemon (open-conduit web url prints its sign-in URL)`},
		{"service", `  service install [--no-start] [serve flags] | service uninstall|start|stop|status
                             run the daemon in the background (launchd on macOS, systemd --user on Linux)`},
	}},
	{"Observe", []helpEntry{
		{"health", `  health                     whether the daemon answers`},
		{"doctor", `  doctor [--check-secrets] [--json]   check the config, its bindings, the agent CLIs and the daemon`},
		{"scopes", `  scopes list [--parent-id ID] [--enabled true|false] | scopes get ID`},
		{"work", `  work list [--scope S --state --kind --parent-id] [--json] | work get ID`},
		{"runs", `  runs list [--scope S --work-id --state --executor-binding --executor --work-kind --outcome] [--json] | runs get ID
  runs log ID [--follow] [--transcript] [--range bytes=A-B]`},
		{"events", `  events list [--scope S --work-id --type --after-sequence] [--json] | events get ID | events chain ID`},
		{"executions", `  executions list [--scope S --work-id --state] [--json] | executions get ID`},
		{"waits", `  waits list [--scope S --work-id --execution-id --state] [--json] | waits get ID`},
		{"artifacts", `  artifacts list [--work-id --execution-id --kind] [--json] | artifacts cat ID [--range bytes=A-B]`},
		{"route", `  route KEY [--scope S] [--limit N]   explain how a key's Events were routed (KEY: ENG-12, issue/ENG-12, #12 or an external ID)
  route --trace TYPE|SAMPLE --scope S [--source SRC] [--key-source S --key-external-id X] [--work-id W] [--schema S --data JSON]
                             dry-run how an Event would route now (nothing is recorded)
  route --samples --scope S  the sample Events the scope's routes offer for --trace`},
		{"actions", `  actions list (--scope S | --work W)   the actions extensions offer there now`},
		{"tui", `  tui                        the run history in the terminal`},
		{"web", `  web [--addr 127.0.0.1:7420] [--host open-conduit.localhost]   the same view in a browser on this machine (prints a sign-in URL)
  web url [--addr ADDR] [--host NAME]   print the web view's sign-in URL (it lasts across restarts)`},
	}},
	{"Control", []helpEntry{
		{"work", `  work create --scope S --kind K --binding B [--schema S --data JSON] [--key K]
  work cancel|retry|hold|release ID [--revision N] [--reason TEXT] [--key K]
  work rebind ID --binding B [--revision N] [--reason TEXT] [--key K]
  work attach ID [--keep-held]   hold, attach to the executor's session, release on exit
  work wake ID --type T --delivery-id D --key-source S --key-external-id X [--revision N] [--schema S --data JSON] [--key K]`},
		{"container", `  container attach ID        attach to held Work's session in a container, as work attach does for Work that ran in one`},
		{"claim", `  claim [SCOPE#]N [--scope S] [--entry NAME]   claim an issue by hand (the scope's claim action)`},
		{"actions", `  actions run PROVIDER ID (--scope S | --work W) [--input TEXT] [--key K]   invoke one as the local operator`},
		{"events", `  events emit --scope S --type T --delivery-id D [--key-source S --key-external-id X] [--work-id W] [--causation-id E] [--schema S --data JSON] [--key K]`},
		{"executions", `  executions cancel ID [--revision N] [--reason TEXT] [--key K]`},
		{"scopes", `  scopes reconcile ID [--revision N]`},
	}},
	{"Maintain", []helpEntry{
		{"auth", `  auth rotate | auth revoke TOKEN_ID [--reason TEXT] [--key K]
  auth recover               (daemon stopped) revoke every operator credential and write a fresh token file`},
		{"migrate", `  migrate config --from V1_CONFIG [--out FILE | --write] [--force] [--no-repo-files] [--time-zone ZONE] [--shell-jobs] [--detached-timeout S] [--json]
  migrate upgrade [--config FILE] [--write] [--json]
                             convert an Open Supervisor config (preview unless --out/--write; v1 files are never modified)`},
		{"backup", `  backup create DEST | backup verify BUNDLE | backup restore BUNDLE --to DIR   (daemon stopped; restore writes a new state directory)`},
		{"state", `  state check [--json]       (daemon stopped) diagnose the state directory: permissions, schema, integrity, artifacts, drain status`},
		{"extension", `  extension list|inspect DIR|install DIR|update DIR|rollback NAME|remove NAME [--force]   manage extension packages (next serve)
  extension certify DIR [--json] [--report FILE]   run the conformance runner on a package`},
		{"dev", `  dev [DIR] [--secret NAME=VALUE|NAME=env:VAR]... [--entry TYPE=JSON]... [--agent PATH] [--web-addr 127.0.0.1:PORT]
                             run the package in DIR (default .) against a throwaway daemon and its web view,
                             rebuilding and restarting it when its files change; Ctrl-C stops everything`},
		{"upgrade", `  upgrade [--check] [--version V] [--no-restart] [--json]   install the latest release and restart the background service`},
		{"version", `  version`},
	}},
}

const helpFooter = `
Lists print a table; --json prints the records as JSON. Lists accept --limit and --cursor.
Controls without --revision act on the current revision; pass --revision N to act only if it is still N.
Mutations accept --key to retry safely with the same Idempotency-Key.
Run open-conduit COMMAND --help for one command.
`

// operatorHelp is `open-conduit --help`: every command, grouped.
var operatorHelp = func() string {
	var b strings.Builder
	b.WriteString("open-conduit — Open Conduit operator client\n\n")
	b.WriteString("Usage: open-conduit [--endpoint unix:///path] [--token-file PATH] [--socket PATH] [--data-dir DIR] COMMAND\n")
	for _, g := range helpGroups {
		fmt.Fprintf(&b, "\n%s:\n", g.title)
		for _, e := range g.entries {
			b.WriteString(e.text)
			b.WriteString("\n")
		}
	}
	b.WriteString(helpFooter)
	return b.String()
}()

// commandHelp is `open-conduit COMMAND --help`, or "" for a command the
// help does not list.
func commandHelp(cmd string) string {
	var b strings.Builder
	for _, g := range helpGroups {
		for _, e := range g.entries {
			if e.cmd == cmd {
				b.WriteString(e.text)
				b.WriteString("\n")
			}
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "Usage of open-conduit " + cmd + " (global flags: open-conduit --help):\n\n" + b.String() + helpFooter
}

// needsSubcommand are the commands that do nothing alone, so a bare one
// prints its help.
var needsSubcommand = map[string]bool{"scopes": true, "work": true, "events": true, "executions": true, "waits": true, "artifacts": true, "config": true, "auth": true, "runs": true, "secret": true, "actions": true, "backup": true, "state": true, "container": true, "migrate": true}

// helpRequested reports whether args ask for help: --help, -h or -help
// anywhere before a "--", or a leading "help".
func helpRequested(args []string) bool {
	if len(args) > 0 && args[0] == "help" {
		return true
	}
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "--help", "-h", "-help":
			return true
		}
	}
	return false
}

// printHelp writes cmd's help for `open-conduit COMMAND --help`; commands
// with a fuller help of their own print that.
func (o *operator) printHelp(w io.Writer, cmd string, args []string) {
	switch cmd {
	case "service":
		fmt.Fprint(w, serviceHelp)
	case "dev":
		fmt.Fprint(w, devHelp)
	case "extension":
		fmt.Fprint(w, extensionHelp)
	case "upgrade":
		fmt.Fprint(w, upgradeHelp)
	case "init":
		o.initHelp(w, args)
	default:
		fmt.Fprint(w, commandHelp(cmd))
	}
}
