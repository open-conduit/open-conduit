package tui

import "strings"

// where says which views a key works in.
type where uint8

const (
	inList where = 1 << iota
	inDetail
)

// Key is one key the view answers. The footers and the key table in the
// operating guide are both rendered from Keys, so they can't drift apart.
type Key struct {
	Key string
	// Label is the footer's word for it in the list and in a detail.
	List, Detail string
	// Does is the operating guide's description.
	Does  string
	where where
	// control keys are offered only when the source has operator controls.
	control bool
}

// Keys is every key the view offers, in footer order.
var Keys = []Key{
	{Key: "enter", List: "detail", Does: "show a run with all of its Work's turns", where: inList},
	{Key: "g", List: "every turn", Does: "switch between one row per issue or thread and one row per turn", where: inList},
	{Key: "l", List: "log", Detail: "log", Does: "the run's log", where: inList | inDetail},
	{Key: "t", List: "transcript", Detail: "transcript", Does: "the agent's transcript; in a log, the run's next output", where: inList | inDetail},
	{Key: "f", List: "filter", Does: "cycle the state filter (running, parked, failed, …)", where: inList},
	{Key: "r", List: "refresh", Detail: "refresh", Does: "refresh now", where: inList | inDetail},
	{Key: "esc", Detail: "back", Does: "in a detail or log, go back", where: inDetail},
	{Key: "q", List: "quit", Detail: "back", Does: "quit from the list; in a detail or log, go back (`ctrl+c` quits from anywhere)", where: inList | inDetail},
	{Key: "c", List: "scope actions", Does: "the scopes' actions, such as claiming an issue by hand", where: inList, control: true},
	{Key: "1-9", Detail: "action", Does: "in a Work's detail, the actions its extensions offer now", where: inDetail, control: true},
	{Key: "R", List: "run/retry", Detail: "run/retry", Does: "run or retry, after asking, since it starts a new turn", where: inList | inDetail, control: true},
	{Key: "H", List: "hold/release", Detail: "hold/release", Does: "hold, asking for a reason (`enter` to skip), or release", where: inList | inDetail, control: true},
	{Key: "a", List: "attach", Detail: "attach", Does: "attach to the agent's session", where: inList | inDetail, control: true},
	{Key: "x", List: "cancel run", Detail: "cancel run", Does: "cancel a live run, after asking", where: inList | inDetail, control: true},
	{Key: "X", List: "cancel work", Detail: "cancel work", Does: "cancel the whole Work, after asking", where: inList | inDetail, control: true},
}

// footer renders the keys for a view. controls says whether operator
// controls are offered and actions whether the Work has actions to pick;
// perTurn turns g's label to what it switches back to.
func footer(in where, controls, actions, perTurn bool) string {
	var parts []string
	for _, k := range Keys {
		if k.where&in == 0 || (k.control && !controls) || (k.Key == "1-9" && !actions) {
			continue
		}
		label := k.List
		if in == inDetail {
			label = k.Detail
		}
		if k.Key == "g" && perTurn {
			label = "by work"
		}
		parts = append(parts, k.Key+" "+label)
	}
	return strings.Join(parts, " · ")
}

// KeyTable renders Keys as the operating guide's Markdown table.
func KeyTable() string {
	var b strings.Builder
	b.WriteString("| Key | Does |\n| --- | --- |\n")
	for _, k := range Keys {
		key := "`" + k.Key + "`"
		if k.Key == "1-9" {
			key = "`1`–`9`"
		}
		b.WriteString("| " + key + " | " + k.Does + " |\n")
	}
	return b.String()
}
