package management

import (
	"encoding/json"
	"slices"
	"sort"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Grant is a role at a scope: it lets an operator do what the role allows
// to that scope and to every scope beneath it, and nothing elsewhere. A
// scoped grant is view or operate; admin is only ever daemon-wide (an
// operator's role), since what it governs (config, secrets, operators)
// is the daemon's, not a scope's.
type Grant struct {
	Scope domain.ID `json:"scope"`
	Role  string    `json:"role"`
}

// maxGrants bounds the grants one credential or issuer carries.
const maxGrants = 64

// validGrants checks grants as a credential or issuer takes them: each a
// scope ID and view or operate, none repeated.
func validGrants(grants []Grant) bool {
	if len(grants) > maxGrants {
		return false
	}
	seen := map[domain.ID]bool{}
	for _, g := range grants {
		if !validScopeID(g.Scope) || (g.Role != RoleView && g.Role != RoleOperate) || seen[g.Scope] {
			return false
		}
		seen[g.Scope] = true
	}
	return true
}

// validScopeID is a scope ID a grant can name: 1 to 128 printable ASCII
// characters. The scope need not exist yet; a grant on a scope that
// doesn't covers nothing until it does.
func validScopeID(id domain.ID) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// mayAnywhere reports whether the operator holds role somewhere: daemon
// wide, or at least one grant of it.
func (p principal) mayAnywhere(role string) bool {
	if p.may(role) {
		return true
	}
	for _, g := range p.grants {
		if roleRank[g.Role] >= roleRank[role] {
			return true
		}
	}
	return false
}

// reachIn is the set of scopes the operator's grants of at least role
// cover in tx: each granted scope that exists, and every scope beneath
// it. A scope whose chain of parents can't be followed to a grant (a
// missing parent, a cycle) is not covered, so the check fails closed.
func reachIn(tx storage.Tx, grants []Grant, role string) (map[domain.ID]bool, error) {
	scopes, err := tx.ListScopes()
	if err != nil {
		return nil, err
	}
	granted := map[domain.ID]bool{}
	for _, g := range grants {
		if roleRank[g.Role] >= roleRank[role] {
			granted[g.Scope] = true
		}
	}
	parent := make(map[domain.ID]domain.ID, len(scopes))
	for _, sc := range scopes {
		parent[sc.ID] = sc.ParentID
	}
	out := map[domain.ID]bool{}
	for _, sc := range scopes {
		id := sc.ID
		for depth := 0; id != "" && depth <= len(scopes); depth++ {
			if granted[id] {
				out[sc.ID] = true
				break
			}
			next, ok := parent[id]
			if !ok {
				break
			}
			id = next
		}
	}
	return out, nil
}

// reachList is reach's scopes, sorted: what a query selects within.
func reachList(reach map[domain.ID]bool) []domain.ID {
	out := make([]domain.ID, 0, len(reach))
	for id := range reach {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// target is what a route acts on, which decides the scope an operator's
// grant must cover. The router resolves it before the handler reads or
// writes anything; every route declares one, and APIRoutes refuses a
// route without.
type target int

const (
	// targetUnset is a route that declared nothing: never served to an
	// operator.
	targetUnset target = iota
	// targetDaemon acts on the whole daemon (config, secrets, operators,
	// doctor, extension pages): it needs the route's role daemon-wide.
	targetDaemon
	// targetAny reveals nothing of any scope (health, the API schema,
	// rotating or revoking one's own credential): the role anywhere will
	// do.
	targetAny
	// targetList lists records of many scopes: the role anywhere, and the
	// list holds only what the operator's grants of it cover.
	targetList
	// targetScope, targetWork, targetExecution, targetEvent, targetWait
	// and targetArtifact name their record by the path's first ID; the
	// record's scope must be covered.
	targetScope
	targetWork
	targetExecution
	targetEvent
	targetWait
	targetArtifact
	// targetBodyScope is a request naming its scope in the body's
	// scope_id (sending an Event, creating Work).
	targetBodyScope
	// targetToolbelt is an execution's own: no operator reaches it.
	targetToolbelt
)

var targetNames = map[target]string{
	targetDaemon: "daemon", targetAny: "any", targetList: "list", targetScope: "scope", targetWork: "work",
	targetExecution: "execution", targetEvent: "event", targetWait: "wait", targetArtifact: "artifact",
	targetBodyScope: "body scope_id", targetToolbelt: "execution token",
}

// authorizeScoped is the check for an operator who doesn't hold the
// route's role daemon-wide: the route must be one a grant can reach, a
// grant of the role must cover its target's scope, and a list keeps only
// what grants cover (c.reach). A target that can't be resolved is refused:
// a record that doesn't exist is 404, as it would be anyway, and one
// outside every grant is 403 forbidden_scope.
func (s *Server) authorizeScoped(c *call, rt *route, need string) error {
	if rt.target == targetDaemon || rt.target == targetUnset || !c.who.mayAnywhere(need) {
		return forbiddenRole(c.who, need)
	}
	if rt.target == targetAny {
		return nil
	}
	return s.read(c.ctx, func(tx storage.Tx) error {
		reach, err := reachIn(tx, c.who.grants, need)
		if err != nil {
			return err
		}
		c.reach = reach
		if rt.target == targetList {
			return nil
		}
		scope, err := targetScopeIn(tx, rt.target, c)
		if err != nil {
			return err
		}
		if !reach[scope] {
			return forbiddenScope(c.who, need, rt.target)
		}
		return nil
	})
}

// targetScopeIn resolves the scope of what c's route names.
func targetScopeIn(tx storage.Tx, t target, c *call) (domain.ID, error) {
	id := ""
	if len(c.params) > 0 {
		id = c.params[0]
	}
	workScope := func(work domain.ID) (domain.ID, error) {
		w, err := tx.Work(work)
		return w.ScopeID, err
	}
	switch t {
	case targetScope:
		sc, err := tx.Scope(domain.ID(id))
		return sc.ID, err
	case targetWork:
		return workScope(domain.ID(id))
	case targetExecution:
		x, err := tx.Execution(domain.ID(id))
		if err != nil {
			return "", err
		}
		return workScope(x.WorkID)
	case targetWait:
		w, err := tx.Wait(domain.ID(id))
		if err != nil {
			return "", err
		}
		return workScope(w.WorkID)
	case targetEvent:
		a, err := tx.Event(domain.ID(id))
		if err != nil {
			return "", err
		}
		scope, err := runtime.EventScopeInTx(tx, a)
		if err == nil && scope == "" {
			// Not yet resolved to a scope: no grant covers it.
			return "", fail(403, "forbidden_scope", "this Event's scope isn't resolved yet; it needs a daemon-wide role")
		}
		return scope, err
	case targetArtifact:
		xs, err := tx.ListExecutions("")
		if err != nil {
			return "", err
		}
		for _, x := range xs {
			for _, a := range x.AllArtifacts() {
				if a.ID == id {
					return workScope(x.WorkID)
				}
			}
		}
		return "", storage.ErrNotFound
	case targetBodyScope:
		var in struct {
			ScopeID string `json:"scope_id"`
		}
		if json.Unmarshal(c.body, &in) != nil || in.ScopeID == "" {
			return "", fail(400, "malformed", "scope_id is required")
		}
		return domain.ID(in.ScopeID), nil
	}
	return "", fail(403, "forbidden_scope", "this route has no scope a grant can cover")
}

func forbiddenScope(p principal, role string, t target) error {
	return fail(403, "forbidden_scope", "operator "+p.name+" holds no grant of "+role+" covering this "+targetNames[t]+"'s scope")
}

// sees reports whether a list may show a record of scope: every scope for
// an operator holding the role daemon-wide (no reach), else only those its
// grants cover.
func (c *call) sees(scope domain.ID) bool { return c.reach == nil || c.reach[scope] }

// scopes is the reach as a query's scope list, nil for every scope.
func (c *call) scopes() []domain.ID {
	if c.reach == nil {
		return nil
	}
	return reachList(c.reach)
}

// grantsOf is the grants the groups map to in an issuer's table.
func grantsOf(table []groupGrant, groups []string) []Grant {
	best := map[domain.ID]string{}
	for _, gg := range table {
		if slices.Contains(groups, gg.Group) && roleRank[gg.Role] > roleRank[best[gg.Scope]] {
			best[gg.Scope] = gg.Role
		}
	}
	out := make([]Grant, 0, len(best))
	for scope, role := range best {
		out = append(out, Grant{Scope: scope, Role: role})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

// groupGrant is one row of an issuer's scoped group table: members of
// Group hold Role at Scope.
type groupGrant struct {
	Group string    `json:"group"`
	Scope domain.ID `json:"scope"`
	Role  string    `json:"role"`
}

// readGrants is the shape GET /auth/operators and the issuer routes show
// grants in: never nil.
func readGrants(gs []Grant) []Grant {
	if gs == nil {
		return []Grant{}
	}
	return gs
}
