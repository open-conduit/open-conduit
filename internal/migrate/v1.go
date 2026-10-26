package migrate

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The Open Supervisor v1 configuration (config.yaml) and repository file
// (.open-supervisor.yaml) as read by the pinned baseline's internal/config.
// These are private input shapes: nothing here is Open Conduit vocabulary.

type v1Config struct {
	Version     int                       `yaml:"version"`
	Settings    v1Settings                `yaml:"settings"`
	Connections map[string]map[string]any `yaml:"connections"`
	Projects    []*v1Project              `yaml:"projects"`
	Sources     []*v1Source               `yaml:"sources"`
}

type v1Settings struct {
	MaxTurns              int      `yaml:"maxTurns"`
	MaxOpen               int      `yaml:"maxOpenPerProject"`
	ReconcileMinutes      int      `yaml:"reconcileMinutes"`
	Port                  int      `yaml:"port"`
	Agent                 string   `yaml:"agent"`
	Telemetry             []string `yaml:"telemetry"`
	Tunnel                string   `yaml:"tunnel"`
	Labels                v1Labels `yaml:"labels"`
	Branch                string   `yaml:"branch"`
	Git                   v1Git    `yaml:"git"`
	SweepAt               *string  `yaml:"sweepAt"`
	TriageAt              *string  `yaml:"triageAt"`
	Jobs                  *bool    `yaml:"jobs"`
	UsageMinutes          int      `yaml:"usageMinutes"`
	MaxAttempts           int      `yaml:"maxAttempts"`
	ChecksDebounceSeconds int      `yaml:"checksDebounceSeconds"`
	NoChecksMinutes       int      `yaml:"noChecksMinutes"`
	HandoffMinutes        *int     `yaml:"handoffMinutes"`
	HandoffShare          *float64 `yaml:"handoffShare"`
	Host                  v1Host   `yaml:"host"`
	DryRun                bool     `yaml:"dryRun"`
}

type v1Host struct {
	Minutes int      `yaml:"minutes"`
	Load    float64  `yaml:"load"`
	Memory  float64  `yaml:"memory"`
	Disk    float64  `yaml:"disk"`
	Disks   []string `yaml:"disks"`
}

type v1Labels struct {
	Claimed   string   `yaml:"claimed"`
	Scheduled string   `yaml:"scheduled"`
	Skip      []string `yaml:"skip"`
	Require   []string `yaml:"require"`
	Help      string   `yaml:"help"`
	Review    string   `yaml:"review"`
	Done      string   `yaml:"done"`
	Unrouted  string   `yaml:"unrouted"`
}

// v1DefaultLabels are v1's DefaultSettings labels, which Open Conduit's
// GitHub and tracker extensions also default to.
func v1DefaultLabels() v1Labels {
	return v1Labels{Claimed: "agent-in-progress", Scheduled: "scheduled", Skip: []string{"backlog", "human", "milestone"},
		Help: "agent-needs-help", Review: "ready-for-review", Done: "might-be-done", Unrouted: "needs-routing"}
}

func (l v1Labels) merge(base v1Labels) v1Labels {
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	out := v1Labels{Claimed: pick(l.Claimed, base.Claimed), Scheduled: pick(l.Scheduled, base.Scheduled), Help: pick(l.Help, base.Help),
		Review: pick(l.Review, base.Review), Done: pick(l.Done, base.Done), Unrouted: pick(l.Unrouted, base.Unrouted), Skip: l.Skip, Require: l.Require}
	if out.Skip == nil {
		out.Skip = base.Skip
	}
	if out.Require == nil {
		out.Require = base.Require
	}
	return out
}

type v1Git struct {
	Remote string `yaml:"remote"`
	Sign   bool   `yaml:"sign"`
}

type v1Binding struct {
	Connection string
	Values     map[string]any
}

func (b *v1Binding) UnmarshalYAML(n *yaml.Node) error {
	var m map[string]any
	if err := n.Decode(&m); err != nil {
		return err
	}
	b.Connection, _ = m["connection"].(string)
	delete(m, "connection")
	b.Values = m
	return nil
}

func (b *v1Binding) get(key string) string {
	if b == nil {
		return ""
	}
	return str(b.Values[key])
}

type v1Project struct {
	Name       string            `yaml:"name"`
	Root       string            `yaml:"root"`
	Agent      string            `yaml:"agent"`
	Repo       *v1Binding        `yaml:"repo"`
	Tracker    *v1Binding        `yaml:"tracker"`
	Labels     *v1Labels         `yaml:"labels"`
	Branch     string            `yaml:"branch"`
	Git        *v1Git            `yaml:"git"`
	BaseBranch string            `yaml:"baseBranch"`
	Worktree   v1Worktree        `yaml:"worktree"`
	Sweep      v1Sweep           `yaml:"sweep"`
	Triage     v1Triage          `yaml:"triage"`
	Jobs       []v1Job           `yaml:"jobs"`
	Env        map[string]string `yaml:"env"`
	Disabled   bool              `yaml:"disabled"`
}

type v1Worktree struct {
	Copy      []string `yaml:"copy"`
	Setup     string   `yaml:"setup"`
	SetupWhen []string `yaml:"setupWhen"`
}

type v1Sweep struct {
	Disabled  bool     `yaml:"disabled"`
	RuleFiles []string `yaml:"ruleFiles"`
	Guide     string   `yaml:"guide"`
	Land      string   `yaml:"land"`
}

type v1Triage struct {
	Disabled   bool   `yaml:"disabled"`
	OpenIssues bool   `yaml:"openIssues"`
	Land       string `yaml:"land"`
}

type v1Job struct {
	Name           string        `yaml:"name"`
	Command        []string      `yaml:"command"`
	EveryMinutes   int           `yaml:"everyMinutes"`
	TimeoutMinutes int           `yaml:"timeoutMinutes"`
	At             []string      `yaml:"at"`
	Cron           string        `yaml:"cron"`
	TimeZone       string        `yaml:"timeZone"`
	Status         string        `yaml:"status"`
	Checkout       string        `yaml:"checkout"`
	KeepRuns       int           `yaml:"keepRuns"`
	On             *v1JobTrigger `yaml:"on"`

	// origin is where the job was declared (config or repository file).
	origin string
}

type v1JobTrigger struct {
	Events   []string `yaml:"events"`
	Branches []string `yaml:"branches"`
	Paths    []string `yaml:"paths"`
	Webhook  bool     `yaml:"webhook"`
}

type v1Source struct {
	Name     string     `yaml:"name"`
	Tracker  *v1Binding `yaml:"tracker"`
	Routes   []v1Route  `yaml:"routes"`
	Unrouted string     `yaml:"unrouted"`
	Labels   *v1Labels  `yaml:"labels"`
}

type v1Route struct {
	Project string    `yaml:"project"`
	When    yaml.Node `yaml:"when"`
}

func parseV1(data []byte) (*v1Config, error) {
	var c v1Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		// yaml errors carry line numbers and token text, never values of
		// unrelated fields; keep only the first line.
		msg, _, _ := strings.Cut(err.Error(), "\n")
		return nil, fmt.Errorf("open supervisor config: %s", msg)
	}
	if c.Version != 0 && c.Version != 1 {
		return nil, fmt.Errorf("open supervisor config: version %d is not supported (expected 1)", c.Version)
	}
	if c.Connections == nil {
		c.Connections = map[string]map[string]any{}
	}
	return &c, nil
}

// parseRepoFile reads a checkout's .open-supervisor.yaml jobs, as v1 did.
func parseRepoFile(data []byte) ([]v1Job, error) {
	var own struct {
		Jobs []v1Job `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &own); err != nil {
		msg, _, _ := strings.Cut(err.Error(), "\n")
		return nil, fmt.Errorf("%s", msg)
	}
	return own.Jobs, nil
}

// match is a v1 route `when` reduced to the attribute lists a tracker route's
// match takes; complex is set when it used any/all/not or a nested value.
func match(n yaml.Node) (fields map[string][]string, complex bool) {
	if n.Kind == 0 {
		return nil, false
	}
	if n.Kind != yaml.MappingNode {
		return nil, true
	}
	fields = map[string][]string{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		switch key {
		case "any", "all", "not":
			complex = true
			continue
		}
		switch val.Kind {
		case yaml.ScalarNode:
			fields[key] = []string{val.Value}
		case yaml.SequenceNode:
			for _, c := range val.Content {
				if c.Kind != yaml.ScalarNode {
					complex = true
					continue
				}
				fields[key] = append(fields[key], c.Value)
			}
		default:
			complex = true
		}
	}
	return fields, complex
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
