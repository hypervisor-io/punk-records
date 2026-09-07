package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// SummaryStageVersion stamps every produced summary subtree so a stale or
// foreign summary is never silently presented as current: compaction and
// retrieval both check it.
const SummaryStageVersion = "summary_tree/v1"

// DefaultSummaryFanout bounds how many children a single summary node may
// carry; a level exceeding the fanout is split into a parent level.
const DefaultSummaryFanout = 16

// SummaryTreeOptions tune one BuildSummaryTree run. Fanout <= 0 uses
// DefaultSummaryFanout.
type SummaryTreeOptions struct {
	Fanout int
}

// SummaryTreeResult reports one BuildSummaryTree run. ModelCalls and
// InputTokens surface the incremental model work so a measurement harness
// (E02-style) can price the feature without re-instrumenting the
// Summarizer.
type SummaryTreeResult struct {
	Groups      int `json:"groups"`       // groups with live leaves
	Leaves      int `json:"leaves"`       // live leaves summarized (uncapped)
	Summaries   int `json:"summaries"`    // summary facts present after the run
	Built       int `json:"built"`        // summaries (re)built this run
	Fresh       int `json:"fresh"`        // found fresh: no model work
	ModelCalls  int `json:"model_calls"`  // Summarize invocations this run
	InputTokens int `json:"input_tokens"` // estimated tokens of input to Summarize
	Children    int `json:"children"`     // child records across all summaries
}

// liveAllFacts enumerates every live fact under prefix with no 1000-row
// cap: ListKeys is uncapped and liveByKeys is paged in bounded batches.
func (s *Store) liveAllFacts(ctx context.Context, ns, prefix string) ([]Fact, error) {
	keys, err := s.ListKeys(ctx, ns, prefix)
	if err != nil {
		return nil, err
	}
	out := []Fact{}
	for i := 0; i < len(keys); i += 500 {
		facts, err := s.liveByKeys(ctx, ns, keys[i:min(i+500, len(keys))])
		if err != nil {
			return nil, err
		}
		out = append(out, facts...)
	}
	return out, nil
}

// isSummaryLeaf reports whether a live fact is an eligible summary leaf:
// a SOURCE-LINKED observation under /observations/* whose recorded
// source_ids are non-empty. The hierarchy sits above source-linked
// observations only. Raw facts, operational/task facts, generated
// synthesis tiers (summaries, rollups, mental-models, entities) and
// unlinked/unsupported observations are EXCLUDED from summary coverage:
// a summary is never generated from raw or operational evidence, and an
// observation with no grounding is never presented as current evidence.
func isSummaryLeaf(f Fact) bool {
	return strings.HasPrefix(f.Key, "/observations/") && len(sourceIDList(f)) > 0
}

// sourceIDList extracts an observation's recorded source fact IDs.
func sourceIDList(f Fact) []string {
	value := f.Attributes["source_ids"]
	if summary, ok := f.Attributes["summary"].(map[string]any); ok {
		value = summary["source_ids"]
	}
	if ids, ok := value.([]string); ok {
		return append([]string(nil), ids...)
	}
	raw, _ := value.([]any)
	ids := make([]string, 0, len(raw))
	for _, v := range raw {
		if id, ok := v.(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// factIDCurrent reports whether a given revision ID is the current live
// revision of its key: not superseded, not tombstoned, not expired, and the
// key's latest live revision. A missing/swept/relocated revision is not
// current.
func (s *Store) factIDCurrent(ctx context.Context, ns, id string) (bool, error) {
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return false, err
	}
	var cur bool
	err = s.db.QueryRowContext(ctx, s.db.Rebind(`
		SELECT EXISTS(
			SELECT 1 FROM memories m
			WHERE m.namespace_id = $1 AND m.id = $2
			  AND m.invalid_at IS NULL AND m.action <> 'tombstone'
			  AND (m.expiration_date IS NULL OR m.expiration_date > $3)
			  AND m.id = (
			      SELECT h.id FROM memories h
			      WHERE h.namespace_id = m.namespace_id AND h.key = m.key
			        AND h.invalid_at IS NULL AND h.action <> 'tombstone'
			        AND (h.expiration_date IS NULL OR h.expiration_date > $3)
			      ORDER BY h.created_at DESC, h.id DESC LIMIT 1)
		)`), nsID, id, store.TimeToDB(s.now())).Scan(&cur)
	if err != nil {
		return false, err
	}
	return cur, nil
}

// observationGrounded reports whether a source-linked observation's recorded
// source revisions are all still current live revisions. An observation with
// no recorded source_ids is ungrounded (unsupported synthesis) and therefore
// not current evidence for the summary above it.
func (s *Store) observationGrounded(ctx context.Context, ns string, obs Fact) (bool, error) {
	ids := sourceIDList(obs)
	if len(ids) == 0 {
		return false, nil
	}
	for _, id := range ids {
		cur, err := s.factIDCurrent(ctx, ns, id)
		if err != nil {
			return false, err
		}
		if !cur {
			return false, nil
		}
	}
	return true, nil
}

// keysByIDs resolves fact IDs to their keys so a flat-slug observation can
// be grouped by the domain of the facts it is grounded in.
func (s *Store) keysByIDs(ctx context.Context, ns string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return nil, err
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids)+1)
	args[0] = nsID
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args[i+1] = id
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT m.key FROM memories m
		WHERE m.namespace_id = $1 AND m.id IN (`+strings.Join(ph, ",")+`)`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// firstKeySegment returns the first path segment of a /hier/key.
func firstKeySegment(key string) string {
	seg := strings.TrimPrefix(key, "/")
	if idx := strings.IndexByte(seg, '/'); idx >= 0 {
		return seg[:idx]
	}
	return seg
}

// mostCommonSourceSegment picks the most common first key segment across
// the observation's source facts, with a lexicographic tie-break so the
// grouping stays deterministic. Empty result means "misc".
func (s *Store) mostCommonSourceSegment(ctx context.Context, ns string, ids []string) (string, error) {
	keys, err := s.keysByIDs(ctx, ns, ids)
	if err != nil {
		return "", err
	}
	counts := map[string]int{}
	for _, k := range keys {
		counts[firstKeySegment(k)]++
	}
	best, bestCount := "", 0
	for seg, c := range counts {
		if c > bestCount || (c == bestCount && seg < best) {
			best, bestCount = seg, c
		}
	}
	return best, nil
}

// summarizeGroup deterministically assigns a live leaf to its summary
// group (domain). The rule adopted at prep time:
//
//   - a hierarchical observation slug groups by its first slug path
//     segment (/observations/db/topology -> "db");
//   - a flat slug groups by the most common first segment of its
//     source_ids' fact-key prefixes, else "misc";
//   - any other leaf groups by the first key path segment.
func (s *Store) summarizeGroup(ctx context.Context, ns string, f Fact) (string, error) {
	if slug, ok := strings.CutPrefix(f.Key, "/observations/"); ok {
		if idx := strings.IndexByte(slug, '/'); idx >= 0 {
			return slug[:idx], nil
		}
		seg, err := s.mostCommonSourceSegment(ctx, ns, sourceIDList(f))
		if err != nil {
			return "", err
		}
		if seg != "" {
			return seg, nil
		}
		return "misc", nil
	}
	return firstKeySegment(f.Key), nil
}

// summaryGroups buckets live non-generated facts into their summary
// groups; each group's leaf slice is key-sorted.
func (s *Store) summaryGroups(ctx context.Context, ns string) (map[string][]Fact, error) {
	leaves, err := s.liveAllFacts(ctx, ns, "/")
	if err != nil {
		return nil, err
	}
	groups := map[string][]Fact{}
	for _, f := range leaves {
		if !isSummaryLeaf(f) {
			continue
		}
		g, err := s.summarizeGroup(ctx, ns, f)
		if err != nil {
			return nil, err
		}
		groups[g] = append(groups[g], f)
	}
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].Key < g[j].Key })
	}
	return groups, nil
}

// The depth guard bounds work even when a pathological set never splits.
// Hash quality improves partitioning; it does not prove termination.
const maxSummaryDepth = 64

// bucketIndex mixes every key bit into the selected bucket. FNV's low
// bits cannot separate keys with equal low character bits at power-of-two
// fanouts, regardless of a common prefix or suffix salt.
func bucketIndex(key string, n int, level int) int {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", level, key)))
	return int(binary.BigEndian.Uint64(h[:8]) % uint64(n))
}

// summaryNodeKey is the deterministic key of a summary node at path depth
// under /summaries/<group>. The root uses just the group; deeper nodes add
// their bucket path so a node's key is stable as long as its subtree lives.
func summaryNodeKey(group string, path []int) string {
	k := "/summaries/" + group
	for _, b := range path {
		k += fmt.Sprintf("/%d", b)
	}
	return k
}

// summaryNode is one desired summary node in a group's tree.
type summaryNode struct {
	key       string
	path      []int
	level     int
	childKeys []string // deterministic, key-sorted
	leafItems bool     // childKeys are live leaves, not sub-summaries
}

// desiredSummaryNodes returns the full desired node set for a group,
// appended children-first so a parent can read the freshly rebuilt child
// summaries. A partition failure returns no partial tree.
func desiredSummaryNodes(group string, leaves []Fact, fanout int) ([]*summaryNode, error) {
	if fanout < 2 {
		return nil, fmt.Errorf("summary fanout must be at least 2, got %d", fanout)
	}
	if len(leaves) == 0 {
		return nil, nil
	}
	var nodes []*summaryNode
	var build func(path []int, level int, items []Fact) (*summaryNode, error)
	build = func(path []int, level int, items []Fact) (*summaryNode, error) {
		if level >= maxSummaryDepth {
			return nil, fmt.Errorf("summary group %q exceeds partition depth %d", group, maxSummaryDepth)
		}
		node := &summaryNode{key: summaryNodeKey(group, path), path: path, level: level}
		if len(items) <= fanout {
			node.leafItems = true
			keys := make([]string, 0, len(items))
			for _, it := range items {
				keys = append(keys, it.Key)
			}
			sort.Strings(keys)
			node.childKeys = keys
			nodes = append(nodes, node)
			return node, nil
		}
		buckets := make([][]Fact, fanout)
		for _, it := range items {
			b := bucketIndex(it.Key, fanout, level)
			buckets[b] = append(buckets[b], it)
		}
		childKeys := []string{}
		for b := 0; b < fanout; b++ {
			if len(buckets[b]) == 0 {
				continue
			}
			child, err := build(append(append([]int{}, path...), b), level+1, buckets[b])
			if err != nil {
				return nil, err
			}
			childKeys = append(childKeys, child.key)
		}
		sort.Strings(childKeys)
		node.childKeys = childKeys
		node.leafItems = false
		nodes = append(nodes, node)
		return node, nil
	}
	if _, err := build(nil, 0, leaves); err != nil {
		return nil, err
	}
	return nodes, nil
}

// isParentSummary reports whether a summary fact's recorded children are
// themselves generated summaries (a split/parent node) rather than leaves.
func isParentSummary(f Fact) bool {
	rec, _, ok := summaryAttrs(f)
	if !ok {
		return false
	}
	for _, c := range rec {
		if strings.HasPrefix(c.Key, "/summaries/") {
			return true
		}
	}
	return false
}

// existingGroupSummaries loads the live summary facts under a group's exact
// subtree (root and root+'/' descendants; a prefix lookup would also match
// a neighbouring group name like db2).
func (s *Store) existingGroupSummaries(ctx context.Context, ns, group string) (map[string]Fact, error) {
	root := "/summaries/" + group
	keys, err := s.ListKeys(ctx, ns, root)
	if err != nil {
		return nil, err
	}
	want := []string{}
	for _, k := range keys {
		if k == root || strings.HasPrefix(k, root+"/") {
			want = append(want, k)
		}
	}
	facts, err := s.liveByKeys(ctx, ns, want)
	if err != nil {
		return nil, err
	}
	out := map[string]Fact{}
	for _, f := range facts {
		out[f.Key] = f
	}
	return out, nil
}

// establishedSplit reports whether a group already has a generated child
// summary (a split topology), so a later shrink keeps it rather than
// collapsing the tree and retiring a non-empty sibling.
func establishedSplit(group string, existing map[string]Fact) bool {
	root, ok := existing["/summaries/"+group]
	return ok && isParentSummary(root)
}

// reconcileGroup produces a group's target node set. On a first build or
// when the group has no established split, it is the minimal
// desiredSummaryNodes tree (fresh growth). When the group already splits
// (an established parent), it RETAINS that topology: every non-empty
// sub-bucket stays a generated summary and only empty subtrees are retired,
// so a shrink across the fanout threshold cannot retire an untouched
// non-empty sibling. The shape is stable because the split decision
// inherits the established structure, while growth (a bucket exceeding
// fanout) still recurses deeper.
func (s *Store) reconcileGroup(ctx context.Context, ns, group string, leaves []Fact, fanout int, existing map[string]Fact) ([]*summaryNode, error) {
	if len(leaves) == 0 {
		return nil, nil
	}
	if !establishedSplit(group, existing) {
		return desiredSummaryNodes(group, leaves, fanout)
	}
	var nodes []*summaryNode
	var build func(path []int, level int, owned []Fact) (*summaryNode, error)
	build = func(path []int, level int, owned []Fact) (*summaryNode, error) {
		if level >= maxSummaryDepth {
			return nil, fmt.Errorf("summary group %q exceeds partition depth %d", group, maxSummaryDepth)
		}
		nodeKey := summaryNodeKey(group, path)
		cur, has := existing[nodeKey]
		curIsParent := has && isParentSummary(cur)
		node := &summaryNode{key: nodeKey, path: path, level: level}
		// A node stays a parent when it must split (owned > fanout) or when
		// the established topology already made it a parent and it is not
		// empty: preserve, never collapse non-empty structure.
		if len(owned) <= fanout && (!curIsParent || len(owned) == 0) {
			node.leafItems = true
			keys := make([]string, len(owned))
			for i, f := range owned {
				keys[i] = f.Key
			}
			sort.Strings(keys)
			node.childKeys = keys
			nodes = append(nodes, node)
			return node, nil
		}
		sub := make([][]Fact, fanout)
		for _, f := range owned {
			b := bucketIndex(f.Key, fanout, level)
			sub[b] = append(sub[b], f)
		}
		childKeys := []string{}
		for b := 0; b < fanout; b++ {
			if len(sub[b]) == 0 {
				continue
			}
			child, err := build(append(append([]int{}, path...), b), level+1, sub[b])
			if err != nil {
				return nil, err
			}
			childKeys = append(childKeys, child.key)
		}
		sort.Strings(childKeys)
		node.childKeys = childKeys
		node.leafItems = false
		nodes = append(nodes, node)
		return node, nil
	}
	if _, err := build(nil, 0, leaves); err != nil {
		return nil, err
	}
	return nodes, nil
}

// summaryChildRef is one recorded child (key + live revision at build time).
type summaryChildRef struct {
	Key string `json:"key"`
	Rev string `json:"rev"`
}

// summaryAttrs parses a summary fact's attribute block.
func summaryAttrs(f Fact) (children []summaryChildRef, stageVersion string, ok bool) {
	sm, ok := f.Attributes["summary"].(map[string]any)
	if !ok {
		return nil, "", false
	}
	stageVersion, _ = sm["stage_version"].(string)
	raw, _ := sm["children"].([]any)
	children = make([]summaryChildRef, 0, len(raw))
	for _, c := range raw {
		cm, _ := c.(map[string]any)
		key, _ := cm["key"].(string)
		rev, _ := cm["rev"].(string)
		if key != "" {
			children = append(children, summaryChildRef{Key: key, Rev: rev})
		}
	}
	return children, stageVersion, true
}

// summaryAttrBlock builds the attributes stored on a summary fact:
// children (key, rev) pairs, stage version, level, group, fanout, build
// time, and the union of source_ids so sources stay traceable. Fanout is
// recorded so a later SummaryStale can recompute the node's expected child
// membership without guessing the split width.
func summaryAttrBlock(node *summaryNode, group string, fanout int, builtAt time.Time, childRevs []summaryChildRef, sourceIDs []string) map[string]any {
	rawChildren := make([]any, len(childRevs))
	for i, c := range childRevs {
		rawChildren[i] = map[string]any{"key": c.Key, "rev": c.Rev}
	}
	ids := make([]any, len(sourceIDs))
	for i, id := range sourceIDs {
		ids[i] = id
	}
	return map[string]any{
		"summary": map[string]any{
			"children":      rawChildren,
			"stage_version": SummaryStageVersion,
			"level":         float64(node.level),
			"group":         group,
			"fanout":        float64(fanout),
			"built_at":      store.TimeToDB(builtAt),
			"source_ids":    ids,
		},
	}
}

// leafSourceIDs unions the source_ids of a set of leaf facts.
func leafSourceIDs(facts []Fact) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range facts {
		for _, id := range sourceIDList(f) {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// BuildSummaryTree builds bounded topic summaries above source-linked
// leaves, regenerating only stale ancestors. Deterministic-first contract
// (same as ConsolidateObservations): a nil Summarizer is a no-op, so the
// feature stays off by default until wired to a real Summarizer.
func (s *Store) BuildSummaryTree(ctx context.Context, ns string, sum Summarizer, opts SummaryTreeOptions) (*SummaryTreeResult, error) {
	res := &SummaryTreeResult{}
	if sum == nil {
		return res, nil
	}
	fanout := opts.Fanout
	if fanout <= 0 {
		fanout = DefaultSummaryFanout
	}
	if fanout < 2 {
		return res, fmt.Errorf("summary fanout must be at least 2, got %d", fanout)
	}

	groups, err := s.summaryGroups(ctx, ns)
	if err != nil {
		return res, err
	}
	res.Groups = len(groups)
	desired := map[string]bool{}

	for group, leaves := range groups {
		res.Leaves += len(leaves)
		existing, err := s.existingGroupSummaries(ctx, ns, group)
		if err != nil {
			return res, err
		}
		nodes, err := s.reconcileGroup(ctx, ns, group, leaves, fanout, existing)
		if err != nil {
			return res, err
		}
		leafByKey := map[string]Fact{}
		for _, lf := range leaves {
			leafByKey[lf.Key] = lf
		}
		for _, node := range nodes {
			desired[node.key] = true
			res.Children += len(node.childKeys)
			res.Summaries++
			if err := s.rebuildSummaryNode(ctx, ns, sum, node, group, fanout, leafByKey, res); err != nil {
				return res, err
			}
		}
	}

	if err := s.pruneOrphanSummaries(ctx, ns, desired); err != nil {
		return res, err
	}
	return res, nil
}

// rebuildSummaryNode checks a node's recorded children against the current
// live revisions of those children; fresh nodes do zero model work, stale
// nodes are re-summarized and written.
func (s *Store) rebuildSummaryNode(ctx context.Context, ns string, sum Summarizer, node *summaryNode, group string, fanout int, leafByKey map[string]Fact, res *SummaryTreeResult) error {
	var input []Fact
	if node.leafItems {
		input = make([]Fact, 0, len(node.childKeys))
		for _, k := range node.childKeys {
			if f, ok := leafByKey[k]; ok {
				input = append(input, f)
			}
		}
	} else {
		facts, err := s.liveByKeys(ctx, ns, node.childKeys)
		if err != nil {
			return err
		}
		input = facts
	}
	if len(input) == 0 {
		return fmt.Errorf("summarize %s: no input facts", node.key)
	}

	// freshness: recorded (key,rev) pairs match current live revisions,
	// membership unchanged, stage version matches.
	existing, err := s.Recall(ctx, ns, node.key, 1)
	if err != nil {
		return err
	}
	if len(existing) == 1 {
		if rec, stage, ok := summaryAttrs(existing[0]); ok && stage == SummaryStageVersion && childRefsMatch(rec, node.childKeys, input) {
			res.Fresh++
			return nil
		}
	}

	// Attempted work remains visible if the model or subsequent write
	// fails. These input tokens are estimates, not provider usage.
	res.ModelCalls++
	for _, f := range input {
		res.InputTokens += EstimateTokens(f.Body)
	}
	text, err := sum.Summarize(ctx, input)
	if err != nil {
		return fmt.Errorf("summarize %s: %w", node.key, err)
	}
	revs := make([]summaryChildRef, 0, len(input))
	for _, f := range input {
		revs = append(revs, summaryChildRef{Key: f.Key, Rev: f.ID})
	}
	var srcIDs []string
	if node.leafItems {
		srcIDs = leafSourceIDs(input)
	} else {
		seen := map[string]bool{}
		for _, f := range input {
			for _, id := range sourceIDList(f) {
				if !seen[id] {
					seen[id] = true
					srcIDs = append(srcIDs, id)
				}
			}
		}
	}
	if _, err := s.Write(ctx, WriteInput{
		Namespace: ns, Key: node.key, Body: text,
		Attributes: summaryAttrBlock(node, group, fanout, s.now(), revs, srcIDs),
		Writer:     "summaries", Importance: 0.5,
	}); err != nil {
		return err
	}
	res.Built++
	return nil
}

// childRefsMatch reports whether recorded child refs exactly match the
// current child keys and their live revision IDs: the freshness predicate.
func childRefsMatch(rec []summaryChildRef, childKeys []string, current []Fact) bool {
	if len(rec) != len(childKeys) {
		return false
	}
	curByKey := map[string]string{}
	for _, f := range current {
		curByKey[f.Key] = f.ID
	}
	seen := map[string]bool{}
	for _, c := range rec {
		if c.Rev == "" {
			return false
		}
		if curByKey[c.Key] != c.Rev {
			return false
		}
		seen[c.Key] = true
	}
	for _, k := range childKeys {
		if !seen[k] {
			return false
		}
	}
	return true
}

// pruneOrphanSummaries forgets summary keys that are no longer part of the
// desired tree, so an emptied subtree (and its whole prefix) disappears.
func (s *Store) pruneOrphanSummaries(ctx context.Context, ns string, desired map[string]bool) error {
	keys, err := s.ListKeys(ctx, ns, "/summaries")
	if err != nil {
		return err
	}
	for _, k := range keys {
		if desired[k] {
			continue
		}
		if err := s.Forget(ctx, ns, k, "summaries"); err != nil {
			if strings.Contains(err.Error(), ErrNotFound.Error()) {
				continue // already gone: prune is idempotent, not an error
			}
			return err
		}
	}
	return nil
}

// summaryGroupAndPath derives a summary node's group and bucket path from
// its key: /summaries/<group>[/<b>[/<b>...]].
func summaryGroupAndPath(key string) (group string, path []int) {
	rest := strings.TrimPrefix(key, "/summaries/")
	segs := strings.Split(rest, "/")
	group = segs[0]
	for _, s := range segs[1:] {
		if n, err := strconv.Atoi(s); err == nil {
			path = append(path, n)
		}
	}
	return group, path
}

// intFromAny reads a JSON-decoded numeric attribute, defaulting when absent.
func intFromAny(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return def
	}
}

// leavesMatchingPath returns the live leaves whose recursive bucket
// assignment equals path: for level i, bucketIndex(key, fanout, i) ==
// path[i]. The root (empty path) owns every leaf in the group.
func leavesMatchingPath(leaves []Fact, path []int, fanout int) []Fact {
	var out []Fact
	for _, f := range leaves {
		match := true
		for lvl, b := range path {
			if bucketIndex(f.Key, fanout, lvl) != b {
				match = false
				break
			}
		}
		if match {
			out = append(out, f)
		}
	}
	return out
}

// expectedSummaryChildKeys returns the child keys a node at (group, path)
// SHOULD have under the current live leaves and fanout: its leaf keys when
// it is a terminal level, otherwise the sub-bucket summary keys. An empty
// result means the node's subtree should no longer exist.
// expectedSummaryChildKeys returns the child keys a node at (group, path)
// SHOULD have under the current live leaves, fanout and retained topology.
// selfIsParent reflects the node's established shape (recorded children are
// themselves generated summaries): a retained parent keeps splitting at its
// established level even when its leaf count falls to or below fanout.
// A node must split when owned > fanout or when it is a non-empty retained
// parent. An empty result means the node's subtree is gone.
func expectedSummaryChildKeys(group string, path []int, leaves []Fact, fanout int, selfIsParent bool) ([]string, error) {
	if fanout < 2 {
		return nil, fmt.Errorf("summary fanout must be at least 2, got %d", fanout)
	}
	owned := leavesMatchingPath(leaves, path, fanout)
	if len(owned) == 0 {
		return nil, nil
	}
	if len(owned) <= fanout && (!selfIsParent || len(owned) == 0) {
		keys := make([]string, len(owned))
		for i, f := range owned {
			keys[i] = f.Key
		}
		sort.Strings(keys)
		return keys, nil
	}
	sub := make([][]Fact, fanout)
	for _, f := range owned {
		b := bucketIndex(f.Key, fanout, len(path))
		sub[b] = append(sub[b], f)
	}
	keys := []string{}
	for b := 0; b < fanout; b++ {
		if len(sub[b]) > 0 {
			keys = append(keys, summaryNodeKey(group, append(append([]int{}, path...), b)))
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// SummaryStale reports whether a summary fact no longer reflects its
// sources. It is a stale trigger, NEVER an error for missing,
// superseded or retention-swept revisions: retention can delete revisions,
// and SweepRetention never deletes the latest live revision, so a fresh
// summary cannot be falsely swept.
//
// A summary is stale when its recorded child (key, rev) pairs no longer
// match the current live revisions, when membership changed (a leaf added,
// removed or re-bucketed), when its stage version differs from this build,
// when a generated descendant summary is stale (so a root is not presented
// as current merely because its immediate sub-summary row has not been
// regenerated yet), or when a source-linked observation leaf's recorded
// source revisions are no longer current (deleted, expired, missing,
// superseded or ungrounded — validated directly rather than inferred from
// a "newer live raw fact" heuristic, so a tombstoned source cannot slip
// through). Recursion is bounded by maxSummaryDepth so a cycle degrades
// to stale rather than looping.
func (s *Store) SummaryStale(ctx context.Context, ns string, summary Fact) (bool, error) {
	rec, stage, ok := summaryAttrs(summary)
	if !ok || stage != SummaryStageVersion {
		return true, nil
	}
	if len(rec) == 0 {
		return true, nil
	}
	// Fast path: if any recorded child's live revision no longer matches, the
	// summary is stale WITHOUT the full uncapped group scan. This is the common
	// decoration case (an edited/superseded/swept child) and avoids a group
	// membership recomputation for every recalled summary.
	keys := make([]string, len(rec))
	revs := map[string]string{}
	for i, c := range rec {
		keys[i] = c.Key
		revs[c.Key] = c.Rev
	}
	current, err := s.liveByKeys(ctx, ns, keys)
	if err != nil {
		return false, err
	}
	for _, f := range current {
		if f.ID != revs[f.Key] {
			return true, nil // edited / superseded / retention-swept
		}
	}
	// All direct children are currently live: check membership, descendants
	// and source validity (the full recursive check).
	group, path := summaryGroupAndPath(summary.Key)
	leaves, err := s.groupLeaves(ctx, ns, group)
	if err != nil {
		return false, err
	}
	return s.summaryStaleRec(ctx, ns, summary, group, path, leaves, 0)
}

// groupLeaves returns the current live leaves that fall in group (uncapped
// scan), reused by staleness recomputation and rebuild path.
func (s *Store) groupLeaves(ctx context.Context, ns, group string) ([]Fact, error) {
	groups, err := s.summaryGroups(ctx, ns)
	if err != nil {
		return nil, err
	}
	return groups[group], nil
}

func (s *Store) summaryStaleRec(ctx context.Context, ns string, summary Fact, group string, path []int, leaves []Fact, depth int) (bool, error) {
	if depth > maxSummaryDepth {
		return true, nil // cycle/bound: degrade to stale, never loop
	}
	rec, stage, ok := summaryAttrs(summary)
	if !ok || stage != SummaryStageVersion {
		return true, nil
	}
	if len(rec) == 0 {
		return true, nil
	}
	sm, _ := summary.Attributes["summary"].(map[string]any)
	fanout := intFromAny(sm["fanout"], DefaultSummaryFanout)
	if fanout < 2 {
		return true, nil // cannot recompute membership: conservative
	}
	expected, err := expectedSummaryChildKeys(group, path, leaves, fanout, isParentSummary(summary))
	if err != nil {
		return false, err
	}
	if len(expected) == 0 {
		return true, nil // subtree should be gone; an empty summary is stale
	}
	recByKey := map[string]string{}
	for _, c := range rec {
		recByKey[c.Key] = c.Rev
	}
	if len(recByKey) != len(expected) {
		return true, nil // membership changed (a leaf added/removed)
	}
	seen := map[string]bool{}
	for _, k := range expected {
		if _, ok := recByKey[k]; !ok {
			return true, nil
		}
		seen[k] = true
	}
	current, err := s.liveByKeys(ctx, ns, expected)
	if err != nil {
		return false, err
	}
	curByKey := map[string]Fact{}
	for _, f := range current {
		curByKey[f.Key] = f
	}
	for _, key := range expected {
		cur, ok := curByKey[key]
		if !ok {
			return true, nil // a child vanished
		}
		if cur.ID != recByKey[key] {
			return true, nil // edited / superseded / retention-swept
		}
		if hasPrefix(key, "/summaries/") {
			childGroup, childPath := summaryGroupAndPath(key)
			staleChild, err := s.summaryStaleRec(ctx, ns, cur, childGroup, childPath, leaves, depth+1)
			if err != nil {
				return false, err
			}
			if staleChild {
				return true, nil
			}
		} else if hasPrefix(key, "/observations/") {
			// A source-linked observation's grounding is the recorded
			// source_ids. Validate each recorded source revision is still a
			// CURRENT live revision: a deleted, expired, missing or
			// superseded source (or an unlinked observation with no
			// grounding) is conservatively stale. This catches an
			// underlying raw-source change that the generic
			// "newer live raw fact" heuristic cannot (it ignores
			// tombstones).
			grounded, err := s.observationGrounded(ctx, ns, cur)
			if err != nil {
				return false, err
			}
			if !grounded {
				return true, nil
			}
		}
	}
	return false, nil
}

// ListSummaries enumerates every live summary fact with no 1000-row cap.
func (s *Store) ListSummaries(ctx context.Context, ns string) ([]Fact, error) {
	keys, err := s.ListKeys(ctx, ns, "/summaries")
	if err != nil {
		return nil, err
	}
	out := []Fact{}
	for i := 0; i < len(keys); i += 500 {
		facts, err := s.liveByKeys(ctx, ns, keys[i:min(i+500, len(keys))])
		if err != nil {
			return nil, err
		}
		out = append(out, facts...)
	}
	return out, nil
}
