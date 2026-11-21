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
  init --with EXECUTOR       start an executor's CLI on the setup guide (--with claude, --with codex)`},
		{"config", `  config check [--file F]    validate the config file offline (no daemon needed)
  config path | config show [--file F]
  config get POINTER | config set POINTER JSON | config unset POINTER   [--file F] (local file, validated before saving)
  config include add [FOLDER] --parent SCOPE | config include remove [FOLDER]   [--file F] (compose a folder's .open-conduit/config.json)
  config include add --name NAME --parent SCOPE | config include remove --name NAME   [--file F] (declare an include whose fragment is submitted to the daemon)
  config include show NAME | config include apply NAME FILE [--hash H] [--key K]   (read or apply a submitted include's fragment; apply needs configure covering its parent)
  config edit [--file F]     full-screen editor with the adapters' documented fields, validated before saving
  config validate FILE       validate against the running daemon's registrations (needs the daemon)
  config apply FILE [--generation N] [--key K]   make FILE the running daemon's next generation (needs the daemon;
                             --generation applies only if N is still current, for scripted compare-and-swap)
  config export [FILE]       write the running generation as a config file, secret references as references
                             (needs the daemon and the admin role; FILE defaults to the daemon's config file, - prints it;
                             never replaces a file)`},
		{"secret", `  secret set NAME [--store keyring|file] | secret path   (value read from stdin, never an argument)`},
		{"adapters", `  adapters [TYPE] [--json]   the registered integrations and the fields each takes`},
		{"serve", `  serve [--config PATH] [--toolbelt-dir DIR] [--lifecycle-log PATH] [--poll-interval 1s] [--lease-duration 1m] [--retention-interval 10m] [--sweep-interval 6h] [--call-timeout 5s]
        [--backup-interval 24h] [--backups-kept 7] [--backup-dir DIR]
                             (scheduled online backups into <data-dir>/backups; --backup-interval 0 turns them off)
        [--upgrade-bundles-kept 3] [--upgrade-bundles-max-age 0] [--no-upgrade-bundle]
                             (a first start on a newer record format takes a verified pre-upgrade-<time> bundle in the
                             backup directory and won't start without one; --no-upgrade-bundle skips it for this run)
        [--restore BUNDLE [--restore-start-config FILE] [--restore-artifacts NAME --restore-artifacts-location LOC [--restore-artifacts-command PATH]] [--restore-trust-key KEYS] [--restore-accept-unsigned]]
                             (restore BUNDLE in place before serving, as backup restore --in-place does, once:
                             a start that finds the directory restored from it already serves it as it is)
        [--webhook-addr 127.0.0.1:8787] [--tunnel TYPE [--tunnel-config JSON] | --public-url URL]
                             (override the config's settings.webhooks for this run; TYPE is a tunnel
                             a package provides, such as ngrok or cloudflared, and JSON its config)
        [--probe-addr 0.0.0.0:8080]
                             (also serve /livez and /readyz, with no credential, for an orchestrator's probes)
        [--no-tunnel] [--dry-run] [--no-telemetry] [--no-updates]
        (--webhook-addr takes any free local port; 8787 is only an example)
        (--no-telemetry, OPEN_CONDUIT_NO_TELEMETRY=1 or DO_NOT_TRACK=1 turn off the anonymous usage reports)
        (--no-updates: nothing installs a release through the daemon or its web view, for a binary a package
        manager, an image or a fleet manages; open-conduit upgrade still works on the machine)
        [--web] [--web-addr 127.0.0.1:7420] [--web-host open-conduit.localhost]
                             also serve the web view from the daemon (open-conduit web url prints its sign-in URL)
        [--web-subject-header HEADER --web-token-file FILE]
                             behind a sign-in proxy that passes each person's own credential in HEADER, act for
                             that person, with FILE's credential (auth add … --act-for operator) in place of yours
        [--management-addr HOST:PORT --management-tls-cert FILE --management-tls-key FILE [--management-client-ca FILE]]
        [--management-addr 127.0.0.1:PORT --management-trusted-proxy]
                             also serve the management API over the network: TLS (mutual TLS with a client CA),
                             or plaintext on loopback to a local proxy that terminates TLS; off by default`},
		{"service", `  service install [--no-start] [serve flags] | service uninstall|start|stop|status
                             run the daemon in the background (launchd on macOS, systemd --user on Linux)`},
	}},
	{"Observe", []helpEntry{
		{"health", `  health                     the daemon's state and components; exits 1 unless it is ready`},
		{"quiescence", `  quiescence                 what is in flight, or when the daemon is next due; exits 1 unless it is quiescent`},
		{"drain", `  drain [--reason TEXT] | drain status | drain end
                             stop starting attempts so the state can be moved; exits 1 until it is safe to move`},
		{"portability", `  portability                what is tied to this machine, before a move; exits 1 unless nothing is and it is drained`},
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
		{"web", `  web [--addr 127.0.0.1:7420] [--host open-conduit.localhost] [--subject-header HEADER] [--no-updates]   the same view in a browser on this machine (prints a sign-in URL); with --subject-header, it acts for the person a sign-in proxy passes the credential of in HEADER (its own credential needs --act-for operator); --no-updates leaves out the update page (installing a release from it needs the admin role)
  web url [--addr ADDR] [--host NAME]   print the web view's sign-in URL (it lasts across restarts)`},
	}},
	{"Control", []helpEntry{
		{"work", `  work create --scope S --kind K --binding B [--schema S --data JSON] [--key K]
  work cancel|retry|hold|release ID [--revision N] [--reason TEXT] [--key K]
  work interrupt ID [--revision N] [--reason TEXT] [--key K]   stop the running turn; the Work and its queued input stay for the next
  work rebind ID --binding B [--revision N] [--reason TEXT] [--key K]
  work attach ID [--keep-held] [--stream]
                                 hold, attach to the executor's session, release on exit;
                                 --stream (automatic over https) runs it through the daemon
  work wake ID --type T --delivery-id D --key-source S --key-external-id X [--revision N] [--schema S --data JSON] [--key K]`},
		{"container", `  container attach ID [--stream]
                             attach to held Work's session in a container, as work attach does for Work that ran in one`},
		{"claim", `  claim [SCOPE#]N [--scope S] [--entry NAME]   claim an issue by hand (the scope's claim action)`},
		{"actions", `  actions run PROVIDER ID (--scope S | --work W) [--input TEXT] [--key K]   invoke one as the local operator`},
		{"events", `  events skip ID [--reason TEXT] | events reroute ID [--reason TEXT]
  events emit --scope S --type T --delivery-id D [--key-source S --key-external-id X] [--work-id W] [--causation-id E] [--schema S --data JSON] [--key K]`},
		{"executions", `  executions cancel ID [--revision N] [--reason TEXT] [--key K]`},
		{"scopes", `  scopes reconcile ID [--revision N]`},
	}},
	{"Maintain", []helpEntry{
		{"auth", `  auth rotate | auth revoke TOKEN_ID [--reason TEXT] [--key K]
  auth operators | auth add NAME --out FILE [--role admin|configure|operate|view] [--grants SCOPE=ROLE,...] [--ttl DURATION] [--act-for KIND]   list operator credentials; issue one for a named operator (default role operate, or only its --grants: view, operate or configure at each scope's subtree), recorded as operator:NAME, that expires after --ttl (e.g. 8h) if given, and that may act for KIND:ID subjects with --act-for (operator: for operators, within both's authority)
  auth issuers | auth issuer set ID --type TYPE [--config JSON] [--groups JSON] [--grants JSON] | auth issuer remove ID   list, trust or stop trusting an identity provider issuer (admin); its credentials are recorded as operator:ID:SUBJECT, with the role --groups maps their groups to and the scoped roles --grants rows ({group, scope, role}) give them
  auth recover               (daemon stopped) revoke every operator credential and write a fresh token file`},
		{"backup", `  backup keygen FILE | backup create DEST [--unsigned] | backup verify BUNDLE | backup restore BUNDLE (--to DIR | --in-place) [--start-config FILE] [--artifacts NAME --artifacts-location LOC [--artifacts-command PATH]] [--trust-key KEYS] [--accept-unsigned] [--config FILE] | backup list DESTINATION | backup fetch DESTINATION BUNDLE DIR [--trust-key KEYS] [--accept-unsigned]   (create, verify and restore need the daemon stopped; list and fetch read a destination in settings.backup.destinations, and fetch needs a trust key or --accept-unsigned; a bundle is signed with settings.backup.signing_key and checked against settings.backup.trust_keys or --trust-key; --to writes a new state directory, --in-place replaces the daemon's own and moves what it replaces to before-restore-*; both put the artifacts into an empty store, write a fresh operator token and report why the config generation would not validate against the installed packages)`},
		{"state", `  state check [--json]       (daemon stopped) diagnose the state directory: permissions, schema, integrity, artifacts, drain status`},
		{"extension", `  extension list|inspect DIR|install DIR|update DIR|rollback NAME|remove NAME [--force]   manage extension packages (next serve)
  extension certify DIR [--json] [--report FILE]   run the conformance runner on a package`},
		{"dev", `  dev [DIR] [--secret NAME=VALUE|NAME=env:VAR]... [--entry TYPE=JSON]... [--agent PATH] [--web-addr 127.0.0.1:PORT]
                             run the package in DIR (default .) against a throwaway daemon and its web view,
                             rebuilding and restarting it when its files change; Ctrl-C stops everything`},
		{"upgrade", `  upgrade [--check] [--version V [--allow-unsigned]] [--no-restart] [--json]   install the latest release and restart the background service`},
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
	b.WriteString("Usage: open-conduit [--endpoint unix:///path | https://host:port] [--token-file PATH] [--socket PATH] [--data-dir DIR]\n")
	b.WriteString("                    [--ca-file PEM] [--client-cert PEM --client-key PEM] COMMAND\n")
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
var needsSubcommand = map[string]bool{"scopes": true, "work": true, "events": true, "executions": true, "waits": true, "artifacts": true, "config": true, "auth": true, "runs": true, "secret": true, "actions": true, "backup": true, "state": true, "container": true}

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

// HelpGroup is one heading of the global help and the commands under it,
// in the order `open-conduit --help` lists them. A command listed under
// several headings belongs to the first.
type HelpGroup struct {
	Title    string
	Commands []string
}

// HelpGroups lists the commands the help documents, for the generated CLI
// reference page (site/src/content/docs/reference/cli.md): each command's
// text is what `open-conduit COMMAND --help` prints.
func HelpGroups() []HelpGroup {
	seen := map[string]bool{}
	var out []HelpGroup
	for _, g := range helpGroups {
		hg := HelpGroup{Title: g.title}
		for _, e := range g.entries {
			if !seen[e.cmd] {
				seen[e.cmd] = true
				hg.Commands = append(hg.Commands, e.cmd)
			}
		}
		out = append(out, hg)
	}
	return out
}
