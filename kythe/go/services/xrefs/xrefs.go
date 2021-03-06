/*
 * Copyright 2015 Google Inc. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package xrefs defines the xrefs Service interface and some useful utility
// functions.
package xrefs

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"
	"unicode"

	"kythe.io/kythe/go/services/web"
	"kythe.io/kythe/go/util/kytheuri"
	"kythe.io/kythe/go/util/schema/edges"
	"kythe.io/kythe/go/util/schema/facts"
	"kythe.io/kythe/go/util/schema/nodes"

	"bitbucket.org/creachadair/stringset"
	"github.com/sergi/go-diff/diffmatchpatch"
	"golang.org/x/net/context"

	cpb "kythe.io/kythe/proto/common_proto"
	xpb "kythe.io/kythe/proto/xref_proto"
)

// Service defines the interface for file based cross-references.  Informally,
// the cross-references of an entity comprise the definitions of that entity,
// together with all the places where those definitions are referenced through
// constructs such as type declarations, variable references, function calls,
// and so on.
type Service interface {
	GraphService

	// TODO(fromberger): Separate direct graph access from xrefs.

	// Decorations returns an index of the nodes associated with a specified file.
	Decorations(context.Context, *xpb.DecorationsRequest) (*xpb.DecorationsReply, error)

	// CrossReferences returns the global cross-references for the given nodes.
	CrossReferences(context.Context, *xpb.CrossReferencesRequest) (*xpb.CrossReferencesReply, error)

	// Documentation takes a set of tickets and returns documentation for them.
	Documentation(context.Context, *xpb.DocumentationRequest) (*xpb.DocumentationReply, error)
}

// GraphService exposes direct access to nodes and edges in a Kythe graph.
type GraphService interface {
	Nodes(context.Context, *xpb.NodesRequest) (*xpb.NodesReply, error)
	Edges(context.Context, *xpb.EdgesRequest) (*xpb.EdgesReply, error)
}

// ErrDecorationsNotFound is returned by an implementation of the Decorations
// method when decorations for the given file cannot be found.
var ErrDecorationsNotFound = errors.New("file decorations not found")

// FixTickets converts the specified tickets, which are expected to be Kythe
// URIs, into canonical form. It is an error if len(tickets) == 0.
func FixTickets(tickets []string) ([]string, error) {
	if len(tickets) == 0 {
		return nil, errors.New("no tickets specified")
	}

	canonical := make([]string, len(tickets))
	for i, ticket := range tickets {
		fixed, err := kytheuri.Fix(ticket)
		if err != nil {
			return nil, fmt.Errorf("invalid ticket %q: %v", ticket, err)
		}
		canonical[i] = fixed
	}
	return canonical, nil
}

// InSpanBounds reports whether [start,end) is bounded by the specified
// [startBoundary,endBoundary) span.
func InSpanBounds(kind xpb.DecorationsRequest_SpanKind, start, end, startBoundary, endBoundary int32) bool {
	switch kind {
	case xpb.DecorationsRequest_WITHIN_SPAN:
		return start >= startBoundary && end <= endBoundary
	case xpb.DecorationsRequest_AROUND_SPAN:
		return start <= startBoundary && end >= endBoundary
	default:
		log.Printf("WARNING: unknown DecorationsRequest_SpanKind: %v", kind)
	}
	return false
}

// IsDefKind reports whether the given edgeKind matches the requested
// definition kind.
func IsDefKind(requestedKind xpb.CrossReferencesRequest_DefinitionKind, edgeKind string, incomplete bool) bool {
	// TODO(schroederc): handle full vs. binding CompletesEdge
	edgeKind = edges.Canonical(edgeKind)
	if IsDeclKind(xpb.CrossReferencesRequest_ALL_DECLARATIONS, edgeKind, incomplete) {
		return false
	}
	switch requestedKind {
	case xpb.CrossReferencesRequest_NO_DEFINITIONS:
		return false
	case xpb.CrossReferencesRequest_FULL_DEFINITIONS:
		return edgeKind == edges.Defines || edges.IsVariant(edgeKind, edges.Completes)
	case xpb.CrossReferencesRequest_BINDING_DEFINITIONS:
		return edgeKind == edges.DefinesBinding || edges.IsVariant(edgeKind, edges.Completes)
	case xpb.CrossReferencesRequest_ALL_DEFINITIONS:
		return edges.IsVariant(edgeKind, edges.Defines) || edges.IsVariant(edgeKind, edges.Completes)
	default:
		panic("unhandled CrossReferencesRequest_DefinitionKind")
	}
}

// IsDeclKind reportsa whether the given edgeKind matches the requested
// declaration kind
func IsDeclKind(requestedKind xpb.CrossReferencesRequest_DeclarationKind, edgeKind string, incomplete bool) bool {
	if !incomplete {
		return false
	}
	edgeKind = edges.Canonical(edgeKind)
	switch requestedKind {
	case xpb.CrossReferencesRequest_NO_DECLARATIONS:
		return false
	case xpb.CrossReferencesRequest_ALL_DECLARATIONS:
		return edges.IsVariant(edgeKind, edges.Defines)
	default:
		panic("unhandled CrossReferenceRequest_DeclarationKind")
	}
}

// IsRefKind determines whether the given edgeKind matches the requested
// reference kind.
func IsRefKind(requestedKind xpb.CrossReferencesRequest_ReferenceKind, edgeKind string) bool {
	edgeKind = edges.Canonical(edgeKind)
	switch requestedKind {
	case xpb.CrossReferencesRequest_NO_REFERENCES:
		return false
	case xpb.CrossReferencesRequest_CALL_REFERENCES:
		return edgeKind == edges.RefCall
	case xpb.CrossReferencesRequest_NON_CALL_REFERENCES:
		return edgeKind != edges.RefCall && edges.IsVariant(edgeKind, edges.Ref)
	case xpb.CrossReferencesRequest_ALL_REFERENCES:
		return edges.IsVariant(edgeKind, edges.Ref)
	default:
		panic("unhandled CrossReferencesRequest_ReferenceKind")
	}
}

// IsDocKind determines whether the given edgeKind matches the requested
// documentation kind.
func IsDocKind(requestedKind xpb.CrossReferencesRequest_DocumentationKind, edgeKind string) bool {
	edgeKind = edges.Canonical(edgeKind)
	switch requestedKind {
	case xpb.CrossReferencesRequest_NO_DOCUMENTATION:
		return false
	case xpb.CrossReferencesRequest_ALL_DOCUMENTATION:
		return edges.IsVariant(edgeKind, edges.Documents)
	default:
		panic("unhandled CrossDocumentationRequest_DocumentationKind")
	}
}

// AllEdges returns all edges for a particular EdgesRequest.  This means that
// the returned reply will not have a next page token.  WARNING: the paging API
// exists for a reason; using this can lead to very large memory consumption
// depending on the request.
func AllEdges(ctx context.Context, es GraphService, req *xpb.EdgesRequest) (*xpb.EdgesReply, error) {
	req.PageSize = math.MaxInt32
	reply, err := es.Edges(ctx, req)
	if err != nil || reply.NextPageToken == "" {
		return reply, err
	}

	nodes, edges := NodesMap(reply.Nodes), EdgesMap(reply.EdgeSets)

	for reply.NextPageToken != "" && err == nil {
		req.PageToken = reply.NextPageToken
		reply, err = es.Edges(ctx, req)
		if err != nil {
			return nil, err
		}
		nodesMapInto(reply.Nodes, nodes)
		edgesMapInto(reply.EdgeSets, edges)
	}

	reply = &xpb.EdgesReply{
		Nodes:    make(map[string]*xpb.NodeInfo, len(nodes)),
		EdgeSets: make(map[string]*xpb.EdgeSet, len(edges)),
	}

	for ticket, facts := range nodes {
		info := &xpb.NodeInfo{
			Facts: make(map[string][]byte, len(facts)),
		}
		for name, val := range facts {
			info.Facts[name] = val
		}
		reply.Nodes[ticket] = info
	}

	for source, groups := range edges {
		set := &xpb.EdgeSet{
			Groups: make(map[string]*xpb.EdgeSet_Group, len(groups)),
		}
		for kind, targets := range groups {
			edges := make([]*xpb.EdgeSet_Group_Edge, 0, len(targets))
			for target, ordinals := range targets {
				for ordinal := range ordinals {
					edges = append(edges, &xpb.EdgeSet_Group_Edge{
						TargetTicket: target,
						Ordinal:      ordinal,
					})
				}
			}
			sort.Sort(ByOrdinal(edges))
			set.Groups[kind] = &xpb.EdgeSet_Group{
				Edge: edges,
			}
		}
		reply.EdgeSets[source] = set
	}

	return reply, err
}

// SlowOverrides retrieves the list of Overrides for every input ticket. The map that it
// returns will only contain entries for input tickets that have at least one Override.
func SlowOverrides(ctx context.Context, xs Service, tickets []string) (map[string][]*xpb.DecorationsReply_Override, error) {
	start := time.Now()
	log.Println("WARNING: performing slow-lookup of overrides")
	overrides := make(map[string][]*xpb.DecorationsReply_Override)
	edgeKinds := []string{
		edges.Overrides,
		edges.Extends,
		edges.ExtendsPrivate,
		edges.ExtendsPrivateVirtual,
		edges.ExtendsProtected,
		edges.ExtendsProtectedVirtual,
		edges.ExtendsPublic,
		edges.ExtendsPublicVirtual,
		edges.ExtendsVirtual,
	}
	err := forAllEdges(ctx, xs, stringset.New(tickets...), edgeKinds, func(source, target, _, edgeKind string) error {
		sig, err := SlowSignature(ctx, xs, target)
		if err != nil {
			log.Printf("SlowOverrides: error getting signature for %s: %v", target, err)
			sig = &xpb.Printable{}
		}
		okind := xpb.DecorationsReply_Override_EXTENDS
		if edgeKind == edges.Overrides {
			okind = xpb.DecorationsReply_Override_OVERRIDES
		}
		overrides[source] = append(overrides[source], &xpb.DecorationsReply_Override{Ticket: target, DisplayName: sig, Kind: okind})
		return nil
	})
	// It's not necessarily the case that we should find *any* overrides, so counting them is not useful here.
	log.Printf("SlowOverrides: finished in %s", time.Since(start))
	if err != nil {
		return nil, err
	}
	return overrides, nil
}

// SlowDefinitions attempts to return a definition location for every node ticket given.  A
// definition will be returned only if it is unambiguous, but the definition may be indirect
// (through an intermediary node).
func SlowDefinitions(ctx context.Context, xs Service, tickets []string) (map[string]*xpb.Anchor, error) {
	start := time.Now()
	log.Println("WARNING: performing slow-lookup of definitions")
	defs := make(map[string]*xpb.Anchor)

	nodeTargets := make(map[string]string, len(tickets))
	for _, ticket := range tickets {
		nodeTargets[ticket] = ticket
	}

	const maxJumps = 2
	for i := 0; i < maxJumps && len(nodeTargets) > 0; i++ {
		tickets := make([]string, 0, len(nodeTargets))
		for ticket := range nodeTargets {
			tickets = append(tickets, ticket)
		}

		xReply, err := xs.CrossReferences(ctx, &xpb.CrossReferencesRequest{
			Ticket:         tickets,
			DefinitionKind: xpb.CrossReferencesRequest_BINDING_DEFINITIONS,

			// Get node kinds of related nodes for indirect definitions
			Filter: []string{facts.NodeKind},
		})
		if err != nil {
			return nil, fmt.Errorf("error retrieving definition locations: %v", err)
		}

		nextJump := make(map[string]string)

		// Give client a definition location for each reference that has only 1
		// definition location.
		//
		// If a node does not have a single definition, but does have a relevant
		// relation to another node, try to find a single definition for the
		// related node instead.
		for ticket, cr := range xReply.CrossReferences {
			if len(cr.Definition) == 1 {
				loc := cr.Definition[0]
				// TODO(schroederc): handle differing kinds; completes vs. binding
				loc.Anchor.Kind = ""
				defs[nodeTargets[ticket]] = loc.Anchor
			}
		}

		nodeTargets = nextJump
	}

	var found int
	for _, ticket := range tickets {
		if _, ok := defs[ticket]; ok {
			found++
		}
	}
	log.Printf("SlowDefinitions: found %d/%d (%.1f%%) requested definitions in %s", found, len(tickets), float64(100*found)/float64(len(tickets)), time.Since(start))

	return defs, nil
}

// NodesMap returns a map from each node ticket to a map of its facts.
func NodesMap(nodes map[string]*xpb.NodeInfo) map[string]map[string][]byte {
	m := make(map[string]map[string][]byte, len(nodes))
	nodesMapInto(nodes, m)
	return m
}

func nodesMapInto(nodes map[string]*xpb.NodeInfo, m map[string]map[string][]byte) {
	for ticket, n := range nodes {
		facts, ok := m[ticket]
		if !ok {
			facts = make(map[string][]byte, len(n.Facts))
			m[ticket] = facts
		}
		for name, value := range n.Facts {
			facts[name] = value
		}
	}
}

// EdgesMap returns a map from each node ticket to a map of its outward edge kinds.
func EdgesMap(edges map[string]*xpb.EdgeSet) map[string]map[string]map[string]map[int32]struct{} {
	m := make(map[string]map[string]map[string]map[int32]struct{}, len(edges))
	edgesMapInto(edges, m)
	return m
}

func edgesMapInto(edges map[string]*xpb.EdgeSet, m map[string]map[string]map[string]map[int32]struct{}) {
	for source, es := range edges {
		kinds, ok := m[source]
		if !ok {
			kinds = make(map[string]map[string]map[int32]struct{}, len(es.Groups))
			m[source] = kinds
		}
		for kind, g := range es.Groups {
			for _, e := range g.Edge {
				targets, ok := kinds[kind]
				if !ok {
					targets = make(map[string]map[int32]struct{})
					kinds[kind] = targets
				}
				ordinals, ok := targets[e.TargetTicket]
				if !ok {
					ordinals = make(map[int32]struct{})
					targets[e.TargetTicket] = ordinals
				}
				ordinals[e.Ordinal] = struct{}{}
			}
		}
	}
}

// Patcher uses a computed diff between two texts to map spans from the original
// text to the new text.
type Patcher struct {
	dmp  *diffmatchpatch.DiffMatchPatch
	diff []diffmatchpatch.Diff
}

// NewPatcher returns a Patcher based on the diff between oldText and newText.
func NewPatcher(oldText, newText []byte) *Patcher {
	dmp := diffmatchpatch.New()
	return &Patcher{dmp, dmp.DiffCleanupEfficiency(dmp.DiffMain(string(oldText), string(newText), true))}
}

// Patch returns the resulting span of mapping the given span from the Patcher's
// constructed oldText to its newText.  If the span no longer exists in newText
// or is invalid, the returned bool will be false.  As a convenience, if p==nil,
// the original span will be returned.
func (p *Patcher) Patch(spanStart, spanEnd int32) (newStart, newEnd int32, exists bool) {
	if spanStart > spanEnd {
		return 0, 0, false
	} else if p == nil {
		return spanStart, spanEnd, true
	}

	var old, new int32
	for _, d := range p.diff {
		l := int32(len(d.Text))
		if old > spanStart {
			return 0, 0, false
		}
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			if old <= spanStart && spanEnd <= old+l {
				newStart = new + (spanStart - old)
				newEnd = new + (spanEnd - old)
				exists = true
				return
			}
			old += l
			new += l
		case diffmatchpatch.DiffDelete:
			old += l
		case diffmatchpatch.DiffInsert:
			new += l
		}
	}

	return 0, 0, false
}

// Normalizer fixes xref.Locations within a given source text so that each point
// has consistent byte_offset, line_number, and column_offset fields within the
// range of text's length and its line lengths.
type Normalizer struct {
	textLen   int32
	lineLen   []int32
	prefixLen []int32
}

// NewNormalizer returns a Normalizer for Locations within text.
func NewNormalizer(text []byte) *Normalizer {
	lines := bytes.Split(text, lineEnd)
	lineLen := make([]int32, len(lines))
	prefixLen := make([]int32, len(lines))
	for i := 1; i < len(lines); i++ {
		lineLen[i-1] = int32(len(lines[i-1]) + len(lineEnd))
		prefixLen[i] = prefixLen[i-1] + lineLen[i-1]
	}
	lineLen[len(lines)-1] = int32(len(lines[len(lines)-1]) + len(lineEnd))
	return &Normalizer{int32(len(text)), lineLen, prefixLen}
}

// Location returns a normalized location within the Normalizer's text.
// Normalized FILE locations have no start/end points.  Normalized SPAN
// locations have fully populated start/end points clamped in the range [0,
// len(text)).
func (n *Normalizer) Location(loc *xpb.Location) (*xpb.Location, error) {
	nl := &xpb.Location{}
	if loc == nil {
		return nl, nil
	}
	nl.Ticket = loc.Ticket
	nl.Kind = loc.Kind
	if loc.Kind == xpb.Location_FILE {
		return nl, nil
	}

	if loc.Start == nil {
		return nil, errors.New("invalid SPAN: missing start point")
	} else if loc.End == nil {
		return nil, errors.New("invalid SPAN: missing end point")
	}

	nl.Start = n.Point(loc.Start)
	nl.End = n.Point(loc.End)

	start, end := nl.Start.ByteOffset, nl.End.ByteOffset
	if start > end {
		return nil, fmt.Errorf("invalid SPAN: start (%d) is after end (%d)", start, end)
	}
	return nl, nil
}

var lineEnd = []byte("\n")

// Point returns a normalized point within the Normalizer's text.  A normalized
// point has all of its fields set consistently and clamped within the range
// [0,len(text)).
func (n *Normalizer) Point(p *xpb.Location_Point) *xpb.Location_Point {
	if p == nil {
		return nil
	}

	if p.ByteOffset > 0 {
		return n.ByteOffset(p.ByteOffset)
	} else if p.LineNumber > 0 {
		np := &xpb.Location_Point{
			LineNumber:   p.LineNumber,
			ColumnOffset: p.ColumnOffset,
		}

		if totalLines := int32(len(n.lineLen)); p.LineNumber > totalLines {
			np.LineNumber = totalLines
			np.ColumnOffset = n.lineLen[np.LineNumber-1] - 1
		}
		if np.ColumnOffset < 0 {
			np.ColumnOffset = 0
		} else if np.ColumnOffset > 0 {
			if lineLen := n.lineLen[np.LineNumber-1] - 1; p.ColumnOffset > lineLen {
				np.ColumnOffset = lineLen
			}
		}

		np.ByteOffset = n.prefixLen[np.LineNumber-1] + np.ColumnOffset

		return np
	}

	return &xpb.Location_Point{LineNumber: 1}
}

// ByteOffset returns a normalized point based on the given offset within the
// Normalizer's text.  A normalized point has all of its fields set consistently
// and clamped within the range [0,len(text)).
func (n *Normalizer) ByteOffset(offset int32) *xpb.Location_Point {
	np := &xpb.Location_Point{ByteOffset: offset}
	if np.ByteOffset > n.textLen {
		np.ByteOffset = n.textLen
	}

	np.LineNumber = int32(sort.Search(len(n.lineLen), func(i int) bool {
		return n.prefixLen[i] > np.ByteOffset
	}))
	np.ColumnOffset = np.ByteOffset - n.prefixLen[np.LineNumber-1]

	return np
}

// ConvertFilters converts each filter glob into an equivalent regexp.
func ConvertFilters(filters []string) []*regexp.Regexp {
	var patterns []*regexp.Regexp
	for _, filter := range filters {
		patterns = append(patterns, filterToRegexp(filter))
	}
	return patterns
}

var filterOpsRE = regexp.MustCompile("[*][*]|[*?]")

func filterToRegexp(pattern string) *regexp.Regexp {
	var re string
	for {
		loc := filterOpsRE.FindStringIndex(pattern)
		if loc == nil {
			break
		}
		re += regexp.QuoteMeta(pattern[:loc[0]])
		switch pattern[loc[0]:loc[1]] {
		case "**":
			re += ".*"
		case "*":
			re += "[^/]*"
		case "?":
			re += "[^/]"
		default:
			log.Fatal("Unknown filter operator: " + pattern[loc[0]:loc[1]])
		}
		pattern = pattern[loc[1]:]
	}
	return regexp.MustCompile(re + regexp.QuoteMeta(pattern))
}

// MatchesAny reports whether if str matches any of the patterns
func MatchesAny(str string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if p.MatchString(str) {
			return true
		}
	}
	return false
}

func forAllEdges(ctx context.Context, service Service, source stringset.Set, edge []string, f func(source, target, targetKind, edgeKind string) error) error {
	if source.Empty() {
		return nil
	}
	req := &xpb.EdgesRequest{
		Ticket: source.Elements(),
		Kind:   edge,
		Filter: []string{facts.NodeKind},
	}
	edges, err := AllEdges(ctx, service, req)
	if err != nil {
		return err
	}
	for source, es := range edges.EdgeSets {
		for edgeKind, group := range es.Groups {
			for _, edge := range group.Edge {
				info, foundInfo := edges.Nodes[edge.TargetTicket]
				kind := ""
				if foundInfo {
					kind = string(info.Facts[facts.NodeKind])
				}
				err = f(source, edge.TargetTicket, kind, edgeKind)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

const (
	// The maximum number of times Callers can expand its frontier.
	maxCallersExpansions = 10
	// The maximum size of the node set Callers will consider. This includes ref/call anchors and functions.
	maxCallersNodeSetSize = 1024
)

// Expand frontier until it includes all nodes connected by defines/binding, completes,
// completes/uniquely, and (optionally) overrides edges. Return the resulting set of
// tickets with anchors removed.
func expandDefRelatedNodeSet(ctx context.Context, service Service, frontier stringset.Set, includeOverrides bool) (stringset.Set, error) {
	var edgeKinds []string
	// From the schema, we know the source of a defines* or completes* edge should be an anchor. Because of this,
	// we don't have to partition our set/queries on node kind.
	if includeOverrides {
		edgeKinds = []string{
			edges.Overrides,
			edges.Mirror(edges.Overrides),
			edges.DefinesBinding,
			edges.Mirror(edges.DefinesBinding),
			edges.Completes,
			edges.Mirror(edges.Completes),
			edges.CompletesUniquely,
			edges.Mirror(edges.CompletesUniquely),
		}
	} else {
		edgeKinds = []string{
			edges.DefinesBinding,
			edges.Mirror(edges.DefinesBinding),
			edges.Completes,
			edges.Mirror(edges.Completes),
			edges.CompletesUniquely,
			edges.Mirror(edges.CompletesUniquely),
		}
	}
	// We keep a worklist of anchors and semantic nodes that are reachable from undirected edges
	// marked [overrides,] defines/binding, completes, or completes/uniquely. This overestimates
	// possible calls because it ignores link structure.
	// TODO(zarko): We need to mark nodes we reached through overrides.
	// All nodes that we've visited.
	var retired, anchors stringset.Set
	iterations := 0
	for len(retired) < maxCallersNodeSetSize && len(frontier) != 0 && iterations < maxCallersExpansions {
		iterations++
		// Nodes that we haven't visited yet but will the next time around the loop.
		next := stringset.New()
		err := forAllEdges(ctx, service, frontier, edgeKinds, func(_, target, kind, _ string) error {
			if kind == nodes.Anchor {
				anchors.Add(target)
			}
			if !retired.Contains(target) && !frontier.Contains(target) {
				next.Add(target)
			}
			return nil
		})
		for old := range frontier {
			retired.Add(old)
		}
		frontier = next
		if err != nil {
			return nil, err
		}
	}
	if len(retired) > maxCallersNodeSetSize {
		log.Printf("Callers iteration truncated (set too big)")
	}
	if iterations >= maxCallersExpansions {
		log.Printf("Callers iteration truncated (too many expansions)")
	}
	for ticket := range anchors {
		retired.Discard(ticket)
	}
	return retired, nil
}

// SlowCallersForCrossReferences is an implementation of callgraph support meant
// for intermediate-term use by CrossReferences.
func SlowCallersForCrossReferences(ctx context.Context, service Service, includeOverrides, generateSignatures bool, ticket string) ([]*xpb.CrossReferencesReply_RelatedAnchor, error) {
	ticket, err := kytheuri.Fix(ticket)
	if err != nil {
		return nil, err
	}
	callees, err := expandDefRelatedNodeSet(ctx, service, stringset.New(ticket), includeOverrides)
	if err != nil {
		return nil, err
	}
	if callees.Empty() {
		return nil, nil
	}
	// This will not recursively call SlowCallersForCrossReferences as we're requesting NO_CALLERS above.
	xrefs, err := service.CrossReferences(ctx, &xpb.CrossReferencesRequest{
		Ticket:            callees.Elements(),
		DefinitionKind:    xpb.CrossReferencesRequest_NO_DEFINITIONS,
		ReferenceKind:     xpb.CrossReferencesRequest_CALL_REFERENCES,
		DocumentationKind: xpb.CrossReferencesRequest_NO_DOCUMENTATION,
		CallerKind:        xpb.CrossReferencesRequest_NO_CALLERS,
		AnchorText:        true,
		PageSize:          math.MaxInt32,
	})
	if err != nil {
		return nil, fmt.Errorf("can't get CrossReferences for xref references set: %v", err)
	}
	if xrefs.NextPageToken != "" {
		return nil, errors.New("UNIMPLEMENTED: Paged CrossReferences reply")
	}
	// Now we've got a bunch of call site anchors. We need to figure out which functions they belong to.
	// anchors is the set of all callsite anchor tickets.
	var anchors stringset.Set
	// callsite anchor ticket to anchor details
	expandedAnchors := make(map[string]*xpb.Anchor)
	// semantic node ticket to owned callsite anchor tickets
	parentToAnchor := make(map[string][]string)
	for _, refs := range xrefs.CrossReferences {
		for _, ref := range refs.Reference {
			if ref.Anchor.Kind == edges.RefCall {
				anchors.Add(ref.Anchor.Ticket)
				expandedAnchors[ref.Anchor.Ticket] = ref.Anchor
			}
		}
	}
	var parentTickets []string
	if err := forAllEdges(ctx, service, anchors, []string{edges.ChildOf}, func(source, target, kind, _ string) error {
		anchor, ok := expandedAnchors[source]
		if !ok {
			log.Printf("Warning: missing expanded anchor for %v", source)
			return nil
		}
		// anchor.Parent is the syntactic parent of the anchor; we want the semantic parent.
		if target != anchor.Parent && kind != nodes.Anchor {
			if _, exists := parentToAnchor[target]; !exists {
				parentTickets = append(parentTickets, target)
			}
			parentToAnchor[target] = append(parentToAnchor[target], source)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// Get definition-site anchors. This won't recurse through this function because we're requesting NO_CALLERS.
	defs, err := service.CrossReferences(ctx, &xpb.CrossReferencesRequest{
		Ticket:         parentTickets,
		DefinitionKind: xpb.CrossReferencesRequest_ALL_DEFINITIONS,
		AnchorText:     true,
		PageSize:       math.MaxInt32,
	})
	if err != nil {
		return nil, fmt.Errorf("can't get CrossReferences for xref references set: %v", err)
	}
	if defs.NextPageToken != "" {
		log.Printf("Warning: paged CrossReferences reply looking for ALL_DEFINITIONS")
	}
	for ticket, refs := range defs.CrossReferences {
		for _, def := range refs.Definition {
			// This chooses the last Anchor arbitrarily.
			expandedAnchors[ticket] = def.Anchor
		}
	}
	var relatedAnchors []*xpb.CrossReferencesReply_RelatedAnchor
	for caller, calls := range parentToAnchor {
		callerAnchor, found := expandedAnchors[caller]
		if !found {
			log.Printf("Warning: missing expanded anchor for caller %v", ticket)
			continue
		}
		var displayName *xpb.Printable
		if generateSignatures {
			displayName, err = SlowSignature(ctx, service, caller)
			if err != nil {
				return nil, fmt.Errorf("error looking up signature for caller ticket %q: %v", caller, err)
			}
		}
		var sites []*xpb.Anchor
		for _, ticket := range calls {
			related, found := expandedAnchors[ticket]
			if found {
				sites = append(sites, related)
			} else {
				log.Printf("Warning: missing expanded anchor for callsite %v", ticket)
			}
		}
		// See below regarding output stability.
		sort.Sort(byAnchor(sites))
		relatedAnchors = append(relatedAnchors, &xpb.CrossReferencesReply_RelatedAnchor{
			Anchor:      callerAnchor,
			DisplayName: displayName,
			Site:        sites,
			Ticket:      caller,
		})
	}
	// For pagination to work correctly, we must return RelatedAnchors in the same
	// order for the same request. Iterations over the parentToAnchor map are
	// not guaranteed to follow the same order, so relatedAnchors is arbitrarily
	// permuted. Undo this here.
	sort.Sort(byRelatedAnchor(relatedAnchors))
	return relatedAnchors, nil
}

// byAnchor implements sort.Interface for []*xpb.Anchor based on the Ticket field.
type byAnchor []*xpb.Anchor

func (a byAnchor) Len() int           { return len(a) }
func (a byAnchor) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byAnchor) Less(i, j int) bool { return a[i].Ticket < a[j].Ticket }

// byRelatedAnchor implements sort.Interface for []*xpb.CrossReferencesReply_RelatedAnchor
// based on the Ticket field.
type byRelatedAnchor []*xpb.CrossReferencesReply_RelatedAnchor

func (a byRelatedAnchor) Len() int           { return len(a) }
func (a byRelatedAnchor) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byRelatedAnchor) Less(i, j int) bool { return a[i].Ticket < a[j].Ticket }

const (
	// The maximum number of times to recur in signature generation.
	maxFormatExpansions = 10
)

// extractParams extracts target tickets of param edges into a string slice ordered by ordinal.
func extractParams(edges []*xpb.EdgeSet_Group_Edge) []string {
	arr := make([]string, len(edges))
	for _, edge := range edges {
		ordinal := int(edge.Ordinal)
		if ordinal >= len(arr) || ordinal < 0 || arr[ordinal] != "" {
			for edgeix, edge := range edges {
				arr[edgeix] = edge.TargetTicket
			}
			return arr
		}
		arr[ordinal] = edge.TargetTicket
	}
	return arr
}

// getLanguage returns the language field of a Kythe ticket.
func getLanguage(ticket string) string {
	uri, err := kytheuri.Parse(ticket)
	if err != nil {
		return ""
	}
	return uri.Language
}

// slowLookupMeta retrieves the meta node for some node kind in some language.
func slowLookupMeta(ctx context.Context, service Service, language string, kind string) (string, error) {
	uri := kytheuri.URI{Language: language, Signature: kind + "#meta"}
	req := &xpb.NodesRequest{
		Ticket: []string{uri.String()},
		Filter: []string{facts.Format},
	}
	nodes, err := service.Nodes(ctx, req)
	if err != nil {
		return "", fmt.Errorf("during slowLookupMeta: %v", err)
	}
	for _, node := range nodes.Nodes {
		for _, value := range node.Facts {
			return string(value), nil
		}
	}
	return "", nil
}

// findParam finds the ticket for param number num in edges. It returns the empty string if a match wasn't found.
func findParam(reply *xpb.EdgesReply, num int) string {
	for _, set := range reply.EdgeSets {
		for kind, group := range set.Groups {
			if kind == edges.Param {
				for _, edge := range group.Edge {
					if int(edge.Ordinal) == num {
						return edge.TargetTicket
					}
				}
			}
		}
	}
	return ""
}

func getFactValue(edges *xpb.EdgesReply, ticket, fact string) []byte {
	if n := edges.Nodes[ticket]; n != nil {
		return n.Facts[fact]
	}
	return nil
}

// findFormatAndKind finds the format and kind facts associated with ticket in edges. It returns empty strings for
// either fact if they aren't found.
func findFormatAndKind(edges *xpb.EdgesReply, ticket string) (string, string) {
	return string(getFactValue(edges, ticket, facts.Format)), string(getFactValue(edges, ticket, facts.NodeKind))
}

// slowSignatureForBacktick handles signature generation for the %N` format token.
// typeTicket should be the type being inspected. num is the N from the token. links is the list of `xpb.Link` to which new links will be appended.
// It will return the expansion of the backtick signature and a (possibly appended to) slice of links (or "", nil, error).
func slowSignatureForBacktick(ctx context.Context, service Service, typeTicket string, depth, num int, links []*xpb.Link) (string, []*xpb.Link, error) {
	if typeTicket != "" {
		req := &xpb.EdgesRequest{
			Ticket:   []string{typeTicket},
			Kind:     []string{edges.Param},
			PageSize: math.MaxInt32,
			Filter:   []string{facts.NodeKind, facts.Format},
		}
		edges, err := AllEdges(ctx, service, req)
		if err != nil {
			return "", nil, fmt.Errorf("fetching parameter edges: %v", err)
		}
		if selectedParam := findParam(edges, num); selectedParam != "" {
			selectedFormat, selectedKind := findFormatAndKind(edges, selectedParam)
			return slowSignatureLevel(ctx, service, selectedParam, selectedKind, selectedFormat, depth+1, links)
		}
	}
	return "", links, nil
}

// SignatureDetails contains information from the graph used by slowSignatureLevel to build signatures.
type SignatureDetails struct {
	// Maps from tickets to node kinds or formats.
	kinds, formats map[string]string
	// The ticket that the described node is typed as.
	typeTicket string
	// The tickets of this node's parameters (in parameter order).
	params []string
	// The parents of this node in some arbitrary order as returned by the underlying service.
	parents []string
}

// findSignatureDetails fills a SignatureDetails from the graph.
func findSignatureDetails(ctx context.Context, service Service, ticket string) (*SignatureDetails, error) {
	req := &xpb.EdgesRequest{
		Ticket:   []string{ticket},
		Kind:     []string{edges.Named, edges.Param, edges.Typed, edges.ChildOf},
		PageSize: math.MaxInt32,
		Filter:   []string{facts.NodeKind, facts.Format},
	}
	allEdges, err := AllEdges(ctx, service, req)
	if err != nil {
		return &SignatureDetails{}, fmt.Errorf("during AllEdges in findSignatureDetails: %v", err)
	}
	details := SignatureDetails{
		kinds:   make(map[string]string),
		formats: make(map[string]string),
	}
	for nodeTicket, node := range allEdges.Nodes {
		details.kinds[nodeTicket] = string(node.Facts[facts.NodeKind])
		details.formats[nodeTicket] = string(node.Facts[facts.Format])
	}
	for _, set := range allEdges.EdgeSets {
		if group := set.Groups[edges.Typed]; group != nil {
			for _, edge := range group.Edge {
				details.typeTicket = edge.TargetTicket
			}
		}
		if group := set.Groups[edges.Param]; group != nil {
			details.params = extractParams(group.Edge)
		}
		if group := set.Groups[edges.ChildOf]; group != nil {
			for _, edge := range group.Edge {
				details.parents = append(details.parents, edge.TargetTicket)
			}
		}
	}
	return &details, nil
}

// Kinds of blocks we might be inside while rendering a signature.
const (
	// Don't emit any text for this block. Don't emit any delimiters.
	ignoredBlock = '_'
	// Pass through text from this block. Don't emit any delimiters.
	passBlock = 'p'
	// Delimit this block and tag it as a list.
	listBlock = 'l'
	// Delimit this block and tag it as important.
	importantBlock = 'i'
)

// slowSignatureLevel uses formats to traverse the graph to produce a signature string for the given ticket.
// links is the list of `xpb.Link` to which new links will be appended.
// It will return the expansion of the signature and a (possibly appended to) slice of links (or "", nil, error).
func slowSignatureLevel(ctx context.Context, service Service, ticket, kind, format string, depth int, links []*xpb.Link) (string, []*xpb.Link, error) {
	if depth > maxFormatExpansions {
		return "...", links, nil
	}
	if kind == "" {
		log.Printf("Node %v missing kind", ticket)
	}
	details, err := findSignatureDetails(ctx, service, ticket)
	if err != nil {
		return "", nil, fmt.Errorf("during findSignatureDetails in slowSignatureLevel: %v", err)
	}
	// tapp nodes allow their 0th param to provide a format.
	if format == "" && kind == nodes.TApp && len(details.params) > 0 && details.formats[details.params[0]] != "" {
		format = details.formats[details.params[0]]
	}
	// Try looking for a meta node as a last resort.
	if format == "" {
		format, err = slowLookupMeta(ctx, service, getLanguage(ticket), kind)
		if err != nil {
			return "", nil, fmt.Errorf("during slowLookupMeta in slowSignatureLevel: %v", err)
		}
		if format == "" {
			log.Printf("Could not deduce format for node %v", ticket)
			return "", links, nil
		}
	}
	// The name we're building up.
	var name string
	// Offset starting an integer in a format token.
	numBegin := 0
	// Was the last character we saw a %?
	sawEscape := false
	// Are we currently parsing an integer?
	parsingNum := false
	// Were the last two characters we saw %[?
	sawOpenBracket := false
	// The stack of open brace lists.
	var braces []byte
	// How many ignored blocks are open.
	ignored := 0
	for i, c := range format {
		switch {
		case sawOpenBracket:
			sawOpenBracket = false
			switch c {
			default:
				log.Printf("Bad escape character %c after %%[ in format %v for node %v (ignored)", c, format, ticket)
			case 'i':
				if depth == 0 {
					if ignored == 0 {
						links = append(links, &xpb.Link{Kind: xpb.Link_IMPORTANT})
						name += "["
					}
					braces = append(braces, importantBlock)
				} else {
					braces = append(braces, passBlock)
				}
			case 'p':
				if ignored == 0 {
					links = append(links, &xpb.Link{Kind: xpb.Link_LIST})
					name += "["
				}
				braces = append(braces, listBlock)
			case '^':
				// Add the signature of the node being expanded's parent node.
				if len(details.parents) > 0 {
					if ignored == 0 {
						parent := details.parents[0]
						var pName string
						pName, links, err = slowSignatureLevel(ctx, service, parent, details.kinds[parent], details.formats[parent], depth+1, links)
						if err != nil {
							return "", nil, fmt.Errorf("while formatting node %v with format %v: %v", ticket, format, err)
						}
						name += pName
					}
					braces = append(braces, passBlock)
				} else {
					braces = append(braces, ignoredBlock)
					ignored++
				}
			}
		case parsingNum:
			if unicode.IsNumber(c) {
				continue
			}
			num, _ := strconv.Atoi(format[numBegin:i])
			parsingNum = false
			switch c {
			default:
				log.Printf("Bad escape character %c in format %v for node %v", c, format, ticket)
			case '.':
				// Pick a single parameter.
				if num < len(details.params) && ignored == 0 {
					param := details.params[num]
					var pName string
					pName, links, err = slowSignatureLevel(ctx, service, param, details.kinds[param], details.formats[param], depth+1, links)
					if err != nil {
						return "", nil, fmt.Errorf("while formatting node %v with format %v: %v", ticket, format, err)
					}
					name += pName
				}
			case '`':
				// Pick a single parameter from the type of the node being expanded.
				if ignored == 0 {
					if details.typeTicket == "" {
						log.Printf("No type found processing format %v for node %v", format, ticket)
					} else {
						var pName string
						pName, links, err = slowSignatureForBacktick(ctx, service, details.typeTicket, depth, num, links)
						if err != nil {
							return "", nil, fmt.Errorf("while formatting node %v with format %v: %v", ticket, format, err)
						}
						name += pName
					}
				}
			case ',':
				// Build a comma-separated list of the signatures of all parameters starting at some parameter.
				if num < len(details.params) && ignored == 0 {
					for p := num; p < len(details.params); p++ {
						if p != num {
							name += ", "
						}
						param := details.params[p]
						links = append(links, &xpb.Link{Kind: xpb.Link_LIST_ITEM})
						name += "["
						var pName string
						pName, links, err = slowSignatureLevel(ctx, service, param, details.kinds[param], details.formats[param], depth+1, links)
						if err != nil {
							return "", nil, fmt.Errorf("while formatting node %v with format %v: %v", ticket, format, err)
						}
						name += pName + "]"
					}
				}
			}
		case sawEscape:
			sawEscape = false
			if unicode.IsNumber(c) {
				parsingNum = true
				numBegin = i
				continue
			}
			switch c {
			default:
				log.Printf("Bad escape character %c in format %v for node %v", c, format, ticket)
			case ']':
				if len(braces) == 0 {
					log.Printf("Brace stack underflow in format %v for node %v", format, ticket)
				} else {
					switch braces[len(braces)-1] {
					case listBlock:
						if ignored == 0 {
							name += "]"
						}
					case importantBlock:
						if ignored == 0 {
							name += "]"
						}
					case ignoredBlock:
						ignored--
					}
					braces = braces[:len(braces)-1]
				}
			case '[':
				sawOpenBracket = true
			case '%':
				// Identity-escape %.
				if ignored == 0 {
					name += "%"
				}
			case '^':
				// Add the signature of the node being expanded's parent node.
				if ignored == 0 {
					if len(details.parents) > 0 {
						parent := details.parents[0]
						var pName string
						pName, links, err = slowSignatureLevel(ctx, service, parent, details.kinds[parent], details.formats[parent], depth+1, links)
						if err != nil {
							return "", nil, fmt.Errorf("while formatting node %v with format %v: %v", ticket, format, err)
						}
						name += pName
					} else {
						log.Printf("No parents found processing format %v for node %v", format, ticket)
					}
				}
			}
		default:
			switch c {
			case '%':
				sawEscape = true
			default:
				if ignored == 0 {
					if c == '[' || c == ']' {
						name += "\\"
					}
					name += string(c)
				}
			}
		}
	}
	if sawEscape || parsingNum || sawOpenBracket {
		log.Printf("Unterminated escape processing format %v for node %v", format, ticket)
	}
	if len(braces) != 0 {
		log.Printf("Unterminated brace processing format %v for node %v", format, ticket)
		for i := 0; i < len(braces); i++ {
			switch braces[len(braces)-i-1] {
			case listBlock:
				if ignored == 0 {
					name += "]"
				}
			case importantBlock:
				if ignored == 0 {
					name += "]"
				}
			case ignoredBlock:
				ignored--
			}
		}
	}
	return name, links, nil
}

// SlowSignature uses formats to generate a xpb.DocumentationReply_Printable given a ticket.
// In the simplest case, if a node's format is some literal "foo", the Printable's text would be "foo".
// Other formats may refer to related nodes; for example, "%^::bar" would append "::bar" to a node's
// parent's (recursively-defined) signature.
// See http://www.kythe.io/docs/schema/#formats for details.
func SlowSignature(ctx context.Context, service Service, ticket string) (*xpb.Printable, error) {
	req := &xpb.NodesRequest{
		Ticket: []string{ticket},
		Filter: []string{facts.NodeKind, facts.Format},
	}
	nodes, err := service.Nodes(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("during Nodes in SlowSignature: %v", err)
	}
	if len(nodes.Nodes) == 0 {
		return nil, fmt.Errorf("could not find node %v", ticket)
	}

	var kind, format string
	for _, node := range nodes.Nodes {
		kind = string(node.Facts[facts.NodeKind])
		format = string(node.Facts[facts.Format])
	}

	text, links, err := slowSignatureLevel(ctx, service, ticket, kind, format, 0, nil)
	if err != nil {
		return nil, err
	}
	return &xpb.Printable{RawText: text, Link: links}, nil
}

// Data from a doc node that documents some other ticket.
type associatedDocNode struct {
	rawText string
	link    []*xpb.Link
	// Tickets this associatedDocNode documents.
	documented []string
}

// A document being compiled for Documentation.
type preDocument struct {
	// The Document being compiled.
	document *xpb.DocumentationReply_Document
	// Maps from tickets to doc nodes.
	docNode map[string]*associatedDocNode
}

// Context for executing Documentation calls.
type documentDetails struct {
	// Maps doc tickets to associatedDocNodes.
	docTicketToAssocNode map[string]*associatedDocNode
	// Maps tickets to Documents being prepared. More than one ticket may map to the same preDocument
	// (e.g., if they are in the same equivalence class defined by expandDefRelatedNodeSet.
	ticketToPreDocument map[string]*preDocument
	// Parent and type information for all expandDefRelatedNodeSet-expanded nodes.
	ticketToParent, ticketToType map[string]string
	// Tickets of known doc nodes.
	docs stringset.Set
}

// getDocRelatedNodes fills details with information about the kinds, parents, types, and associated doc nodes of allTickets.
func getDocRelatedNodes(ctx context.Context, service Service, details documentDetails, allTickets stringset.Set) error {
	// We can't ask for text facts here (since they get filtered out).
	dreq := &xpb.EdgesRequest{
		Ticket:   allTickets.Elements(),
		Kind:     []string{edges.Mirror(edges.Documents), edges.ChildOf, edges.Typed},
		PageSize: math.MaxInt32,
		Filter:   []string{facts.NodeKind},
	}
	dedges, err := AllEdges(ctx, service, dreq)
	if err != nil {
		return fmt.Errorf("couldn't AllEdges: %v", err)
	}
	for nodeTicket, node := range dedges.Nodes {
		kind := string(node.Facts[facts.NodeKind])
		if kind == nodes.Doc {
			details.docs.Add(nodeTicket)
		}
	}
	for sourceTicket, set := range dedges.EdgeSets {
		if group := set.Groups[edges.ChildOf]; group != nil {
			for _, edge := range group.Edge {
				details.ticketToParent[sourceTicket] = edge.TargetTicket
			}
		}
		if group := set.Groups[edges.Typed]; group != nil {
			for _, edge := range group.Edge {
				details.ticketToType[sourceTicket] = edge.TargetTicket
			}
		}
		if group := set.Groups[edges.Mirror(edges.Documents)]; group != nil {
			for _, edge := range group.Edge {
				if preDoc := details.ticketToPreDocument[sourceTicket]; preDoc != nil {
					assocNode := details.docTicketToAssocNode[edge.TargetTicket]
					if assocNode == nil {
						assocNode = &associatedDocNode{}
						details.docTicketToAssocNode[edge.TargetTicket] = assocNode
					}
					assocNode.documented = append(assocNode.documented, sourceTicket)
					preDoc.docNode[edge.TargetTicket] = assocNode
				}
			}
		}
	}
	return nil
}

// getDocText gets text for doc nodes we've found.
func getDocText(ctx context.Context, service Service, details documentDetails) error {
	if details.docs.Empty() {
		return nil
	}
	nreq := &xpb.NodesRequest{
		Ticket: details.docs.Elements(),
		Filter: []string{facts.Text},
	}
	nodes, err := service.Nodes(ctx, nreq)
	if err != nil {
		return fmt.Errorf("error in Nodes during getDocText: %v", err)
	}
	for nodeTicket, node := range nodes.Nodes {
		if text, ok := node.Facts[facts.Text]; ok {
			if assocNode := details.docTicketToAssocNode[nodeTicket]; assocNode != nil {
				assocNode.rawText = string(text)
			}
		}
	}
	return nil
}

// getDocLinks gets links for doc nodes we've found.
func getDocLinks(ctx context.Context, service Service, details documentDetails) error {
	if details.docs.Empty() {
		return nil
	}
	preq := &xpb.EdgesRequest{
		Ticket:   details.docs.Elements(),
		Kind:     []string{edges.Param},
		PageSize: math.MaxInt32,
	}
	pedges, err := AllEdges(ctx, service, preq)
	if err != nil {
		return fmt.Errorf("error in Edges during getDocLinks: %v", err)
	}
	for sourceTicket, set := range pedges.EdgeSets {
		if assocDoc := details.docTicketToAssocNode[sourceTicket]; assocDoc != nil {
			for _, group := range set.Groups {
				params := extractParams(group.Edge)
				assocDoc.link = make([]*xpb.Link, len(params))
				for i, param := range extractParams(group.Edge) {
					assocDoc.link[i] = &xpb.Link{Definition: []string{param}}
				}
			}
		}
	}
	return nil
}

// compilePreDocument finishes and returns the Document attached to ticket's preDocument.
func compilePreDocument(ctx context.Context, service Service, details documentDetails, ticket string, preDocument *preDocument) (*xpb.DocumentationReply_Document, error) {
	document := preDocument.document
	sig, err := SlowSignature(ctx, service, ticket)
	if err != nil {
		return nil, fmt.Errorf("can't get SlowSignature for %v: %v", ticket, err)
	}
	document.Signature = sig
	ty, ok := details.ticketToType[ticket]
	if ok {
		tystr, err := SlowSignature(ctx, service, ty)
		if err != nil {
			return nil, fmt.Errorf("can't get SlowSignature for type %v: %v", ty, err)
		}
		document.Type = tystr
	}
	parent, ok := details.ticketToParent[ticket]
	if ok {
		parentstr, err := SlowSignature(ctx, service, parent)
		if err != nil {
			return nil, fmt.Errorf("can't get SlowSignature for parent %v: %v", parent, err)
		}
		document.DefinedBy = parentstr
	}
	text := &xpb.Printable{}
	document.Text = text
	for _, assocDoc := range preDocument.docNode {
		text.RawText = text.RawText + assocDoc.rawText
		text.Link = append(text.Link, assocDoc.link...)
	}
	return document, nil
}

func getLinkTickets(p *xpb.Printable, s stringset.Set) {
	if p == nil {
		return
	}
	for _, l := range p.Link {
		for _, d := range l.Definition {
			s.Add(d)
		}
	}
}

// SlowDocumentation is an implementation of the Documentation API built from other APIs.
func SlowDocumentation(ctx context.Context, service Service, req *xpb.DocumentationRequest) (*xpb.DocumentationReply, error) {
	tickets, err := FixTickets(req.Ticket)
	if err != nil {
		return nil, err
	}
	details := documentDetails{
		ticketToPreDocument:  make(map[string]*preDocument),
		ticketToParent:       make(map[string]string),
		ticketToType:         make(map[string]string),
		docTicketToAssocNode: make(map[string]*associatedDocNode),
		docs:                 make(stringset.Set),
	}
	// We assume that expandDefRelatedNodeSet will return disjoint sets (and thus we can treat them as equivalence classes
	// with the original request's ticket as the characteristic element).
	var allTickets stringset.Set
	for _, ticket := range tickets {
		// TODO(zarko): Include outbound override edges.
		ticketSet, err := expandDefRelatedNodeSet(ctx, service, stringset.New(ticket) /*includeOverrides=*/, false)
		if err != nil {
			return nil, fmt.Errorf("couldn't expandDefRelatedNodeSet in Documentation: %v", err)
		}
		document := &preDocument{document: &xpb.DocumentationReply_Document{Ticket: ticket}, docNode: make(map[string]*associatedDocNode)}
		for ticket := range ticketSet {
			allTickets.Add(ticket)
			details.ticketToPreDocument[ticket] = document
		}
	}
	if err = getDocRelatedNodes(ctx, service, details, allTickets); err != nil {
		return nil, fmt.Errorf("couldn't getDocRelatedNodes: %v", err)
	}
	if err = getDocText(ctx, service, details); err != nil {
		return nil, fmt.Errorf("couldn't getDocText: %v", err)
	}
	if err = getDocLinks(ctx, service, details); err != nil {
		return nil, fmt.Errorf("couldn't getDocLinks: %v", err)
	}
	// Finish up and attach the Documents we've built to the reply.
	reply := &xpb.DocumentationReply{}
	var definitionSet stringset.Set
	for _, ticket := range tickets {
		preDocument, found := details.ticketToPreDocument[ticket]
		if !found {
			continue
		}
		document, err := compilePreDocument(ctx, service, details, ticket, preDocument)
		if err != nil {
			return nil, fmt.Errorf("during compilePreDocument for %v: %v", ticket, err)
		}
		definitionSet.Add(document.Ticket)
		getLinkTickets(document.Text, definitionSet)
		getLinkTickets(document.Signature, definitionSet)
		getLinkTickets(document.Type, definitionSet)
		getLinkTickets(document.Initializer, definitionSet)
		getLinkTickets(document.DefinedBy, definitionSet)
		reply.Document = append(reply.Document, document)
	}
	defs, err := SlowDefinitions(ctx, service, definitionSet.Elements())
	if err != nil {
		return nil, fmt.Errorf("during SlowDefinitions for %v: %v", definitionSet, err)
	}
	if len(defs) != 0 {
		reply.DefinitionLocations = make(map[string]*xpb.Anchor, len(defs))
		for _, def := range defs {
			reply.DefinitionLocations[def.Ticket] = def
		}
	}
	nodes, err := service.Nodes(ctx, &xpb.NodesRequest{
		Filter: req.Filter,
		Ticket: definitionSet.Elements(),
	})
	if len(nodes.Nodes) != 0 {
		reply.Nodes = make(map[string]*xpb.NodeInfo, len(nodes.Nodes))
		for node, info := range nodes.Nodes {
			if def, ok := defs[node]; ok {
				info.Definition = def.Ticket
			}
			reply.Nodes[node] = info
		}
	}
	return reply, nil
}

type grpcClient struct{ xpb.XRefServiceClient }

// Nodes implements part of the Service interface.
func (w *grpcClient) Nodes(ctx context.Context, req *xpb.NodesRequest) (*xpb.NodesReply, error) {
	return w.XRefServiceClient.Nodes(ctx, req)
}

// Edges implements part of the Service interface.
func (w *grpcClient) Edges(ctx context.Context, req *xpb.EdgesRequest) (*xpb.EdgesReply, error) {
	return w.XRefServiceClient.Edges(ctx, req)
}

// Decorations implements part of the Service interface.
func (w *grpcClient) Decorations(ctx context.Context, req *xpb.DecorationsRequest) (*xpb.DecorationsReply, error) {
	return w.XRefServiceClient.Decorations(ctx, req)
}

// CrossReferences implements part of the Service interface.
func (w *grpcClient) CrossReferences(ctx context.Context, req *xpb.CrossReferencesRequest) (*xpb.CrossReferencesReply, error) {
	return w.XRefServiceClient.CrossReferences(ctx, req)
}

// Documentation implements part of the Service interface.
func (w *grpcClient) Documentation(ctx context.Context, req *xpb.DocumentationRequest) (*xpb.DocumentationReply, error) {
	return w.XRefServiceClient.Documentation(ctx, req)
}

// GRPC returns an xrefs Service backed by the given GRPC client and context.
func GRPC(c xpb.XRefServiceClient) Service { return &grpcClient{c} }

type webClient struct{ addr string }

// Nodes implements part of the Service interface.
func (w *webClient) Nodes(ctx context.Context, q *xpb.NodesRequest) (*xpb.NodesReply, error) {
	var reply xpb.NodesReply
	return &reply, web.Call(w.addr, "nodes", q, &reply)
}

// Edges implements part of the Service interface.
func (w *webClient) Edges(ctx context.Context, q *xpb.EdgesRequest) (*xpb.EdgesReply, error) {
	var reply xpb.EdgesReply
	return &reply, web.Call(w.addr, "edges", q, &reply)
}

// Decorations implements part of the Service interface.
func (w *webClient) Decorations(ctx context.Context, q *xpb.DecorationsRequest) (*xpb.DecorationsReply, error) {
	var reply xpb.DecorationsReply
	return &reply, web.Call(w.addr, "decorations", q, &reply)
}

// CrossReferences implements part of the Service interface.
func (w *webClient) CrossReferences(ctx context.Context, q *xpb.CrossReferencesRequest) (*xpb.CrossReferencesReply, error) {
	var reply xpb.CrossReferencesReply
	return &reply, web.Call(w.addr, "xrefs", q, &reply)
}

// Documentation implements part of the Service interface.
func (w *webClient) Documentation(ctx context.Context, q *xpb.DocumentationRequest) (*xpb.DocumentationReply, error) {
	var reply xpb.DocumentationReply
	return &reply, web.Call(w.addr, "documentation", q, &reply)
}

// WebClient returns an xrefs Service based on a remote web server.
func WebClient(addr string) Service {
	return &webClient{addr}
}

// RegisterHTTPHandlers registers JSON HTTP handlers with mux using the given
// xrefs Service.  The following methods with be exposed:
//
//   GET /nodes
//     Request: JSON encoded xrefs.NodesRequest
//     Response: JSON encoded xrefs.NodesReply
//   GET /edges
//     Request: JSON encoded xrefs.EdgesRequest
//     Response: JSON encoded xrefs.EdgesReply
//   GET /decorations
//     Request: JSON encoded xrefs.DecorationsRequest
//     Response: JSON encoded xrefs.DecorationsReply
//   GET /xrefs
//     Request: JSON encoded xrefs.CrossReferencesRequest
//     Response: JSON encoded xrefs.CrossReferencesReply
//   GET /documentation
//     Request: JSON encoded xrefs.DocumentationRequest
//     Response: JSON encoded xrefs.DocumentationReply
//
// Note: /nodes, /edges, /decorations, and /xrefs will return their responses as
// serialized protobufs if the "proto" query parameter is set.
func RegisterHTTPHandlers(ctx context.Context, xs Service, mux *http.ServeMux) {
	mux.HandleFunc("/xrefs", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("xrefs.CrossReferences:\t%s", time.Since(start))
		}()
		var req xpb.CrossReferencesRequest
		if err := web.ReadJSONBody(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply, err := xs.CrossReferences(ctx, &req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := web.WriteResponse(w, r, reply); err != nil {
			log.Println(err)
		}
	})
	mux.HandleFunc("/decorations", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("xrefs.Decorations:\t%s", time.Since(start))
		}()
		var req xpb.DecorationsRequest
		if err := web.ReadJSONBody(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply, err := xs.Decorations(ctx, &req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := web.WriteResponse(w, r, reply); err != nil {
			log.Println(err)
		}
	})
	mux.HandleFunc("/documentation", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("xrefs.Documentation:\t%s", time.Since(start))
		}()
		var req xpb.DocumentationRequest
		if err := web.ReadJSONBody(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply, err := xs.Documentation(ctx, &req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := web.WriteResponse(w, r, reply); err != nil {
			log.Println(err)
		}
	})
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("xrefs.Nodes:\t%s", time.Since(start))
		}()

		var req xpb.NodesRequest
		if err := web.ReadJSONBody(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply, err := xs.Nodes(ctx, &req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := web.WriteResponse(w, r, reply); err != nil {
			log.Println(err)
		}
	})
	mux.HandleFunc("/edges", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("xrefs.Edges:\t%s", time.Since(start))
		}()

		var req xpb.EdgesRequest
		if err := web.ReadJSONBody(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply, err := xs.Edges(ctx, &req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := web.WriteResponse(w, r, reply); err != nil {
			log.Println(err)
		}
	})
}

// ByName orders a slice of facts by their fact names.
type ByName []*cpb.Fact

func (s ByName) Len() int           { return len(s) }
func (s ByName) Less(i, j int) bool { return s[i].Name < s[j].Name }
func (s ByName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// ByOrdinal orders the edges in an edge group by their ordinals, with ties
// broken by ticket.
type ByOrdinal []*xpb.EdgeSet_Group_Edge

func (s ByOrdinal) Len() int      { return len(s) }
func (s ByOrdinal) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s ByOrdinal) Less(i, j int) bool {
	if s[i].Ordinal == s[j].Ordinal {
		return s[i].TargetTicket < s[j].TargetTicket
	}
	return s[i].Ordinal < s[j].Ordinal
}
