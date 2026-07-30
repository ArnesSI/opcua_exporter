package browse

import (
	"context"
	"errors"
	"testing"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mwieczorkiewicz/opcua_exporter/internal/config"
)

// fakeClient is a minimal in-memory address space for testing ResolveBrowseNames
// without a real OPC UA server. tree maps a parent NodeID string to the list of
// children references Browse should return for it (possibly split into pages,
// see pages). browseCalls counts how many times Browse included a given parent
// NodeID string, used to assert dedup/cycle-guard behavior. requestSizes records
// the size of req.NodesToBrowse for every Browse call, used to assert chunking.
type fakeClient struct {
	tree         map[string][]*ua.ReferenceDescription
	pages        map[string][][]*ua.ReferenceDescription // remaining continuation pages per parent
	browseCalls  map[string]int
	browseErr    error
	requestSizes []int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		tree:        make(map[string][]*ua.ReferenceDescription),
		pages:       make(map[string][][]*ua.ReferenceDescription),
		browseCalls: make(map[string]int),
	}
}

func (f *fakeClient) Browse(_ context.Context, req *ua.BrowseRequest) (*ua.BrowseResponse, error) {
	f.requestSizes = append(f.requestSizes, len(req.NodesToBrowse))
	if f.browseErr != nil {
		return nil, f.browseErr
	}
	results := make([]*ua.BrowseResult, len(req.NodesToBrowse))
	for i, d := range req.NodesToBrowse {
		key := d.NodeID.String()
		f.browseCalls[key]++
		var cp []byte
		if len(f.pages[key]) > 0 {
			cp = []byte(key)
		}
		results[i] = &ua.BrowseResult{References: f.tree[key], ContinuationPoint: cp}
	}
	return &ua.BrowseResponse{Results: results}, nil
}

func (f *fakeClient) BrowseNext(_ context.Context, req *ua.BrowseNextRequest) (*ua.BrowseNextResponse, error) {
	key := string(req.ContinuationPoints[0])
	pages := f.pages[key]
	if len(pages) == 0 {
		return &ua.BrowseNextResponse{Results: []*ua.BrowseResult{{}}}, nil
	}
	page := pages[0]
	f.pages[key] = pages[1:]
	var nextCP []byte
	if len(f.pages[key]) > 0 {
		nextCP = []byte(key)
	}
	return &ua.BrowseNextResponse{Results: []*ua.BrowseResult{{References: page, ContinuationPoint: nextCP}}}, nil
}

// child builds a ReferenceDescription for a node with the given browse name,
// as if reached via a hierarchical forward reference.
func child(nodeID *ua.NodeID, browseName string) *ua.ReferenceDescription {
	return &ua.ReferenceDescription{
		NodeID:     ua.NewExpandedNodeID(nodeID, "", 0),
		BrowseName: &ua.QualifiedName{NamespaceIndex: nodeID.Namespace(), Name: browseName},
	}
}

func objectsFolder() *ua.NodeID {
	return ua.NewTwoByteNodeID(id.ObjectsFolder)
}

func node(ns uint16, s string) *ua.NodeID {
	n, err := ua.ParseNodeID(nodeIDString(ns, s))
	if err != nil {
		panic(err)
	}
	return n
}

func nodeIDString(ns uint16, s string) string {
	return "ns=" + itoa(ns) + ";s=" + s
}

func itoa(n uint16) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func TestResolveBrowseNames_NoBrowseNameSkipsTraversal(t *testing.T) {
	fc := newFakeClient()
	mappings := []config.NodeMapping{
		{NodeName: "ns=1;s=Foo", MetricName: "foo"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	assert.Equal(t, mappings, result)
	assert.Empty(t, fc.browseCalls, "Browse should never be called when no mapping uses BrowseName")
}

func TestResolveBrowseNames_UniqueMatchUnderDefaultRoot(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()
	target := node(2, "Temperature")
	fc.tree[root.String()] = []*ua.ReferenceDescription{child(target, "Temperature")}

	mappings := []config.NodeMapping{
		{BrowseName: "Temperature", MetricName: "temp_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, target.String(), result[0].NodeName)
}

func TestResolveBrowseNames_UniqueMatchUnderCustomRoot(t *testing.T) {
	fc := newFakeClient()
	root := node(4, "|var|WAGO.Application")
	branch := node(4, "|var|WAGO.Application.PLCV")
	target := node(4, "|var|WAGO.Application.PLCV.Temp")

	fc.tree[root.String()] = []*ua.ReferenceDescription{child(branch, "PLCV")}
	fc.tree[branch.String()] = []*ua.ReferenceDescription{child(target, "Temp")}

	mappings := []config.NodeMapping{
		{BrowseName: "Temp", MetricName: "temp_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, root.String(), 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, target.String(), result[0].NodeName)

	// Objects folder must never be touched when a custom root is given.
	assert.NotContains(t, fc.browseCalls, objectsFolder().String())
}

func TestResolveBrowseNames_ZeroMatchesDropsMappingOnly(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()
	found := node(2, "Found")
	fc.tree[root.String()] = []*ua.ReferenceDescription{child(found, "Found")}

	mappings := []config.NodeMapping{
		{BrowseName: "Found", MetricName: "found_metric"},
		{BrowseName: "Missing", MetricName: "missing_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "found_metric", result[0].MetricName)
	assert.Equal(t, found.String(), result[0].NodeName)
}

func TestResolveBrowseNames_AmbiguousMatchIsFatal(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()
	branchA := node(2, "PLCV")
	branchB := node(2, "PLCV2")
	matchA := node(2, "PLCV.Temp")
	matchB := node(2, "PLCV2.Temp")

	fc.tree[root.String()] = []*ua.ReferenceDescription{child(branchA, "PLCV"), child(branchB, "PLCV2")}
	fc.tree[branchA.String()] = []*ua.ReferenceDescription{child(matchA, "Temp")}
	fc.tree[branchB.String()] = []*ua.ReferenceDescription{child(matchB, "Temp")}

	mappings := []config.NodeMapping{
		{BrowseName: "Temp", MetricName: "temp_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "Temp")
}

func TestResolveBrowseNames_TwoMappingsSharingOneBrowseName(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()
	target := node(2, "Shared")
	fc.tree[root.String()] = []*ua.ReferenceDescription{child(target, "Shared")}

	mappings := []config.NodeMapping{
		{BrowseName: "Shared", MetricName: "metric_a"},
		{BrowseName: "Shared", MetricName: "metric_b"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, target.String(), result[0].NodeName)
	assert.Equal(t, target.String(), result[1].NodeName)
}

func TestResolveBrowseNames_MixedNodeNameAndBrowseNamePreservesOrder(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()
	target := node(2, "Resolved")
	fc.tree[root.String()] = []*ua.ReferenceDescription{child(target, "Resolved")}

	mappings := []config.NodeMapping{
		{NodeName: "ns=1;s=AlreadySet", MetricName: "already_set"},
		{BrowseName: "Resolved", MetricName: "resolved_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, "ns=1;s=AlreadySet", result[0].NodeName)
	assert.Equal(t, target.String(), result[1].NodeName)
}

func TestResolveBrowseNames_CycleIsDeduped(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()
	shared := node(2, "Shared")
	parentA := node(2, "ParentA")
	parentB := node(2, "ParentB")

	// shared is referenced from two different parents at the same depth.
	fc.tree[root.String()] = []*ua.ReferenceDescription{child(parentA, "ParentA"), child(parentB, "ParentB")}
	fc.tree[parentA.String()] = []*ua.ReferenceDescription{child(shared, "Shared")}
	fc.tree[parentB.String()] = []*ua.ReferenceDescription{child(shared, "Shared")}

	mappings := []config.NodeMapping{
		{BrowseName: "Shared", MetricName: "shared_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, shared.String(), result[0].NodeName)
	// shared should only ever have been browsed once, from whichever parent visited it first.
	assert.LessOrEqual(t, fc.browseCalls[shared.String()], 1)
}

func TestResolveBrowseNames_DepthBoundary(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()

	// Build a chain of exactly maxBrowseDepth hops so the target sits right at the boundary.
	parent := root
	var target *ua.NodeID
	for depth := 0; depth < maxBrowseDepth; depth++ {
		child_ := node(2, "n"+itoa(uint16(depth)))
		name := "n" + itoa(uint16(depth))
		fc.tree[parent.String()] = []*ua.ReferenceDescription{child(child_, name)}
		parent = child_
		target = child_
	}

	mappings := []config.NodeMapping{
		{BrowseName: "n" + itoa(uint16(maxBrowseDepth-1)), MetricName: "boundary_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, target.String(), result[0].NodeName)
}

func TestResolveBrowseNames_PastDepthBoundaryNotFound(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()

	// One hop deeper than the boundary test: target is unreachable within maxBrowseDepth.
	parent := root
	for depth := 0; depth < maxBrowseDepth+1; depth++ {
		child_ := node(2, "m"+itoa(uint16(depth)))
		name := "m" + itoa(uint16(depth))
		fc.tree[parent.String()] = []*ua.ReferenceDescription{child(child_, name)}
		parent = child_
	}

	mappings := []config.NodeMapping{
		{BrowseName: "m" + itoa(uint16(maxBrowseDepth)), MetricName: "too_deep_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestResolveBrowseNames_Pagination(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()
	firstPageChild := node(2, "First")
	secondPageChild := node(2, "OnSecondPage")

	rootKey := root.String()
	fc.tree[rootKey] = []*ua.ReferenceDescription{child(firstPageChild, "First")}
	fc.pages[rootKey] = [][]*ua.ReferenceDescription{
		{child(secondPageChild, "OnSecondPage")},
	}

	mappings := []config.NodeMapping{
		{BrowseName: "OnSecondPage", MetricName: "second_page_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, secondPageChild.String(), result[0].NodeName)
}

func TestResolveBrowseNames_WideLevelIsChunked(t *testing.T) {
	const chunkSize = 500

	fc := newFakeClient()
	root := objectsFolder()

	// Build a level wider than chunkSize, mimicking a server that rejects an
	// oversized Browse request (StatusBadTooManyOperations) if everything
	// were sent in a single call.
	width := chunkSize*2 + 37
	var siblings []*ua.ReferenceDescription
	var target *ua.NodeID
	for i := 0; i < width; i++ {
		name := "sibling" + itoa(uint16(i))
		n := node(2, name)
		siblings = append(siblings, child(n, name))
		if i == width-1 {
			target = n
		}
	}
	fc.tree[root.String()] = siblings

	mappings := []config.NodeMapping{
		{BrowseName: "sibling" + itoa(uint16(width-1)), MetricName: "last_sibling_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", chunkSize, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, target.String(), result[0].NodeName)

	// fc.requestSizes[0] is the depth-0 call browsing the root itself (1 node);
	// the remaining calls are the chunked depth-1 browse of the wide sibling set.
	require.Len(t, fc.requestSizes, 4, "expected the wide level to be split into 3 chunked Browse calls plus the initial root browse")
	assert.Equal(t, []int{1, chunkSize, chunkSize, 37}, fc.requestSizes)
}

func TestResolveBrowseNames_NoChunkingWhenLimitUnset(t *testing.T) {
	fc := newFakeClient()
	root := objectsFolder()

	width := 1200
	var siblings []*ua.ReferenceDescription
	for i := 0; i < width; i++ {
		name := "sibling" + itoa(uint16(i))
		siblings = append(siblings, child(node(2, name), name))
	}
	fc.tree[root.String()] = siblings

	mappings := []config.NodeMapping{
		{BrowseName: "sibling0", MetricName: "sibling0_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	require.Len(t, result, 1)

	// maxItemsPerRequest <= 0 means no limit: the whole wide level goes out
	// as a single Browse request, same as the root-browsing call before it.
	assert.Equal(t, []int{1, width}, fc.requestSizes)
}

func TestResolveBrowseNames_BrowseErrorIsNotFatal(t *testing.T) {
	fc := newFakeClient()
	fc.browseErr = errors.New("simulated RPC failure")

	mappings := []config.NodeMapping{
		{BrowseName: "Anything", MetricName: "anything_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "", 0, false, mappings)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestResolveBrowseNames_InvalidRootIsFatal(t *testing.T) {
	fc := newFakeClient()
	mappings := []config.NodeMapping{
		{BrowseName: "Anything", MetricName: "anything_metric"},
	}

	result, err := ResolveBrowseNames(context.Background(), fc, "ns=not-a-number;s=Foo", 0, false, mappings)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Empty(t, fc.browseCalls)
}
