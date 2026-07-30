// Package browse resolves OPC UA browse names to concrete NodeIDs by
// searching the server's address space, so node mappings can reference a
// node by its browse name instead of a raw NodeID string.
package browse

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"

	"github.com/mwieczorkiewicz/opcua_exporter/internal/config"
)

// maxBrowseDepth bounds how many hierarchical-reference hops the resolver
// will follow from the search root. Not user-configurable: 10 hops covers
// virtually any real device/object tree while keeping a runaway or cyclic
// address space from making startup take unreasonably long.
const maxBrowseDepth = 10

// browseClient is the minimal subset of opcua.ClientInterface this package
// needs. Accepting this narrow interface (rather than *opcua.Client or the
// full opcua.ClientInterface) keeps ResolveBrowseNames unit-testable with a
// small hand-written fake; *opcua.Client satisfies it structurally.
type browseClient interface {
	Browse(context.Context, *ua.BrowseRequest) (*ua.BrowseResponse, error)
	BrowseNext(context.Context, *ua.BrowseNextRequest) (*ua.BrowseNextResponse, error)
}

// ResolveBrowseNames returns mappings where every entry has a concrete
// NodeName, resolving any mapping with BrowseName set by searching the
// address space rooted at rootNodeID (a raw NodeID string; an empty string
// means the standard Objects folder, ns=0;i=85).
//
// Mappings that already have NodeName set pass through unchanged. A browse
// name that resolves to zero nodes is logged and dropped from the result
// (not fatal). A browse name that resolves to more than one node is logged
// (with every matching NodeID) and makes the whole call fail: ResolveBrowseNames
// returns a non-nil error and the caller should abort startup, since silently
// picking one of several matches could subscribe to the wrong node.
//
// maxItemsPerRequest caps how many nodes go into a single Browse call (the
// same limit used for CreateMonitoredItems when subscribing): some OPC UA
// servers reject a request with too many operations at once
// (StatusBadTooManyOperations) when an address-space level is wide. A value
// <= 0 means no limit: each level is browsed in a single request.
func ResolveBrowseNames(ctx context.Context, client browseClient, rootNodeID string, maxItemsPerRequest int, mappings []config.NodeMapping) ([]config.NodeMapping, error) {
	wanted := make(map[string][]int) // browseName -> indices into mappings needing that name
	for i, m := range mappings {
		if m.BrowseName != "" {
			wanted[m.BrowseName] = append(wanted[m.BrowseName], i)
		}
	}

	if len(wanted) == 0 {
		return mappings, nil // nobody uses browseName: skip the traversal entirely
	}

	root := ua.NewTwoByteNodeID(id.ObjectsFolder)
	if rootNodeID != "" {
		parsedRoot, err := ua.ParseNodeID(rootNodeID)
		if err != nil {
			return nil, fmt.Errorf("invalid browseRoot %q: %w", rootNodeID, err)
		}
		root = parsedRoot
	}

	matches := searchAddressSpace(ctx, client, root, maxItemsPerRequest, wanted)

	var duplicates []string
	result := make([]config.NodeMapping, 0, len(mappings))
	for _, m := range mappings {
		if m.BrowseName == "" {
			result = append(result, m) // already had NodeName
			continue
		}
		ids := matches[m.BrowseName]
		switch len(ids) {
		case 0:
			log.Printf("browse name %q not found under root %s (searched to depth %d); skipping node mapping for metric %q", m.BrowseName, root, maxBrowseDepth, m.MetricName)
		case 1:
			resolved := m
			resolved.NodeName = ids[0].String()
			log.Printf("resolved browse name %q -> %s for metric %q", m.BrowseName, resolved.NodeName, m.MetricName)
			result = append(result, resolved)
		default:
			idStrs := make([]string, len(ids))
			for i, nid := range ids {
				idStrs[i] = nid.String()
			}
			log.Printf("browse name %q is ambiguous, matched %d nodes %v; metric %q", m.BrowseName, len(ids), idStrs, m.MetricName)
			duplicates = append(duplicates, m.BrowseName)
		}
	}

	if len(duplicates) > 0 {
		return nil, fmt.Errorf("ambiguous browse name(s), see log above: %s", strings.Join(duplicates, ", "))
	}

	return result, nil
}

// searchAddressSpace performs one bounded-depth breadth-first walk from root,
// batching every node at a given depth into a single Browse call (following
// ContinuationPoints via BrowseNext as needed), and returns every NodeID
// whose BrowseName.Name is a key of wanted.
func searchAddressSpace(ctx context.Context, client browseClient, root *ua.NodeID, maxItemsPerRequest int, wanted map[string][]int) map[string][]*ua.NodeID {
	matches := make(map[string][]*ua.NodeID)

	visited := map[string]bool{root.String(): true}
	frontier := []*ua.NodeID{root}

	for depth := 0; depth < maxBrowseDepth && len(frontier) > 0; depth++ {
		refs := browseLevel(ctx, client, frontier, depth, maxItemsPerRequest)

		var next []*ua.NodeID
		for _, ref := range refs {
			if ref.NodeID == nil {
				continue
			}
			childID := ua.NewNodeIDFromExpandedNodeID(ref.NodeID)
			key := childID.String()
			if visited[key] {
				continue // dedup / cycle guard
			}
			visited[key] = true

			if ref.BrowseName != nil {
				if _, ok := wanted[ref.BrowseName.Name]; ok {
					matches[ref.BrowseName.Name] = append(matches[ref.BrowseName.Name], childID)
				}
			}
			next = append(next, childID)
		}
		frontier = next
	}

	return matches
}

// browseLevel covers every node in frontier, splitting it into chunks of at
// most maxItemsPerRequest (some servers reject a Browse request with too many
// operations at once; maxItemsPerRequest <= 0 means no limit, one request for
// the whole frontier), and returns the flattened reference list across all
// chunks. A failure in one chunk is logged and treated as "no children found"
// for the nodes in that chunk, without affecting the rest.
func browseLevel(ctx context.Context, client browseClient, frontier []*ua.NodeID, depth int, maxItemsPerRequest int) []*ua.ReferenceDescription {
	if maxItemsPerRequest <= 0 {
		return browseChunk(ctx, client, frontier, depth)
	}
	var refs []*ua.ReferenceDescription
	for i := 0; i < len(frontier); i += maxItemsPerRequest {
		end := min(i+maxItemsPerRequest, len(frontier))
		refs = append(refs, browseChunk(ctx, client, frontier[i:end], depth)...)
	}
	return refs
}

// browseChunk issues one batched BrowseRequest covering every node in chunk,
// follows ContinuationPoints via BrowseNext until each node's children are
// fully drained, and returns the flattened reference list. Errors are logged
// and treated as "no children found" for the affected node(s) rather than
// aborting the whole traversal.
func browseChunk(ctx context.Context, client browseClient, chunk []*ua.NodeID, depth int) []*ua.ReferenceDescription {
	descs := make([]*ua.BrowseDescription, len(chunk))
	for i, n := range chunk {
		descs[i] = &ua.BrowseDescription{
			NodeID:          n,
			BrowseDirection: ua.BrowseDirectionForward,
			ReferenceTypeID: ua.NewNumericNodeID(0, id.HierarchicalReferences),
			IncludeSubtypes: true,
			NodeClassMask:   uint32(ua.NodeClassAll),
			ResultMask:      uint32(ua.BrowseResultMaskAll),
		}
	}

	req := &ua.BrowseRequest{
		View:                          &ua.ViewDescription{ViewID: ua.NewTwoByteNodeID(0)},
		RequestedMaxReferencesPerNode: 0,
		NodesToBrowse:                 descs,
	}

	resp, err := client.Browse(ctx, req)
	if err != nil {
		log.Printf("browse name resolution: Browse failed at depth %d (%d nodes): %v; results at this level may be incomplete", depth, len(chunk), err)
		return nil
	}

	var refs []*ua.ReferenceDescription
	// resp.Results is positionally aligned with req.NodesToBrowse, per the OPC UA spec.
	for _, result := range resp.Results {
		if result == nil {
			continue
		}
		refs = append(refs, result.References...)
		cp := result.ContinuationPoint
		for len(cp) > 0 {
			nextResp, err := client.BrowseNext(ctx, &ua.BrowseNextRequest{
				ContinuationPoints: [][]byte{cp},
			})
			if err != nil {
				log.Printf("browse name resolution: BrowseNext failed at depth %d: %v; results at this level may be incomplete", depth, err)
				break
			}
			if len(nextResp.Results) == 0 || nextResp.Results[0] == nil {
				break
			}
			refs = append(refs, nextResp.Results[0].References...)
			cp = nextResp.Results[0].ContinuationPoint
		}
	}
	return refs
}
