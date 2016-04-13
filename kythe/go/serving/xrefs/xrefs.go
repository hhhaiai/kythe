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

// Package xrefs provides a high-performance serving table implementation of the
// xrefs.Service.
//
// Table format:
//   edgeSets:<ticket>      -> srvpb.PagedEdgeSet
//   edgePages:<page_key>   -> srvpb.EdgePage
//   decor:<ticket>         -> srvpb.FileDecorations
//   xrefs:<ticket>         -> srvpb.PagedCrossReferences
//   xrefPages:<page_key>   -> srvpb.PagedCrossReferences_Page
package xrefs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"kythe.io/kythe/go/services/xrefs"
	"kythe.io/kythe/go/storage/table"
	"kythe.io/kythe/go/util/kytheuri"
	"kythe.io/kythe/go/util/schema"
	"kythe.io/kythe/go/util/stringset"

	cpb "kythe.io/kythe/proto/common_proto"
	srvpb "kythe.io/kythe/proto/serving_proto"
	xpb "kythe.io/kythe/proto/xref_proto"

	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
)

type edgeSetResult struct {
	PagedEdgeSet *srvpb.PagedEdgeSet

	Err error
}

type staticLookupTables interface {
	pagedEdgeSets(ctx context.Context, tickets []string) (<-chan edgeSetResult, error)
	edgePage(ctx context.Context, key string) (*srvpb.EdgePage, error)
	fileDecorations(ctx context.Context, ticket string) (*srvpb.FileDecorations, error)
	crossReferences(ctx context.Context, ticket string) (*srvpb.PagedCrossReferences, error)
	crossReferencesPage(ctx context.Context, key string) (*srvpb.PagedCrossReferences_Page, error)
}

// SplitTable implements the xrefs Service interface using separate static
// lookup tables for each API component.
type SplitTable struct {
	// Edges is a table of srvpb.PagedEdgeSets keyed by their source tickets.
	Edges table.ProtoBatch

	// EdgePages is a table of srvpb.EdgePages keyed by their page keys.
	EdgePages table.Proto

	// Decorations is a table of srvpb.FileDecorations keyed by their source
	// location tickets.
	Decorations table.Proto

	// CrossReferences is a table of srvpb.PagedCrossReferences keyed by their
	// source node tickets.
	CrossReferences table.Proto

	// CrossReferencePages is a table of srvpb.PagedCrossReferences_Pages keyed by
	// their page keys.
	CrossReferencePages table.Proto
}

func lookupPagedEdgeSets(ctx context.Context, tbl table.ProtoBatch, keys [][]byte) (<-chan edgeSetResult, error) {
	rs, err := tbl.LookupBatch(ctx, keys, (*srvpb.PagedEdgeSet)(nil))
	if err != nil {
		return nil, err
	}
	ch := make(chan edgeSetResult)
	go func() {
		defer close(ch)
		for r := range rs {
			if r.Err == table.ErrNoSuchKey {
				log.Printf("Could not locate edges with key %q", r.Key)
				ch <- edgeSetResult{Err: r.Err}
				continue
			} else if r.Err != nil {
				ticket := strings.TrimPrefix(string(r.Key), edgeSetsTablePrefix)
				ch <- edgeSetResult{
					Err: fmt.Errorf("edges lookup error (ticket %q): %v", ticket, r.Err),
				}
				continue
			}

			ch <- edgeSetResult{PagedEdgeSet: r.Value.(*srvpb.PagedEdgeSet)}
		}
	}()
	return ch, nil
}

func toKeys(ss []string) [][]byte {
	keys := make([][]byte, len(ss), len(ss))
	for i, s := range ss {
		keys[i] = []byte(s)
	}
	return keys
}

func (s *SplitTable) pagedEdgeSets(ctx context.Context, tickets []string) (<-chan edgeSetResult, error) {
	return lookupPagedEdgeSets(ctx, s.Edges, toKeys(tickets))
}
func (s *SplitTable) edgePage(ctx context.Context, key string) (*srvpb.EdgePage, error) {
	var ep srvpb.EdgePage
	return &ep, s.EdgePages.Lookup(ctx, []byte(key), &ep)
}
func (s *SplitTable) fileDecorations(ctx context.Context, ticket string) (*srvpb.FileDecorations, error) {
	var fd srvpb.FileDecorations
	return &fd, s.Decorations.Lookup(ctx, []byte(ticket), &fd)
}
func (s *SplitTable) crossReferences(ctx context.Context, ticket string) (*srvpb.PagedCrossReferences, error) {
	var cr srvpb.PagedCrossReferences
	return &cr, s.CrossReferences.Lookup(ctx, []byte(ticket), &cr)
}
func (s *SplitTable) crossReferencesPage(ctx context.Context, key string) (*srvpb.PagedCrossReferences_Page, error) {
	var p srvpb.PagedCrossReferences_Page
	return &p, s.CrossReferencePages.Lookup(ctx, []byte(key), &p)
}

// Key prefixes for the combinedTable implementation.
const (
	crossRefTablePrefix     = "xrefs:"
	crossRefPageTablePrefix = "xrefPages:"
	decorTablePrefix        = "decor:"
	edgeSetsTablePrefix     = "edgeSets:"
	edgePagesTablePrefix    = "edgePages:"
)

type combinedTable struct{ table.ProtoBatch }

func (c *combinedTable) pagedEdgeSets(ctx context.Context, tickets []string) (<-chan edgeSetResult, error) {
	keys := make([][]byte, len(tickets), len(tickets))
	for i, ticket := range tickets {
		keys[i] = EdgeSetKey(ticket)
	}
	return lookupPagedEdgeSets(ctx, c, keys)
}
func (c *combinedTable) edgePage(ctx context.Context, key string) (*srvpb.EdgePage, error) {
	var ep srvpb.EdgePage
	return &ep, c.Lookup(ctx, EdgePageKey(key), &ep)
}
func (c *combinedTable) fileDecorations(ctx context.Context, ticket string) (*srvpb.FileDecorations, error) {
	var fd srvpb.FileDecorations
	return &fd, c.Lookup(ctx, DecorationsKey(ticket), &fd)
}
func (c *combinedTable) crossReferences(ctx context.Context, ticket string) (*srvpb.PagedCrossReferences, error) {
	var cr srvpb.PagedCrossReferences
	return &cr, c.Lookup(ctx, CrossReferencesKey(ticket), &cr)
}
func (c *combinedTable) crossReferencesPage(ctx context.Context, key string) (*srvpb.PagedCrossReferences_Page, error) {
	var p srvpb.PagedCrossReferences_Page
	return &p, c.Lookup(ctx, CrossReferencesPageKey(key), &p)
}

// NewSplitTable returns an xrefs.Service based on the given serving tables for
// each API component.
func NewSplitTable(c *SplitTable) xrefs.Service { return &tableImpl{c} }

// NewCombinedTable returns an xrefs.Service for the given combined xrefs
// serving table.  The table's keys are expected to be constructed using only
// the EdgeSetKey, EdgePageKey, and DecorationsKey functions.
func NewCombinedTable(t table.ProtoBatch) xrefs.Service { return &tableImpl{&combinedTable{t}} }

// EdgeSetKey returns the edgeset CombinedTable key for the given source ticket.
func EdgeSetKey(ticket string) []byte {
	return []byte(edgeSetsTablePrefix + ticket)
}

// EdgePageKey returns the edgepage CombinedTable key for the given key.
func EdgePageKey(key string) []byte {
	return []byte(edgePagesTablePrefix + key)
}

// DecorationsKey returns the decorations CombinedTable key for the given source
// location ticket.
func DecorationsKey(ticket string) []byte {
	return []byte(decorTablePrefix + ticket)
}

// CrossReferencesKey returns the cross-references CombinedTable key for the
// given node ticket.
func CrossReferencesKey(ticket string) []byte {
	return []byte(crossRefTablePrefix + ticket)
}

// CrossReferencesPageKey returns the cross-references page CombinedTable key
// for the given key.
func CrossReferencesPageKey(key string) []byte {
	return []byte(crossRefPageTablePrefix + key)
}

// tableImpl implements the xrefs Service interface using static lookup tables.
type tableImpl struct{ staticLookupTables }

// Nodes implements part of the xrefs Service interface.
func (t *tableImpl) Nodes(ctx context.Context, req *xpb.NodesRequest) (*xpb.NodesReply, error) {
	tickets, err := xrefs.FixTickets(req.Ticket)
	if err != nil {
		return nil, err
	}

	rs, err := t.pagedEdgeSets(ctx, tickets)
	if err != nil {
		return nil, err
	}
	defer func() {
		// drain channel in case of errors
		for _ = range rs {
		}
	}()

	reply := &xpb.NodesReply{}
	patterns := xrefs.ConvertFilters(req.Filter)

	for r := range rs {
		if r.Err == table.ErrNoSuchKey {
			continue
		} else if r.Err != nil {
			return nil, r.Err
		}
		node := r.PagedEdgeSet.Source
		ni := &xpb.NodeInfo{Ticket: node.Ticket}
		for _, f := range node.Fact {
			if len(patterns) == 0 || xrefs.MatchesAny(f.Name, patterns) {
				ni.Fact = append(ni.Fact, f)
			}
		}
		if len(ni.Fact) > 0 {
			sort.Sort(xrefs.ByName(ni.Fact))
			reply.Node = append(reply.Node, ni)
		}
	}
	return reply, nil
}

const (
	defaultPageSize = 2048
	maxPageSize     = 10000
)

// Edges implements part of the xrefs Service interface.
func (t *tableImpl) Edges(ctx context.Context, req *xpb.EdgesRequest) (*xpb.EdgesReply, error) {
	tickets, err := xrefs.FixTickets(req.Ticket)
	if err != nil {
		return nil, err
	}

	allowedKinds := stringset.New(req.Kind...)
	return t.edges(ctx, edgesRequest{
		Tickets: tickets,
		Filters: req.Filter,
		Kinds: func(kind string) bool {
			return len(allowedKinds) == 0 || allowedKinds.Contains(kind)
		},

		PageSize:  int(req.PageSize),
		PageToken: req.PageToken,
	})
}

type edgesRequest struct {
	Tickets []string
	Filters []string
	Kinds   func(string) bool

	PageSize  int
	PageToken string
}

func (t *tableImpl) edges(ctx context.Context, req edgesRequest) (*xpb.EdgesReply, error) {
	stats := filterStats{
		max: int(req.PageSize),
	}
	if stats.max < 0 {
		return nil, fmt.Errorf("invalid page_size: %d", req.PageSize)
	} else if stats.max == 0 {
		stats.max = defaultPageSize
	} else if stats.max > maxPageSize {
		stats.max = maxPageSize
	}

	if req.PageToken != "" {
		rec, err := base64.StdEncoding.DecodeString(req.PageToken)
		if err != nil {
			return nil, fmt.Errorf("invalid page_token: %q", req.PageToken)
		}
		var t srvpb.PageToken
		if err := proto.Unmarshal(rec, &t); err != nil || t.Index < 0 {
			return nil, fmt.Errorf("invalid page_token: %q", req.PageToken)
		}
		stats.skip = int(t.Index)
	}
	pageToken := stats.skip

	var totalEdgesPossible int

	nodeTickets := stringset.New()

	rs, err := t.pagedEdgeSets(ctx, req.Tickets)
	if err != nil {
		return nil, err
	}
	defer func() {
		// drain channel in case of errors or early return
		for _ = range rs {
		}
	}()

	patterns := xrefs.ConvertFilters(req.Filters)

	reply := &xpb.EdgesReply{}
	for r := range rs {
		if r.Err == table.ErrNoSuchKey {
			continue
		} else if r.Err != nil {
			return nil, r.Err
		}
		pes := r.PagedEdgeSet
		totalEdgesPossible += totalEdgesWithKinds(pes, req.Kinds)

		// Don't scan the EdgeSet_Groups if we're already at the specified page_size.
		if stats.total == stats.max {
			continue
		}

		var groups []*xpb.EdgeSet_Group
		for _, grp := range pes.Group {
			if req.Kinds == nil || req.Kinds(grp.Kind) {
				ng, ns := stats.filter(grp)
				if ng != nil {
					for _, n := range ns {
						if len(patterns) > 0 && !nodeTickets.Contains(n.Ticket) {
							nodeTickets.Add(n.Ticket)
							reply.Node = append(reply.Node, nodeToInfo(patterns, n))
						}
					}
					groups = append(groups, ng)
					if stats.total == stats.max {
						break
					}
				}
			}
		}

		// TODO(schroederc): ensure that pes.EdgeSet.Groups and pes.PageIndexes of
		// the same kind are grouped together in the EdgesReply

		if stats.total != stats.max {
			for _, idx := range pes.PageIndex {
				if req.Kinds == nil || req.Kinds(idx.EdgeKind) {
					if stats.skipPage(idx) {
						log.Printf("Skipping EdgePage: %s", idx.PageKey)
						continue
					}

					log.Printf("Retrieving EdgePage: %s", idx.PageKey)
					ep, err := t.edgePage(ctx, idx.PageKey)
					if err == table.ErrNoSuchKey {
						return nil, fmt.Errorf("internal error: missing edge page: %q", idx.PageKey)
					} else if err != nil {
						return nil, fmt.Errorf("edge page lookup error (page key: %q): %v", idx.PageKey, err)
					}

					ng, ns := stats.filter(ep.EdgesGroup)
					if ng != nil {
						for _, n := range ns {
							if len(patterns) > 0 && !nodeTickets.Contains(n.Ticket) {
								nodeTickets.Add(n.Ticket)
								reply.Node = append(reply.Node, nodeToInfo(patterns, n))
							}
						}
						groups = append(groups, ng)
						if stats.total == stats.max {
							break
						}
					}
				}
			}
		}

		if len(groups) > 0 {
			reply.EdgeSet = append(reply.EdgeSet, &xpb.EdgeSet{
				SourceTicket: pes.Source.Ticket,
				Group:        groups,
			})

			if len(patterns) > 0 && !nodeTickets.Contains(pes.Source.Ticket) {
				nodeTickets.Add(pes.Source.Ticket)
				reply.Node = append(reply.Node, nodeToInfo(patterns, pes.Source))
			}
		}
	}
	if stats.total > stats.max {
		log.Panicf("totalEdges greater than maxEdges: %d > %d", stats.total, stats.max)
	} else if pageToken+stats.total > totalEdgesPossible && pageToken <= totalEdgesPossible {
		log.Panicf("pageToken+totalEdges greater than totalEdgesPossible: %d+%d > %d", pageToken, stats.total, totalEdgesPossible)
	}

	if pageToken+stats.total != totalEdgesPossible && stats.total != 0 {
		rec, err := proto.Marshal(&srvpb.PageToken{Index: int32(pageToken + stats.total)})
		if err != nil {
			return nil, fmt.Errorf("internal error: error marshalling page token: %v", err)
		}
		reply.NextPageToken = base64.StdEncoding.EncodeToString(rec)
	}
	return reply, nil
}

func totalEdgesWithKinds(pes *srvpb.PagedEdgeSet, kindFilter func(string) bool) int {
	if kindFilter == nil {
		return int(pes.TotalEdges)
	}
	var total int
	for _, grp := range pes.Group {
		if kindFilter(grp.Kind) {
			total += len(grp.Edge)
		}
	}
	for _, page := range pes.PageIndex {
		if kindFilter(page.EdgeKind) {
			total += int(page.EdgeCount)
		}
	}
	return total
}

type filterStats struct {
	skip, total, max int
}

func (s *filterStats) skipPage(idx *srvpb.PageIndex) bool {
	if int(idx.EdgeCount) <= s.skip {
		s.skip -= int(idx.EdgeCount)
		return true
	}
	return false
}

func (s *filterStats) filter(g *srvpb.EdgeGroup) (*xpb.EdgeSet_Group, []*srvpb.Node) {
	edges := g.Edge
	if len(edges) <= s.skip {
		s.skip -= len(edges)
		return nil, nil
	} else if s.skip > 0 {
		edges = edges[s.skip:]
		s.skip = 0
	}

	if len(edges) > s.max-s.total {
		edges = edges[:(s.max - s.total)]
	}

	s.total += len(edges)

	targets := make([]*srvpb.Node, len(edges))
	for i, e := range edges {
		targets[i] = e.Target
	}

	return &xpb.EdgeSet_Group{
		Kind: g.Kind,
		Edge: e2e(edges),
	}, targets
}

func e2e(es []*srvpb.EdgeGroup_Edge) []*xpb.EdgeSet_Group_Edge {
	edges := make([]*xpb.EdgeSet_Group_Edge, len(es))
	for i, e := range es {
		edges[i] = &xpb.EdgeSet_Group_Edge{
			TargetTicket: e.Target.Ticket,
			Ordinal:      e.Ordinal,
		}
	}
	return edges
}

func nodeToInfo(patterns []*regexp.Regexp, n *srvpb.Node) *xpb.NodeInfo {
	ni := &xpb.NodeInfo{Ticket: n.Ticket}
	for _, f := range n.Fact {
		if xrefs.MatchesAny(f.Name, patterns) {
			ni.Fact = append(ni.Fact, f)
		}
	}
	sort.Sort(xrefs.ByName(ni.Fact))
	return ni
}

// Decorations implements part of the xrefs Service interface.
func (t *tableImpl) Decorations(ctx context.Context, req *xpb.DecorationsRequest) (*xpb.DecorationsReply, error) {
	if req.GetLocation() == nil || req.GetLocation().Ticket == "" {
		return nil, errors.New("missing location")
	}

	ticket, err := kytheuri.Fix(req.GetLocation().Ticket)
	if err != nil {
		return nil, fmt.Errorf("invalid ticket %q: %v", req.GetLocation().Ticket, err)
	}

	decor, err := t.fileDecorations(ctx, ticket)
	if err == table.ErrNoSuchKey {
		return nil, xrefs.ErrDecorationsNotFound
	} else if err != nil {
		return nil, fmt.Errorf("lookup error for file decorations %q: %v", ticket, err)
	}

	text := decor.File.Text
	if len(req.DirtyBuffer) > 0 {
		text = req.DirtyBuffer
	}
	norm := xrefs.NewNormalizer(text)

	loc, err := norm.Location(req.GetLocation())
	if err != nil {
		return nil, err
	}

	reply := &xpb.DecorationsReply{Location: loc}

	if req.SourceText {
		reply.Encoding = decor.File.Encoding
		if loc.Kind == xpb.Location_FILE {
			reply.SourceText = text
		} else {
			reply.SourceText = text[loc.Start.ByteOffset:loc.End.ByteOffset]
		}
	}

	if req.References {
		patterns := xrefs.ConvertFilters(req.Filter)

		var patcher *xrefs.Patcher
		if len(req.DirtyBuffer) > 0 {
			patcher = xrefs.NewPatcher(decor.File.Text, req.DirtyBuffer)
		}

		// The span with which to constrain the set of returned anchor references.
		var startBoundary, endBoundary int32
		spanKind := req.SpanKind
		if loc.Kind == xpb.Location_FILE {
			startBoundary = 0
			endBoundary = int32(len(text))
			spanKind = xpb.DecorationsRequest_WITHIN_SPAN
		} else {
			startBoundary = loc.Start.ByteOffset
			endBoundary = loc.End.ByteOffset
		}

		reply.Reference = make([]*xpb.DecorationsReply_Reference, 0, len(decor.Decoration))
		refs := make(map[string][]*xpb.DecorationsReply_Reference)
		nodeTargets := make(map[string]string)

		for _, d := range decor.Decoration {
			start, end, exists := patcher.Patch(d.Anchor.StartOffset, d.Anchor.EndOffset)
			// Filter non-existent anchor.  Anchors can no longer exist if we were
			// given a dirty buffer and the anchor was inside a changed region.
			if exists {
				if xrefs.InSpanBounds(spanKind, start, end, startBoundary, endBoundary) {
					d.Anchor.StartOffset = start
					d.Anchor.EndOffset = end

					r := decorationToReference(norm, d)
					refs[r.TargetTicket] = append(refs[r.TargetTicket], r)
					reply.Reference = append(reply.Reference, r)

					if _, ok := nodeTargets[d.Target.Ticket]; len(patterns) > 0 && !ok {
						reply.Node = append(reply.Node, nodeToInfo(patterns, d.Target))
					}
					nodeTargets[d.Target.Ticket] = d.Target.Ticket
				}
			}
		}

		// TODO(schroederc): break apart Decorations method
		if req.TargetDefinitions {
			reply.DefinitionLocations = make(map[string]*xpb.Anchor)

			const maxJumps = 2
			for i := 0; i < maxJumps && len(nodeTargets) > 0; i++ {
				tickets := make([]string, 0, len(nodeTargets))
				for ticket := range nodeTargets {
					tickets = append(tickets, ticket)
				}

				// TODO(schroederc): cache this in the serving data
				xReply, err := t.CrossReferences(ctx, &xpb.CrossReferencesRequest{
					Ticket:         tickets,
					DefinitionKind: xpb.CrossReferencesRequest_BINDING_DEFINITIONS,

					// Get node kinds of related nodes for indirect definitions
					Filter: []string{schema.NodeKindFact},
				})
				if err != nil {
					return nil, fmt.Errorf("error loading reference target locations: %v", err)
				}

				nextJump := make(map[string]string)

				// Give client a definition location for each reference that has only 1
				// definition location which is not itself.
				//
				// If a node does not have a single definition, but does have a relevant
				// relation to another node, try to find a single definition for the
				// related node instead.
				for ticket, cr := range xReply.CrossReferences {
					refTicket := nodeTargets[ticket]
					if len(cr.Definition) == 1 {
						loc := cr.Definition[0]
						for _, r := range refs[refTicket] {
							if loc.Ticket != r.SourceTicket {
								r.TargetDefinition = loc.Ticket
								if _, ok := reply.DefinitionLocations[loc.Ticket]; !ok {
									// TODO(schroederc): handle differing kinds; completes vs. binding
									loc.Kind = ""
									reply.DefinitionLocations[loc.Ticket] = loc
								}
							}
						}
					} else {
						// Look for relevant node relations for an indirect definition
						var relevant []string
						for _, n := range cr.RelatedNode {
							switch n.RelationKind {
							case revCallableAs: // Jump from a callable
								relevant = append(relevant, n.Ticket)
							}
						}

						if len(relevant) == 1 {
							nextJump[relevant[0]] = refTicket
						}
					}
				}

				nodeTargets = nextJump
			}
		}
	}

	return reply, nil
}

var revCallableAs = schema.MirrorEdge(schema.CallableAsEdge)

type span struct{ start, end int32 }

func decorationToReference(norm *xrefs.Normalizer, d *srvpb.FileDecorations_Decoration) *xpb.DecorationsReply_Reference {
	return &xpb.DecorationsReply_Reference{
		SourceTicket: d.Anchor.Ticket,
		TargetTicket: d.Target.Ticket,
		Kind:         d.Kind,
		AnchorStart:  norm.ByteOffset(d.Anchor.StartOffset),
		AnchorEnd:    norm.ByteOffset(d.Anchor.EndOffset),
	}
}

// CrossReferences implements part of the xrefs.Service interface.
func (t *tableImpl) CrossReferences(ctx context.Context, req *xpb.CrossReferencesRequest) (*xpb.CrossReferencesReply, error) {
	tickets, err := xrefs.FixTickets(req.Ticket)
	if err != nil {
		return nil, err
	}

	stats := refStats{
		max: int(req.PageSize),
	}
	if stats.max < 0 {
		return nil, fmt.Errorf("invalid page_size: %d", req.PageSize)
	} else if stats.max == 0 {
		stats.max = defaultPageSize
	} else if stats.max > maxPageSize {
		stats.max = maxPageSize
	}

	var edgesPageToken string
	if req.PageToken != "" {
		rec, err := base64.StdEncoding.DecodeString(req.PageToken)
		if err != nil {
			return nil, fmt.Errorf("invalid page_token: %q", req.PageToken)
		}
		var t srvpb.PageToken
		if err := proto.Unmarshal(rec, &t); err != nil || t.Index < 0 {
			return nil, fmt.Errorf("invalid page_token: %q", req.PageToken)
		}
		stats.skip = int(t.Index)
		edgesPageToken = t.SecondaryToken
	}
	pageToken := stats.skip

	var totalRefsPossible int

	reply := &xpb.CrossReferencesReply{
		CrossReferences: make(map[string]*xpb.CrossReferencesReply_CrossReferenceSet, len(req.Ticket)),
		Nodes:           make(map[string]*xpb.NodeInfo, len(req.Ticket)),
	}
	var nextToken *srvpb.PageToken

	if edgesPageToken == "" &&
		(req.DefinitionKind != xpb.CrossReferencesRequest_NO_DEFINITIONS ||
			req.ReferenceKind != xpb.CrossReferencesRequest_NO_REFERENCES ||
			req.DocumentationKind != xpb.CrossReferencesRequest_NO_DOCUMENTATION) {
		for _, ticket := range tickets {
			// TODO(schroederc): retrieve PagedCrossReferences in parallel
			cr, err := t.crossReferences(ctx, ticket)
			if err == table.ErrNoSuchKey {
				log.Println("Missing CrossReferences:", ticket)
				continue
			} else if err != nil {
				return nil, fmt.Errorf("error looking up cross-references for ticket %q: %v", ticket, err)
			}

			crs := &xpb.CrossReferencesReply_CrossReferenceSet{
				Ticket: ticket,
			}
			for _, grp := range cr.Group {
				if xrefs.IsDefKind(req.DefinitionKind, grp.Kind, cr.Incomplete) {
					totalRefsPossible += len(grp.Anchor)
					if stats.addAnchors(&crs.Definition, grp.Anchor, req.AnchorText) {
						break
					}
				} else if xrefs.IsDeclKind(req.DeclarationKind, grp.Kind, cr.Incomplete) {
					totalRefsPossible += len(grp.Anchor)
					if stats.addAnchors(&crs.Declaration, grp.Anchor, req.AnchorText) {
						break
					}
				} else if xrefs.IsDocKind(req.DocumentationKind, grp.Kind) {
					totalRefsPossible += len(grp.Anchor)
					if stats.addAnchors(&crs.Documentation, grp.Anchor, req.AnchorText) {
						break
					}
				} else if xrefs.IsRefKind(req.ReferenceKind, grp.Kind) {
					totalRefsPossible += len(grp.Anchor)
					if stats.addAnchors(&crs.Reference, grp.Anchor, req.AnchorText) {
						break
					}
				}
			}

			if stats.total < stats.max {
				for _, idx := range cr.PageIndex {

					// TODO(schroederc): skip entire read if s.skip >= idx.Count
					p, err := t.crossReferencesPage(ctx, idx.PageKey)
					if err != nil {
						return nil, fmt.Errorf("internal error: error retrieving cross-references page: %v", idx.PageKey)
					}

					if xrefs.IsDefKind(req.DefinitionKind, p.Group.Kind, cr.Incomplete) {
						totalRefsPossible += len(p.Group.Anchor)
						if stats.addAnchors(&crs.Definition, p.Group.Anchor, req.AnchorText) {
							break
						}
					} else if xrefs.IsDeclKind(req.DeclarationKind, p.Group.Kind, cr.Incomplete) {
						totalRefsPossible += len(p.Group.Anchor)
						if stats.addAnchors(&crs.Declaration, p.Group.Anchor, req.AnchorText) {
							break
						}
					} else if xrefs.IsDocKind(req.DocumentationKind, p.Group.Kind) {
						totalRefsPossible += len(p.Group.Anchor)
						if stats.addAnchors(&crs.Documentation, p.Group.Anchor, req.AnchorText) {
							break
						}
					} else {
						totalRefsPossible += len(p.Group.Anchor)
						if stats.addAnchors(&crs.Reference, p.Group.Anchor, req.AnchorText) {
							break
						}
					}
				}
			}

			if len(crs.Definition) > 0 || len(crs.Reference) > 0 || len(crs.Documentation) > 0 {
				reply.CrossReferences[crs.Ticket] = crs
			}
		}

		if pageToken+stats.total != totalRefsPossible && stats.total != 0 {
			nextToken = &srvpb.PageToken{Index: int32(pageToken + stats.total)}
		}
	}

	if len(req.Filter) > 0 && stats.total < stats.max {
		er, err := t.edges(ctx, edgesRequest{
			Tickets:   tickets,
			Filters:   req.Filter,
			Kinds:     func(kind string) bool { return !schema.IsAnchorEdge(kind) },
			PageToken: edgesPageToken,
			PageSize:  stats.max - stats.total,
		})
		if err != nil {
			return nil, fmt.Errorf("error getting related nodes: %v", err)
		}
		for _, es := range er.EdgeSet {
			ticket := es.SourceTicket
			nodes := stringset.New()
			crs, ok := reply.CrossReferences[ticket]
			if !ok {
				crs = &xpb.CrossReferencesReply_CrossReferenceSet{
					Ticket: ticket,
				}
			}
			for _, g := range es.Group {
				if !schema.IsAnchorEdge(g.Kind) {
					for _, edge := range g.Edge {
						nodes.Add(edge.TargetTicket)
						crs.RelatedNode = append(crs.RelatedNode, &xpb.CrossReferencesReply_RelatedNode{
							RelationKind: g.Kind,
							Ticket:       edge.TargetTicket,
							Ordinal:      edge.Ordinal,
						})
					}
				}
			}
			if len(nodes) > 0 {
				for _, n := range er.Node {
					if nodes.Contains(n.Ticket) {
						reply.Nodes[n.Ticket] = n
					}
				}
			}

			if !ok && len(crs.RelatedNode) > 0 {
				reply.CrossReferences[ticket] = crs
			}
		}

		if er.NextPageToken != "" {
			nextToken = &srvpb.PageToken{SecondaryToken: er.NextPageToken}
		}
	}

	if nextToken != nil {
		rec, err := proto.Marshal(nextToken)
		if err != nil {
			return nil, fmt.Errorf("internal error: error marshalling page token: %v", err)
		}
		reply.NextPageToken = base64.StdEncoding.EncodeToString(rec)
	}

	return reply, nil
}

type refStats struct {
	skip, total, max int
}

func (s *refStats) addAnchors(to *[]*xpb.Anchor, as []*srvpb.ExpandedAnchor, anchorText bool) bool {
	if s.total == s.max {
		return true
	} else if s.skip > len(as) {
		s.skip -= len(as)
		return false
	} else if s.skip > 0 {
		as = as[s.skip:]
		s.skip = 0
	}

	if s.total+len(as) > s.max {
		as = as[:(s.max - s.total)]
	}
	s.total += len(as)
	for _, a := range as {
		*to = append(*to, a2a(a, anchorText))
	}
	return s.total == s.max
}

func a2a(a *srvpb.ExpandedAnchor, anchorText bool) *xpb.Anchor {
	var text string
	if anchorText {
		text = a.Text
	}
	return &xpb.Anchor{
		Ticket:       a.Ticket,
		Kind:         schema.Canonicalize(a.Kind),
		Parent:       a.Parent,
		Text:         text,
		Start:        p2p(a.Span.Start),
		End:          p2p(a.Span.End),
		Snippet:      a.Snippet,
		SnippetStart: p2p(a.SnippetSpan.Start),
		SnippetEnd:   p2p(a.SnippetSpan.End),
	}
}

func p2p(p *cpb.Point) *xpb.Location_Point {
	return &xpb.Location_Point{
		ByteOffset:   p.ByteOffset,
		LineNumber:   p.LineNumber,
		ColumnOffset: p.ColumnOffset,
	}
}

// Callers implements part of the xrefs Service interface.
func (t *tableImpl) Callers(ctx context.Context, req *xpb.CallersRequest) (*xpb.CallersReply, error) {
	return xrefs.SlowCallers(ctx, t, req)
}

// Callers implements part of the xrefs Service interface.
func (t *tableImpl) Documentation(ctx context.Context, req *xpb.DocumentationRequest) (*xpb.DocumentationReply, error) {
	return xrefs.SlowDocumentation(ctx, t, req)
}
