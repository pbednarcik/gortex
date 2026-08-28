package resolver

import (
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Member-level C# interface-dispatch synthesis: the implements-family cascade.
//
// Roslyn — the reference C# resolver — treats an interface method and every
// method that implements it (directly, or through a base class that implements
// the interface) as ONE linked family, and reports the union of the family's
// call sites for every member. Two mechanisms feed that union:
//
//  1. Through-interface calls: `x.Convert(1)` where `x` is typed as the
//     interface binds to the interface member node. Those calls must surface
//     on every concrete implementation.
//  2. Sibling implementation calls: a converter's own `Convert(-number)`
//     (a self/recursive or same-class call) binds directly to that class's
//     method node — it never touches the interface node. Roslyn still reports
//     that site for the interface method AND for every sibling implementation.
//
// A fan-out anchored only on calls bound to the interface member (mechanism 1)
// misses the dominant mass of real-corpus usages, which are mechanism 2. This
// pass therefore builds the full implements-family per (interface, method
// name) — the interface member plus the same-named method on every type whose
// implements/extends chain reaches the interface — and, for every call edge
// bound to ANY family member, synthesizes call edges to ALL other members.
//
// Tier: ast_inferred / ConfidenceTyped (non-speculative, type-keyed) — the
// same tier the sibling one-to-many dispatch passes use (MediatR Publish ->
// every handler, Spring publishEvent -> every listener), so the cascade rides
// in the DEFAULT find_usages / get_callers result. Family membership is
// established strictly through the implements/extends chain — never by name
// matching alone — so unrelated same-named methods are never linked.

// csharpIfaceDispatchCap bounds the family size (every interface-member
// overload node plus every implementing method node). C# overloads mint one
// node each, so a broadly-localised interface — one implementation per locale,
// several overloads per class — legitimately runs to ~70+ member nodes
// (Humanizer's INumberToWordsConverter.Convert family measures 72) and is
// exactly the shape this pass exists to cover, so the cap sits above it with
// headroom; a family wider than the cap is dropped whole as noise
// (pathological hub interfaces like a monorepo-wide Dispose).
const csharpIfaceDispatchCap = 128

// MetaViaMethodSetInference is the Meta["via"] marker the resolver stamps on
// EdgeImplements edges minted by structural method-set inference (as opposed
// to a source-declared base list). Hierarchy-walking passes that must follow
// only declared subtyping filter on it.
const MetaViaMethodSetInference = "method-set-inference"

// csharpCallSiteKey identifies one attributed call site. Line is part of the
// key on purpose: ground truth is line-based, so every call-site line of every
// family member must fan out to every other member, not one edge per
// (caller, callee) pair.
func csharpCallSiteKey(from, to, filePath string, line int) string {
	return from + "\x00" + to + "\x00" + filePath + "\x00" + strconv.Itoa(line)
}

// ResolveCSharpInterfaceDispatch fans every call bound to a member of a C#
// implements-family out to all other members of that family. Returns the
// number of fan-out edges landed.
func ResolveCSharpInterfaceDispatch(g graph.Store) int {
	return ResolveCSharpInterfaceDispatchScoped(g, nil)
}

// ResolveCSharpInterfaceDispatchScoped limits partial work to changed
// repositories plus the in-repo interface families targeted by their calls.
// Incoming calls to those exact family members form the reverse dependency
// frontier. A nil scope preserves the full/cold whole-graph behavior.
func ResolveCSharpInterfaceDispatchScoped(g graph.Store, scope map[string]bool) int {
	if g == nil {
		return 0
	}
	familyScope := scope
	scopedSourceCalls := []*graph.Edge(nil)
	if scope != nil {
		familyScope = make(map[string]bool, len(scope))
		for prefix, enabled := range scope {
			if enabled {
				familyScope[prefix] = true
			}
		}
		scopedSourceCalls = frameworkRepoEdges(g, scope, graph.EdgeCalls)
		targetIDs := make([]string, 0, len(scopedSourceCalls))
		for _, edge := range scopedSourceCalls {
			if edge != nil && !graph.IsUnresolvedTarget(edge.To) {
				targetIDs = append(targetIDs, edge.To)
			}
		}
		for _, target := range g.GetNodesByIDs(targetIDs) {
			if target != nil && target.Language == "csharp" && target.Kind == graph.KindMethod {
				familyScope[target.RepoPrefix] = true
			}
		}
	}

	// Subtype adjacency over the resolved type hierarchy: super → subs.
	// EdgeImplements and EdgeExtends both count — a class reaches an interface
	// through any chain of base classes / base interfaces (e.g. Afrikaans
	// extends Genderless which implements INumberToWordsConverter).
	//
	// Only SOURCE-DECLARED hierarchy edges qualify. The method-set inference
	// pass mints EdgeImplements from every type whose bare method names cover
	// an interface — with a single-method interface like IOrdinalizer.Convert
	// that "links" every Convert-bearing class in the repo, and a family built
	// over it would union unrelated hierarchies (NumberToWords converters into
	// the Ordinalizer family). Those edges carry the inference marker; skip
	// them. Origin cannot discriminate here: it is stamped/backfilled at
	// different pipeline stages, so declared and inferred edges converge.
	// This pass can run BEFORE the resolver has bound base-list targets (the
	// pipeline settles hierarchy targets across several later passes), so an
	// `unresolved::Name` target is resolved here by an exact, same-repo,
	// unique type/interface name lookup — ambiguity means skip, never guess.
	hierarchyEdges := frameworkRepoEdges(g, familyScope, graph.EdgeImplements, graph.EdgeExtends)
	hierarchySourceIDs := make([]string, 0, len(hierarchyEdges))
	hierarchyNames := make([]string, 0)
	seenHierarchyNames := map[string]bool{}
	for _, edge := range hierarchyEdges {
		if edge == nil {
			continue
		}
		hierarchySourceIDs = append(hierarchySourceIDs, edge.From)
		if graph.IsUnresolvedTarget(edge.To) {
			name := graph.UnresolvedName(edge.To)
			if name != "" && !seenHierarchyNames[name] {
				seenHierarchyNames[name] = true
				hierarchyNames = append(hierarchyNames, name)
			}
		}
	}
	hierarchySources := g.GetNodesByIDs(hierarchySourceIDs)
	hierarchyByName := g.FindNodesByNames(hierarchyNames)
	children := map[string][]string{}
	// Every hierarchy edge, carrying whatever CLOSED type arguments the
	// extractor stamped on it (target_type_args on generic base-list
	// entries) — the evidence half of the G9 gate: an IBoxStore<Crate>
	// receiver never dispatches into the IBoxStore<Widget> implementor.
	// Absent for non-generic bases, open generics and non-simple
	// arguments — absence always means "do not filter".
	//
	// UNSTAMPED edges are recorded too, as the empty string. A type can
	// implement several constructions of one erased interface, and those
	// constructions can arrive through an inherited interface or a base
	// class rather than the type's own base list. Deciding whether one
	// closure describes a type means walking every path it has to that
	// interface, and a path carrying no closure is exactly as
	// disqualifying as two different ones.
	implEdges := map[string]map[string][]string{}
	anyStamps := false
	for _, e := range hierarchyEdges {
		if e == nil || e.From == "" || e.To == "" {
			continue
		}
		if e.Meta != nil && e.Meta["via"] == MetaViaMethodSetInference {
			continue
		}
		toID := e.To
		if graph.IsUnresolvedTarget(toID) {
			toID = csharpResolveHierarchyTargetPrefetched(hierarchySources[e.From], toID, hierarchyByName)
			if toID == "" {
				continue
			}
		}
		children[toID] = append(children[toID], e.From)
		args := ""
		if e.Meta != nil {
			args, _ = e.Meta["target_type_args"].(string)
		}
		if args != "" {
			anyStamps = true
		}
		m := implEdges[e.From]
		if m == nil {
			m = map[string][]string{}
			implEdges[e.From] = m
		}
		m[toID] = append(m[toID], args)
	}
	if len(children) == 0 {
		return 0
	}

	// Project-global using aliases (`global using Entity = App.Crate;`)
	// make their identifier opaque to the string-compared stamps in EVERY
	// file of the project — the extractor's ancestor scan only sees the
	// declaring file, so a receiver spelled IBox<Entity> and an implementor
	// spelled IBox<Crate> stamp unequal spellings of one constructed
	// interface. Any stamp naming such an alias is refused (never filter).
	// Collected once per pass from the file nodes' extractor stamps, and
	// only when stamps exist to gate with; the union across repos is
	// deliberate — over-refusing can only PRESERVE edges.
	globalAliasNames := map[string]bool{}
	if anyStamps {
		for n := range graph.NodesByKindsSeq(g, graph.KindFile) {
			if n == nil || n.Meta == nil {
				continue
			}
			for _, a := range csharpMetaStrings(n.Meta["global_using_aliases"]) {
				for _, form := range csharpAliasComparableForms(a) {
					globalAliasNames[form] = true
				}
			}
		}
	}

	// implementation/interface type node id → member name → method nodes.
	// Every overload matters: C# overloads mint one node each (Convert,
	// Convert_L39, ...) sharing the same Name, and real call sites bind to any
	// of them — a single-node-per-name projection would silently drop the
	// overload the corpus actually calls through.
	// The compact projection is valid for both partial and full-census runs.
	// On a full run a nil familyScope means every repository; using the same
	// light EdgeMemberOf and qualified-method streams avoids decoding every
	// member edge and method metadata blob merely to discover C# anchors.
	memberEdges, memberNodes, anchorNodes, projected := csharpScopedMemberProjection(g, familyScope, children)
	if !projected {
		memberEdges = frameworkRepoEdges(g, familyScope, graph.EdgeMemberOf)
		memberNodeIDs := make([]string, 0, len(memberEdges))
		for _, edge := range memberEdges {
			if edge != nil {
				memberNodeIDs = append(memberNodeIDs, edge.From)
			}
		}
		memberNodes = g.GetNodesByIDs(memberNodeIDs)
		anchorNodes = memberNodes
	}
	var membersByType map[string]map[string][]*graph.Node
	switch {
	case projected:
		// Reuse the compact nodes already read for anchor discovery. This is
		// the full-census fast path as well as the normal scoped path.
		membersByType = csharpMemberMethodsAllByTypeFromEdges(memberEdges, memberNodes)
	case scope == nil:
		membersByType = csharpMemberMethodsAllByType(g)
	default:
		membersByType = csharpMemberMethodsAllByTypeFromEdges(memberEdges, memberNodes)
	}
	if len(membersByType) == 0 {
		return 0
	}

	// Anchor discovery: every C# interface member method node, via its
	// EdgeMemberOf owner, grouped by (interface, name) so the interface's own
	// overload nodes land in ONE family rather than seeding duplicates.
	type anchorGroup struct {
		ifaceID    string
		name       string
		repoPrefix string
		nodeIDs    []string
	}
	anchorGroups := map[string]*anchorGroup{}
	var anchorOrder []string
	for _, e := range memberEdges {
		if e == nil || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		m := anchorNodes[e.From]
		if m == nil || m.Kind != graph.KindMethod || m.Language != "csharp" || !csharpIsIfaceMember(m) {
			continue
		}
		key := e.To + "\x00" + m.Name
		ag := anchorGroups[key]
		if ag == nil {
			ag = &anchorGroup{ifaceID: e.To, name: m.Name, repoPrefix: m.RepoPrefix}
			anchorGroups[key] = ag
			anchorOrder = append(anchorOrder, key)
		}
		ag.nodeIDs = append(ag.nodeIDs, m.ID)
	}
	if len(anchorGroups) == 0 {
		return 0
	}

	// A variance-declaring interface (ISource<out T> / ISink<in T>) makes
	// differently-closed constructions assignable across the family, so the
	// closed-and-unequal equality gate — which models invariant parameters
	// only — must never arm for it: its families carry no stamped args at
	// all, and every site keeps the full fan-out.
	ifaceIDs := make([]string, 0, len(anchorGroups))
	seenIfaceIDs := map[string]bool{}
	for _, key := range anchorOrder {
		id := anchorGroups[key].ifaceID
		if !seenIfaceIDs[id] {
			seenIfaceIDs[id] = true
			ifaceIDs = append(ifaceIDs, id)
		}
	}
	variantIface := map[string]bool{}
	for id, n := range g.GetNodesByIDs(ifaceIDs) {
		if n == nil || n.Meta == nil {
			continue
		}
		if v, _ := n.Meta["variant_type_params"].(bool); v {
			variantIface[id] = true
		}
	}

	// Descendant closure per interface, computed once and shared across that
	// interface's anchors (one per member name).
	descCache := map[string][]string{}
	descendants := func(ifaceID string) []string {
		if d, ok := descCache[ifaceID]; ok {
			return d
		}
		var out []string
		visited := map[string]bool{ifaceID: true}
		queue := append([]string(nil), children[ifaceID]...)
		for len(queue) > 0 {
			t := queue[0]
			queue = queue[1:]
			if visited[t] {
				continue
			}
			visited[t] = true
			out = append(out, t)
			queue = append(queue, children[t]...)
		}
		descCache[ifaceID] = out
		return out
	}

	// The single closure by which each descendant reaches an interface,
	// or "" where that is not unique. Computed once per interface and
	// shared across its anchors, like descCache — an interface with many
	// members would otherwise re-walk the same hierarchy per member.
	closureCache := map[string]map[string]string{}
	closuresFor := func(ifaceID string) map[string]string {
		if c, ok := closureCache[ifaceID]; ok {
			return c
		}
		out := map[string]string{}
		for _, sub := range descendants(ifaceID) {
			out[sub] = csharpUniqueClosureToIface(sub, ifaceID, implEdges)
		}
		closureCache[ifaceID] = out
		return out
	}

	// Build families and the member → families index.
	type family struct {
		ifaceID   string
		ifaceName string // short interface name, for matching a receiver's declared field type
		members   []string
		implArgs  map[string]string // member method ID → its DIRECT implementor's stamped type args ("" absent = never filter)
	}
	var families []family
	famsOfMember := map[string][]int{}
	for _, key := range anchorOrder {
		ag := anchorGroups[key]
		memberIDs := append([]string(nil), ag.nodeIDs...)
		anchorSet := map[string]bool{}
		for _, id := range ag.nodeIDs {
			anchorSet[id] = true
		}
		memberArgs := map[string]string{}
		implCount := 0
		variant := variantIface[ag.ifaceID]
		for _, sub := range descendants(ag.ifaceID) {
			byName := membersByType[sub]
			if byName == nil {
				continue
			}
			subArgs := ""
			if !variant {
				subArgs = closuresFor(ag.ifaceID)[sub]
				if csharpArgsNameGlobalAlias(subArgs, globalAliasNames) {
					subArgs = ""
				}
			}
			for _, m := range byName[ag.name] {
				if m == nil || anchorSet[m.ID] {
					continue
				}
				// In-repo only: cross-repo dispatch is CrossRepoResolver's domain.
				if m.RepoPrefix != ag.repoPrefix {
					continue
				}
				memberIDs = append(memberIDs, m.ID)
				if subArgs != "" {
					memberArgs[m.ID] = subArgs
				}
				implCount++
			}
		}
		// A family needs an interface member plus at least one implementation
		// to cascade; one wider than the cap is dropped whole as noise.
		if implCount == 0 || len(memberIDs) > csharpIfaceDispatchCap {
			continue
		}
		idx := len(families)
		families = append(families, family{
			ifaceID: ag.ifaceID, ifaceName: csharpShortTypeName(ag.ifaceID),
			members: memberIDs, implArgs: memberArgs,
		})
		for _, id := range memberIDs {
			famsOfMember[id] = append(famsOfMember[id], idx)
		}
	}
	if len(families) == 0 {
		return 0
	}
	// Every call that can affect interface dispatch targets a known family
	// member. Read only those incoming adjacency lists, even for a full pass,
	// instead of decoding the repository's entire calls corpus.
	callSeen := make(map[graph.EdgeIdentity]struct{})
	var callEdges []*graph.Edge
	if scope != nil {
		callEdges = appendUniqueFrameworkEdges(callEdges, callSeen, scopedSourceCalls...)
	}
	familyMemberIDs := make([]string, 0, len(famsOfMember))
	for id := range famsOfMember {
		familyMemberIDs = append(familyMemberIDs, id)
	}
	for _, incoming := range g.GetInEdgesByNodeIDs(familyMemberIDs) {
		for _, edge := range incoming {
			if edge != nil && edge.Kind == graph.EdgeCalls {
				callEdges = appendUniqueFrameworkEdges(callEdges, callSeen, edge)
			}
		}
	}

	// Existing resolved call sites, keyed per line, so a fan-out edge never
	// duplicates a real call at the same site (a caller that already reaches
	// the member directly, or a prior run of this pass).
	existing := map[string]bool{}
	for _, e := range callEdges {
		if e == nil || e.IsSpeculative() || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		existing[csharpCallSiteKey(e.From, e.To, e.FilePath, e.Line)] = true
	}

	var batch []*graph.Edge
	seen := map[string]bool{}
	receiverLookups := newCSharpReceiverLookupCtx()
	for _, e := range callEdges {
		if e == nil || e.IsSpeculative() || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		// Never re-fan from this pass's own output — real call sites only.
		if e.Meta != nil && e.Meta[MetaSynthesizedBy] == SynthCSharpIfaceDispatch {
			continue
		}
		fams := famsOfMember[e.To]
		if len(fams) == 0 {
			continue
		}
		// Tier-gate the SOURCE: a typed or scope-resolved binding (and an
		// untagged legacy edge, which carries unknown — not low — confidence,
		// mirroring SuppressRedundantTextMatches) fans from any caller. A
		// text_matched binding is a name-only guess that can land on a family
		// member from a completely unrelated same-named method (an
		// IOrdinalizer.Convert self-call text-matched into the
		// INumberToWordsConverter family); those fan ONLY when the caller is
		// itself a member of the same family — the intra-family self/sibling-
		// call shape the weak tier legitimately carries (overload self-calls
		// bind text_matched).
		weakSource := e.Origin == graph.OriginTextMatched
		var fromFams []int
		if weakSource {
			fromFams = famsOfMember[e.From]
			if len(fromFams) == 0 {
				continue
			}
		}
		for _, fi := range fams {
			if weakSource && !containsInt(fromFams, fi) {
				continue
			}
			f := families[fi]
			// Constructed-interface gate (G9): the source site's own type
			// arguments — a sibling site bound to a stamped implementor
			// carries that implementor's args; a through-interface site
			// carries its receiver FIELD's declared args when the receiver
			// evidence names one. "" means unknown — never filter. A
			// family with no stamps at all (every non-generic interface,
			// and the whole graph until a reindex) can never filter, so
			// it never pays the receiver lookup either.
			srcArgs := f.implArgs[e.To]
			if srcArgs == "" && len(f.implArgs) > 0 {
				srcArgs = csharpReceiverDeclaredArgs(g, e, f.ifaceID, f.ifaceName, receiverLookups)
				if csharpArgsNameGlobalAlias(srcArgs, globalAliasNames) {
					srcArgs = ""
				}
			}
			for _, member := range f.members {
				// Skip the member the call already reaches — and the CALLER
				// itself: a family member forwarding through its own
				// interface (decorator/facade shape) must not gain a
				// synthesized from==to edge that find_usages consumers read
				// as "the symbol is its own caller". Real recursion is the
				// binder's edge to mint, never this synthesizer's.
				if member == e.To || member == e.From {
					continue
				}
				// A site with known constructed args never fans into an
				// implementor stamped with DIFFERENT args — they implement
				// different constructed interfaces. Members without a stamp
				// (the interface member itself, open generics, transitive
				// implementors) always stay in.
				if srcArgs != "" {
					if ma := f.implArgs[member]; ma != "" && ma != srcArgs {
						continue
					}
				}
				k := csharpCallSiteKey(e.From, member, e.FilePath, e.Line)
				if existing[k] || seen[k] {
					continue
				}
				seen[k] = true
				batch = append(batch, csharpIfaceDispatchEdge(e, member, f.ifaceID, len(f.members)-1))
			}
		}
	}
	if len(batch) > 0 {
		g.AddBatch(nil, batch)
	}
	return len(batch)
}

// csharpScopedMemberProjection replaces full EdgeMemberOf and full-node
// materialisation with metadata-free identities. A nil scope admits every
// repository for a full-census run. Only C# methods on owners with descendants
// can seed a dispatch family, so those are exact-refetched after both cursors
// close; every other family member stays a compact Node value.
// The final sort mirrors graph.ReadRepoEdgesByKinds so anchor/family and
// provenance order are unchanged.
func csharpScopedMemberProjection(
	g graph.Store,
	scope map[string]bool,
	children map[string][]string,
) (memberEdges []*graph.Edge, memberNodes, anchorNodes map[string]*graph.Node, ok bool) {
	sequencer, ok := g.(graph.QualifiedNodeIdentitySequencer)
	if !ok {
		return nil, nil, nil, false
	}

	wanted := map[string]struct{}{}
	for edge := range graph.EdgesLightSeq(g, graph.EdgeMemberOf) {
		if edge == nil || edge.From == "" {
			continue
		}
		memberEdges = append(memberEdges, edge)
		wanted[edge.From] = struct{}{}
	}
	memberNodes = make(map[string]*graph.Node)
	for node := range sequencer.QualifiedNodeIdentitiesSeq(graph.KindMethod) {
		_, needed := wanted[node.ID]
		if !needed || (scope != nil && !scope[node.RepoPrefix]) {
			continue
		}
		memberNodes[node.ID] = &graph.Node{
			ID:         node.ID,
			Kind:       graph.KindMethod,
			Name:       node.Name,
			Language:   node.Language,
			FilePath:   node.FilePath,
			RepoPrefix: node.RepoPrefix,
		}
	}

	filtered := memberEdges[:0]
	for _, edge := range memberEdges {
		if memberNodes[edge.From] != nil {
			filtered = append(filtered, edge)
		}
	}
	memberEdges = filtered
	var csharpMethodIDs []string
	for _, edge := range memberEdges {
		node := memberNodes[edge.From]
		if node.Language == "csharp" && len(children[edge.To]) > 0 {
			csharpMethodIDs = append(csharpMethodIDs, edge.From)
		}
	}
	sort.Slice(memberEdges, func(i, j int) bool {
		left, right := memberEdges[i], memberEdges[j]
		leftRepo := memberNodes[left.From].RepoPrefix
		rightRepo := memberNodes[right.From].RepoPrefix
		if leftRepo != rightRepo {
			return leftRepo < rightRepo
		}
		if left.From != right.From {
			return left.From < right.From
		}
		if left.To != right.To {
			return left.To < right.To
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.FilePath != right.FilePath {
			return left.FilePath < right.FilePath
		}
		return left.Line < right.Line
	})

	// Both streaming projections are exhausted before the exact metadata read.
	anchorNodes = g.GetNodesByIDs(dedupeFrameworkIDs(csharpMethodIDs))
	return memberEdges, memberNodes, anchorNodes, true
}

// csharpIsIfaceMember reports whether n is a bodyless (or default) interface
// member declaration emitted by the C# extractor.
func csharpIsIfaceMember(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["iface_member"].(bool)
	return v
}

// csharpIfaceDispatchEdge builds one fan-out call edge from the call site e to
// another family member, at the non-speculative ast_inferred tier so it
// survives the default speculative filter on find_usages / get_callers. The
// fan-out width rides in candidate_count for auditing; only one implementation
// runs at a site, but Roslyn reports the reference on every family member and
// this pass mirrors that.
func csharpIfaceDispatchEdge(e *graph.Edge, to, ifaceTypeID string, fanout int) *graph.Edge {
	ne := &graph.Edge{
		From: e.From, To: to, Kind: graph.EdgeCalls,
		FilePath: e.FilePath, Line: e.Line,
		Origin:          graph.OriginASTInferred,
		Confidence:      ConfidenceTyped,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped),
		Meta: map[string]any{
			"via":             "csharp-iface-dispatch",
			"iface_type":      ifaceTypeID,
			"candidate_count": fanout,
		},
	}
	StampSynthesized(ne, SynthCSharpIfaceDispatch)
	return ne
}

// csharpMemberMethodsAllByType is the overload-preserving variant of
// memberMethodNodesByType: type node id → member name → EVERY method node with
// that name (C# overloads mint one node per declaration, so a name maps to
// several nodes). Uses the backend's MemberMethodsByType projection when
// available, else walks EdgeMemberOf.
func csharpMemberMethodsAllByType(g graph.Store) map[string]map[string][]*graph.Node {
	if cap, ok := g.(graph.MemberMethodsByType); ok {
		raw := cap.MemberMethodsByType()
		if len(raw) == 0 {
			return nil
		}
		out := make(map[string]map[string][]*graph.Node, len(raw))
		for typeID, methods := range raw {
			set := make(map[string][]*graph.Node, len(methods))
			for _, m := range methods {
				set[m.Name] = append(set[m.Name], &graph.Node{
					ID:         m.MethodID,
					Kind:       graph.KindMethod,
					Name:       m.Name,
					FilePath:   m.FilePath,
					StartLine:  m.StartLine,
					RepoPrefix: m.RepoPrefix,
				})
			}
			out[typeID] = set
		}
		return out
	}
	var edges []*graph.Edge
	methodIDs := make([]string, 0)
	for e := range graph.EdgesLightSeq(g, graph.EdgeMemberOf) {
		if e == nil {
			continue
		}
		edges = append(edges, e)
		methodIDs = append(methodIDs, e.From)
	}
	return csharpMemberMethodsAllByTypeFromEdges(edges, g.GetNodesByIDs(methodIDs))
}

func csharpMemberMethodsAllByTypeFromEdges(edges []*graph.Edge, nodes map[string]*graph.Node) map[string]map[string][]*graph.Node {
	out := map[string]map[string][]*graph.Node{}
	for _, e := range edges {
		if e == nil {
			continue
		}
		method := nodes[e.From]
		if method == nil || method.Kind != graph.KindMethod {
			continue
		}
		set := out[e.To]
		if set == nil {
			set = make(map[string][]*graph.Node)
			out[e.To] = set
		}
		set[method.Name] = append(set[method.Name], method)
	}
	return out
}

// csharpAliasComparableForms returns every form of a global-alias
// identifier a type-argument stamp could carry: the canonical name
// (verbatim prefix stripped — pre-normalization stores stamped it raw)
// and, for an alias legally shadowing a BCL type name, the keyword the
// extractor's canonicalization folds arguments onto. `global using
// Int32 = App.Crate` makes a stamped "int" ambiguous — it may spell the
// genuine keyword or the folded alias — and an ambiguous stamp must
// refuse (never filter). The fold mirrors the parser's
// csharpCanonicalTypeArg table; the packages stay independent, so keep
// the two in sync.
func csharpAliasComparableForms(alias string) []string {
	name := strings.TrimPrefix(alias, "@")
	if name == "" {
		return nil
	}
	forms := []string{name}
	folded := ""
	switch name {
	case "String":
		folded = "string"
	case "Boolean":
		folded = "bool"
	case "Byte":
		folded = "byte"
	case "SByte":
		folded = "sbyte"
	case "Char":
		folded = "char"
	case "Decimal":
		folded = "decimal"
	case "Double":
		folded = "double"
	case "Single":
		folded = "float"
	case "Int16":
		folded = "short"
	case "UInt16":
		folded = "ushort"
	case "Int32":
		folded = "int"
	case "UInt32":
		folded = "uint"
	case "Int64":
		folded = "long"
	case "UInt64":
		folded = "ulong"
	case "Object":
		folded = "object"
	case "IntPtr":
		folded = "nint"
	case "UIntPtr":
		folded = "nuint"
	}
	if folded != "" {
		forms = append(forms, folded)
	}
	return forms
}

// csharpArgsNameGlobalAlias reports whether any comma-separated argument in
// a type-argument stamp names a project-global using alias — a spelling the
// string comparison cannot resolve, so the stamp must be refused.
// csharpUniqueClosureToIface returns the closed type arguments by which
// sub reaches ifaceID, or "" when that is not a single known closure.
//
// A stamp describes one construction, but the gate applies it to every
// same-named member of the implementor, so it is only sound when the
// implementor reaches the interface exactly one way. C# permits several:
// `class C : IEnumerable<int>, IEnumerable<string>` is legal whenever
// the arguments cannot unify (CS0695 fires only when they could), and a
// construction can also arrive through an inherited interface or a base
// class instead of the type's own base list.
//
// The rule is deliberately asymmetric: only the implementor's OWN direct
// closure can qualify it for filtering, while evidence found anywhere up
// the hierarchy can disqualify it. A transitive descendant has never
// been filterable — the closure belongs to an intermediate type, not to
// this one — and this walk does not change that. What it adds is the
// ability to NOTICE a second construction arriving through an inherited
// interface or a base class, and to refuse on it.
//
// So the walk can only ever remove filtering power, never add it. That
// keeps the existing conservative rules intact and makes every outcome
// change here a preserved edge rather than a dropped one.
func csharpUniqueClosureToIface(sub, ifaceID string, implEdges map[string]map[string][]string) string {
	// The implementor's own base list. Absent, ambiguous, or unstamped
	// means there is nothing to filter on, exactly as before.
	direct := ""
	for _, c := range implEdges[sub][ifaceID] {
		if c == "" || (direct != "" && direct != c) {
			return ""
		}
		direct = c
	}
	if direct == "" {
		return ""
	}

	// Any OTHER path to the same interface that disagrees — including one
	// carrying no closure at all, which means a construction we cannot
	// read — proves no single closure describes this type.
	visited := map[string]bool{sub: true}
	queue := []string{sub}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for to, closures := range implEdges[cur] {
			if to == ifaceID && cur != sub {
				for _, c := range closures {
					if c != direct {
						return ""
					}
				}
			}
			if !visited[to] {
				visited[to] = true
				queue = append(queue, to)
			}
		}
	}
	return direct
}

func csharpArgsNameGlobalAlias(args string, aliases map[string]bool) bool {
	if args == "" || len(aliases) == 0 {
		return false
	}
	for _, a := range strings.Split(args, ",") {
		if aliases[a] {
			return true
		}
	}
	return false
}

// containsInt reports whether xs contains v. Family lists are tiny (a method
// belongs to one or two families), so a linear scan beats a map.
func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// csharpShortTypeName reduces a type node ID to its bare type name:
// `file.cs::Ns.IBoxStore` → IBoxStore.
func csharpShortTypeName(id string) string {
	if i := strings.LastIndex(id, "::"); i >= 0 {
		id = id[i+2:]
	}
	if i := strings.LastIndex(id, "."); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// csharpReceiverDeclaredArgs recovers the CLOSED type arguments a
// through-interface call site's receiver declares, for the G9 gate:
// `_store.Fetch(...)` on a field declared `IBoxStore<Crate> _store`
// answers "Crate". Evidence-gated at every step — any absence answers ""
// (never filter):
//
//   - the receiver name comes from the bound edge's own receiver_name
//     meta, or from the extraction's unresolved companion edge for the
//     SAME member name at the same site (the enrichment/LSP tiers bind a
//     NEW edge and leave the companion, receiver evidence and all,
//     alongside); a site the extractor marked receiver_ambiguous — two
//     same-named calls on one line — contributes nothing;
//   - the field is looked up on the caller's own type (bare receivers
//     only — the exact shape the field-identifier emitter covers) and
//     must actually be a field/constant node;
//   - the arguments come from the extractor's field_type_args stamp,
//     which already applied the open/closed rules (enclosing-chain type
//     parameters, non-simple arguments) at the one place the full
//     syntax context is visible — this pass never re-parses type text;
//   - the field's declared type must still name THIS family's interface
//     (short-name comparison against field_type — the one remaining
//     name-based trust, see the caller).
//
// Typed LOCALS are a named remainder: the tenv strips generics before
// receiver_type is stamped, so local-receiver sites keep the full
// fan-out until the extractor carries local type arguments too.
// csharpReceiverLookupCtx carries the per-pass receiver-evidence caches:
// declared args per (caller, member, site, interface), each caller's
// out-edge adjacency read ONCE and served to every site (the companion
// scan and the field-read evidence scan both consume it), and resolved
// field nodes by ID.
type csharpReceiverLookupCtx struct {
	args      map[string]string
	outEdges  map[string][]*graph.Edge
	fields    map[string]*graph.Node
	fieldSeen map[string]bool
}

func newCSharpReceiverLookupCtx() *csharpReceiverLookupCtx {
	return &csharpReceiverLookupCtx{
		args:      map[string]string{},
		outEdges:  map[string][]*graph.Edge{},
		fields:    map[string]*graph.Node{},
		fieldSeen: map[string]bool{},
	}
}

func (c *csharpReceiverLookupCtx) callerOutEdges(g graph.Store, caller string) []*graph.Edge {
	if es, ok := c.outEdges[caller]; ok {
		return es
	}
	es := g.GetOutEdges(caller)
	c.outEdges[caller] = es
	return es
}

func (c *csharpReceiverLookupCtx) fieldNode(g graph.Store, id string) *graph.Node {
	if c.fieldSeen[id] {
		return c.fields[id]
	}
	c.fieldSeen[id] = true
	n := g.GetNodesByIDs([]string{id})[id]
	c.fields[id] = n
	return n
}

func csharpReceiverDeclaredArgs(g graph.Store, e *graph.Edge, ifaceID, ifaceName string, lookups *csharpReceiverLookupCtx) string {
	if e == nil || e.From == "" {
		return ""
	}
	// The member (e.To) selects WHICH companion edge lends its receiver, so
	// two different member calls sharing a line resolve different fields —
	// a key without it lets the first call's arguments poison the second.
	// The full interface ID keeps short-name twins from distinct families
	// apart for the same reason.
	cacheKey := e.From + "\x00" + e.To + "\x00" + e.FilePath + "\x00" + strconv.Itoa(e.Line) + "\x00" + ifaceID
	if v, ok := lookups.args[cacheKey]; ok {
		return v
	}
	args := ""
	if field := csharpReceiverField(g, e, lookups); field != nil {
		ft, _ := field.Meta["field_type"].(string)
		prefix := strings.TrimSpace(ft)
		if lt := strings.Index(prefix, "<"); lt > 0 {
			prefix = prefix[:lt]
		}
		if i := strings.LastIndex(prefix, "."); i >= 0 {
			prefix = prefix[i+1:]
		}
		if prefix == ifaceName {
			args, _ = field.Meta["field_type_args"].(string)
		}
	}
	lookups.args[cacheKey] = args
	return args
}

// csharpReceiverField resolves the call site's receiver to a field (or
// constant) node of the caller's own type, or nil when the receiver is
// not an unambiguous bare same-type field.
func csharpReceiverField(g graph.Store, e *graph.Edge, lookups *csharpReceiverLookupCtx) *graph.Node {
	name := ""
	if e.Meta != nil {
		if amb, _ := e.Meta["receiver_ambiguous"].(bool); amb {
			return nil
		}
		name, _ = e.Meta["receiver_name"].(string)
	}
	if name == "" {
		// The bound edge (enrichment/LSP tiers) carries no receiver
		// evidence; the extraction's unresolved companion for the same
		// member name at the same site does. Match the member name so a
		// different call sharing the line can never lend its receiver.
		memberName := csharpShortTypeName(e.To)
		companionTo := "unresolved::*." + memberName
		for _, out := range lookups.callerOutEdges(g, e.From) {
			if out == nil || out.Kind != graph.EdgeCalls || out.To != companionTo {
				continue
			}
			if out.FilePath != e.FilePath || out.Line != e.Line || out.Meta == nil {
				continue
			}
			if amb, _ := out.Meta["receiver_ambiguous"].(bool); amb {
				return nil
			}
			if rn, _ := out.Meta["receiver_name"].(string); rn != "" {
				name = rn
				break
			}
		}
	}
	if name == "" || strings.ContainsAny(name, ".(") {
		return nil
	}
	ownerID := csharpEnclosingTypeID(e.From)
	if ownerID == "" {
		return nil
	}
	fieldID := ownerID + "." + name
	// Binding evidence: the field-identifier emitter refuses shadowed
	// identifiers (a parameter or local with the field's name owns the
	// identifier inside that method), so an EdgeReads at this exact site
	// naming the field is proof the bare receiver really is the enclosing
	// type's field. A name-only lookup would bind a shadowed identifier to
	// the field it shadows and gate on the wrong declared arguments —
	// without the read edge the receiver stays unknown (never filter).
	fieldRead := false
	for _, out := range lookups.callerOutEdges(g, e.From) {
		if out == nil || out.Kind != graph.EdgeReads {
			continue
		}
		if out.FilePath != e.FilePath || out.Line != e.Line {
			continue
		}
		if out.To == "unresolved::*."+name || out.To == fieldID {
			fieldRead = true
			break
		}
	}
	if !fieldRead {
		return nil
	}
	field := lookups.fieldNode(g, fieldID)
	if field == nil || field.Meta == nil ||
		(field.Kind != graph.KindField && field.Kind != graph.KindConstant) {
		return nil
	}
	return field
}

// csharpEnclosingTypeID strips the member segment off a method node ID:
// `file.cs::Flow.Pull` → `file.cs::Flow`. Empty when the ID carries no
// member segment after the symbol separator.
func csharpEnclosingTypeID(methodID string) string {
	sep := strings.LastIndex(methodID, "::")
	if sep < 0 {
		return ""
	}
	dot := strings.LastIndex(methodID, ".")
	if dot <= sep+2 {
		return ""
	}
	return methodID[:dot]
}

func csharpResolveHierarchyTargetPrefetched(from *graph.Node, unresolvedTo string, byName map[string][]*graph.Node) string {
	name := graph.UnresolvedName(unresolvedTo)
	if name == "" {
		return ""
	}
	if from == nil || from.Language != "csharp" {
		return ""
	}
	var cand *graph.Node
	for _, n := range byName[name] {
		if n == nil || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
			continue
		}
		if n.Language != "csharp" || n.RepoPrefix != from.RepoPrefix {
			continue
		}
		if cand != nil {
			return "" // ambiguous — do not guess
		}
		cand = n
	}
	if cand == nil {
		return ""
	}
	return cand.ID
}
