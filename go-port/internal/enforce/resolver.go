// Is this Drive ID inside one of these folders?
//
// Walks the file's parents upward until it meets a scope folder. The cache
// holds Drive NODES (id → parents, type, trashed), never verdicts: scopes
// differ per agent and change in the dashboard, so the answer is recomputed
// every time from nodes that are at most five minutes old.
//
// Writes pass live, which refetches every level. A folder moved out of scope
// can therefore still be READ for up to the cache TTL, but never written.
// Port of src/enforce/resolver.ts.
package enforce

import (
	"context"
	"sync"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
)

const maxDepth = 32

type Verdict struct {
	OK       bool
	File     *drive.File
	Reason   string // DenyReason when !OK
	Category string // always set for drive_error / credential_rejected
	Detail   string
}

// DriveReasons say Google, not the file, is the problem. They always carry a category.
func isDriveReason(reason string) bool {
	return reason == "drive_error" || reason == "credential_rejected"
}

// isDefiniteNo: a Drive answer that ends a path — gone, or not ours to see.
// A rejection or a Calendar answer is not one.
func isDefiniteNo(err error) bool {
	var de *drive.Error
	if !drive.As(err, &de) {
		return false
	}
	return de.Source == drive.SourceDrive && de.Rejection == "" && !de.Retryable &&
		(de.Status == 403 || de.Status == 404)
}

// rejection: Google refused the credential or the API; retrying cannot help,
// and it is not the file's fault.
func rejectionOf(err error) *Verdict {
	var de *drive.Error
	if drive.As(err, &de) && de.Rejection != "" && de.Source != drive.SourceCalendar {
		return &Verdict{OK: false, Reason: "credential_rejected", Category: drive.CategoryOf(err), Detail: drive.RejectionDetail(de)}
	}
	return nil
}

func driveErrorVerdict(err error) Verdict {
	return Verdict{OK: false, Reason: "drive_error", Category: drive.CategoryOf(err), Detail: err.Error()}
}

type nodeEntry struct {
	file *drive.File
	at   int64
}

type ScopeResolver struct {
	drive func() *drive.Client

	mu         sync.Mutex
	cache      map[string]nodeEntry
	order      []string // insertion order for eviction
	ttlMs      int64
	maxEntries int
	maxFetches int
	now        func() int64
}

func NewScopeResolver(d func() *drive.Client) *ScopeResolver {
	return &ScopeResolver{
		drive:      d,
		cache:      map[string]nodeEntry{},
		ttlMs:      300_000,
		maxEntries: 2000,
		maxFetches: 64,
		now:        func() int64 { return time.Now().UnixMilli() },
	}
}

func (r *ScopeResolver) Flush() {
	r.mu.Lock()
	r.cache = map[string]nodeEntry{}
	r.order = nil
	r.mu.Unlock()
}

// Node returns one node, from the cache unless live. Errors propagate and are
// never cached.
func (r *ScopeResolver) Node(ctx context.Context, id string, live bool) (*drive.File, error) {
	d := r.drive()
	if d == nil {
		return nil, &drive.Error{Status: 0, Retryable: true, Msg: "Drive client not ready", Source: drive.SourceDrive}
	}
	r.mu.Lock()
	if !live {
		if hit, ok := r.cache[id]; ok && r.now()-hit.at < r.ttlMs {
			r.mu.Unlock()
			return hit.file, nil
		}
	}
	r.mu.Unlock()
	file, err := d.GetFile(ctx, id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if _, exists := r.cache[id]; !exists {
		r.order = append(r.order, id)
	}
	r.cache[id] = nodeEntry{file: file, at: r.now()}
	for len(r.order) > r.maxEntries {
		delete(r.cache, r.order[0])
		r.order = r.order[1:]
	}
	r.mu.Unlock()
	return file, nil
}

func (r *ScopeResolver) Check(ctx context.Context, id string, scopeIDs map[string]bool, live bool) Verdict {
	if r.drive() == nil {
		return Verdict{OK: false, Reason: "drive_error", Category: "unavailable", Detail: "not ready"}
	}
	fetches := 0
	fetchNode := func(nodeID string) (*drive.File, error) {
		fetches++
		return r.Node(ctx, nodeID, live)
	}

	target, err := fetchNode(id)
	if err != nil {
		if v := rejectionOf(err); v != nil {
			return *v
		}
		if isDefiniteNo(err) {
			return Verdict{OK: false, Reason: "not_found"}
		}
		return driveErrorVerdict(err)
	}
	if target == nil {
		return Verdict{OK: false, Reason: "not_found"}
	}
	if target.Trashed {
		return Verdict{OK: false, Reason: "trashed"}
	}
	if scopeIDs[target.ID] || scopeIDs[id] {
		return Verdict{OK: true, File: target}
	}
	if len(target.Parents) == 0 {
		return Verdict{OK: false, Reason: "no_parents"}
	}

	tooDeep := false
	sawTrashed := false
	visited := map[string]bool{target.ID: true}
	type frame struct {
		id    string
		depth int
	}
	stack := []frame{}
	for _, p := range target.Parents {
		stack = append(stack, frame{p, 1})
	}

	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[f.id] {
			continue
		}
		visited[f.id] = true
		if f.depth > maxDepth || fetches >= r.maxFetches {
			tooDeep = true
			continue
		}
		node, err := fetchNode(f.id)
		if err != nil {
			// A parent we may not see ends this path; Drive failing to answer
			// ends the whole check, because "try again" is the only honest result.
			if v := rejectionOf(err); v != nil {
				return *v
			}
			if isDefiniteNo(err) {
				continue
			}
			return driveErrorVerdict(err)
		}
		if node == nil {
			continue
		}
		if node.Trashed {
			sawTrashed = true
			continue
		}
		if scopeIDs[node.ID] {
			return Verdict{OK: true, File: target}
		}
		for _, p := range node.Parents {
			stack = append(stack, frame{p, f.depth + 1})
		}
	}

	if tooDeep {
		return Verdict{OK: false, Reason: "too_deep"}
	}
	if sawTrashed {
		return Verdict{OK: false, Reason: "trashed"}
	}
	return Verdict{OK: false, Reason: "outside"}
}
