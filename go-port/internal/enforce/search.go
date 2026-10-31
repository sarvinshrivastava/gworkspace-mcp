// searchDriveFiles for a folder-confined agent, served by the gateway.
//
// The upstream cannot do this: Drive only knows direct parents, so the
// gateway expands the scope into its folder set first and then searches
// "... and ('f1' in parents or 'f2' in parents ...)" in chunks.
// Port of src/enforce/search.ts.
package enforce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
)

// SearchDefaults: applied by the gateway because the SDK applies none of the
// schema's defaults.
var SearchDefaults = struct {
	SearchIn      string
	OrderBy       string
	SortDirection string
	MaxResults    int
}{"both", "modifiedTime", "desc", 10}

// The upstream's own shortcuts (searchDriveFiles.js), verbatim.
var mimeTypeShortcuts = map[string]string{
	"document":     "application/vnd.google-apps.document",
	"spreadsheet":  "application/vnd.google-apps.spreadsheet",
	"presentation": "application/vnd.google-apps.presentation",
	"folder":       drive.FolderMime,
	"form":         "application/vnd.google-apps.form",
	"pdf":          "application/pdf",
	"zip":          "application/zip",
}

const chunkSize = 40
const resultFields = "nextPageToken,incompleteSearch,files(id,name,mimeType,size,modifiedTime,createdTime,webViewLink,owners(displayName,emailAddress),parents)"

type ScopeTooLarge struct{ Cap int }

func (e *ScopeTooLarge) Error() string { return fmt.Sprintf("more than %d folders in scope", e.Cap) }

// EscapeQ: same escaping as the upstream's escapeDriveQuery — backslash first,
// then quote.
func EscapeQ(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `'`, `\'`)
}

func chunks[T any](items []T, size int) [][]T {
	var out [][]T
	for i := 0; i < len(items); i += size {
		end := i + size
		if end > len(items) {
			end = len(items)
		}
		out = append(out, items[i:end])
	}
	return out
}

func anyParent(ids []string) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = "'" + EscapeQ(id) + "' in parents"
	}
	return strings.Join(parts, " or ")
}

// SubtreeCache: each scope's folder tree, five minutes at a time. Expanded
// level by level: one query per 40 folders of the frontier.
type SubtreeCache struct {
	drive func() *drive.Client

	mu      sync.Mutex
	cache   map[string]*subtreeEntry
	order   []string
	ttlMs   int64
	cap     int
	now     func() int64
	maxKeys int
}

type subtreeEntry struct {
	folders []string // nil remembers ScopeTooLarge
	at      int64
}

func NewSubtreeCache(d func() *drive.Client) *SubtreeCache {
	return &SubtreeCache{
		drive:   d,
		cache:   map[string]*subtreeEntry{},
		ttlMs:   300_000,
		cap:     500,
		now:     func() int64 { return time.Now().UnixMilli() },
		maxKeys: 200,
	}
}

func (c *SubtreeCache) Flush() {
	c.mu.Lock()
	c.cache = map[string]*subtreeEntry{}
	c.order = nil
	c.mu.Unlock()
}

func (c *SubtreeCache) remember(key string, folders []string) {
	if _, ok := c.cache[key]; !ok {
		c.order = append(c.order, key)
	}
	c.cache[key] = &subtreeEntry{folders: folders, at: c.now()}
	for len(c.order) > c.maxKeys {
		delete(c.cache, c.order[0])
		c.order = c.order[1:]
	}
}

func (c *SubtreeCache) Folders(ctx context.Context, roots []string) ([]string, error) {
	sorted := append([]string{}, roots...)
	sort.Strings(sorted)
	key := strings.Join(sorted, ",")

	c.mu.Lock()
	if hit, ok := c.cache[key]; ok && c.now()-hit.at < c.ttlMs {
		folders := hit.folders
		c.mu.Unlock()
		if folders == nil {
			return nil, &ScopeTooLarge{Cap: c.cap}
		}
		return folders, nil
	}
	delete(c.cache, key)
	c.mu.Unlock()

	d := c.drive()
	if d == nil {
		return nil, fmt.Errorf("Drive client not ready")
	}

	seen := map[string]bool{}
	for _, r := range roots {
		seen[r] = true
	}
	frontier := append([]string{}, roots...)
	for len(frontier) > 0 {
		var next []string
		for _, group := range chunks(frontier, chunkSize) {
			pageToken := ""
			for {
				page, err := d.ListFiles(ctx, drive.ListParams{
					Q:         "(" + anyParent(group) + ") and mimeType='" + drive.FolderMime + "' and trashed=false",
					Fields:    "nextPageToken,files(id)",
					PageSize:  1000,
					PageToken: pageToken,
				})
				if err != nil {
					return nil, err
				}
				for _, f := range page.Files {
					id, _ := f["id"].(string)
					if id == "" || seen[id] {
						continue
					}
					seen[id] = true
					next = append(next, id)
					if len(seen) > c.cap {
						c.mu.Lock()
						c.remember(key, nil)
						c.mu.Unlock()
						return nil, &ScopeTooLarge{Cap: c.cap}
					}
				}
				if page.NextPageToken == "" {
					break
				}
				pageToken = page.NextPageToken
			}
		}
		frontier = next
	}

	folders := make([]string, 0, len(seen))
	for id := range seen {
		folders = append(folders, id)
	}
	c.mu.Lock()
	c.remember(key, folders)
	c.mu.Unlock()
	return folders, nil
}

type SearchArgs struct {
	Query         string `json:"query"`
	SearchIn      string `json:"searchIn"`
	MimeType      string `json:"mimeType,omitempty"`
	OrderBy       string `json:"orderBy"`
	SortDirection string `json:"sortDirection"`
	MaxResults    int    `json:"maxResults"`
	ModifiedAfter string `json:"modifiedAfter,omitempty"`
}

// BuildConditions: the upstream's conditions, each parenthesised so none can
// absorb another.
func BuildConditions(args SearchArgs) ([]string, error) {
	term := EscapeQ(args.Query)
	conditions := []string{"trashed=false"}
	switch args.SearchIn {
	case "name":
		conditions = append(conditions, "name contains '"+term+"'")
	case "content":
		conditions = append(conditions, "fullText contains '"+term+"'")
	default:
		conditions = append(conditions, "name contains '"+term+"' or fullText contains '"+term+"'")
	}
	if args.MimeType != "" {
		resolved := args.MimeType
		if shortcut, ok := mimeTypeShortcuts[args.MimeType]; ok {
			resolved = shortcut
		}
		conditions = append(conditions, "mimeType='"+EscapeQ(resolved)+"'")
	}
	// Truthiness, as the upstream: "" means no filter, not an invalid date.
	if args.ModifiedAfter != "" {
		cutoff, err := time.Parse(time.RFC3339, args.ModifiedAfter)
		if err != nil {
			// Also accept the looser shapes JS Date parses; fail identically otherwise.
			cutoff2, err2 := parseJSDate(args.ModifiedAfter)
			if err2 != nil {
				return nil, fmt.Errorf("Invalid time value")
			}
			cutoff = cutoff2
		}
		// new Date(v).toISOString(): UTC with milliseconds, always.
		conditions = append(conditions, "modifiedTime > '"+EscapeQ(cutoff.UTC().Format("2006-01-02T15:04:05.000Z"))+"'")
	}
	return conditions, nil
}

func SearchQuery(conditions []string, parents []string) string {
	parts := make([]string, len(conditions))
	for i, c := range conditions {
		parts[i] = "(" + c + ")"
	}
	return strings.Join(parts, " and ") + " and (" + anyParent(parents) + ")"
}

// Field order matches the TS object literal; omitempty reproduces
// JSON.stringify dropping keys whose value is undefined, while size and
// owner are null-coerced upstream and always emit.
type searchFileOut struct {
	ID           any    `json:"id,omitempty"`
	Name         any    `json:"name,omitempty"`
	MimeType     any    `json:"mimeType,omitempty"`
	Size         *int64 `json:"size"`
	ModifiedTime any    `json:"modifiedTime,omitempty"`
	CreatedTime  any    `json:"createdTime,omitempty"`
	Owner        any    `json:"owner"`
	URL          any    `json:"url,omitempty"`
}

// searchEnvelope fixes the wire key order: files, total, hasMore. A Go map
// would marshal alphabetically and drift from the TS text.
type searchEnvelope struct {
	Files   []searchFileOut `json:"files"`
	Total   int             `json:"total"`
	HasMore bool            `json:"hasMore"`
}

// ScopedSearchDriveFiles mirrors the upstream tool's arguments and output.
func ScopedSearchDriveFiles(ctx context.Context, d *drive.Client, subtree *SubtreeCache, roots []string, rawArgs map[string]any, budgetMs int64) (string, error) {
	args := SearchArgs{
		SearchIn:      SearchDefaults.SearchIn,
		OrderBy:       SearchDefaults.OrderBy,
		SortDirection: SearchDefaults.SortDirection,
		MaxResults:    SearchDefaults.MaxResults,
	}
	if v, ok := rawArgs["query"].(string); ok {
		args.Query = v
	}
	if v, ok := rawArgs["searchIn"].(string); ok && v != "" {
		args.SearchIn = v
	}
	if v, ok := rawArgs["mimeType"].(string); ok {
		args.MimeType = v
	}
	if v, ok := rawArgs["orderBy"].(string); ok && v != "" {
		args.OrderBy = v
	}
	if v, ok := rawArgs["sortDirection"].(string); ok && v != "" {
		args.SortDirection = v
	}
	if v, ok := rawArgs["maxResults"].(float64); ok && v > 0 {
		args.MaxResults = int(v)
	}
	if v, ok := rawArgs["modifiedAfter"].(string); ok {
		args.ModifiedAfter = v
	}

	conditions, err := BuildConditions(args)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(budgetMs)*time.Millisecond)
	defer cancel()
	folders, err := subtree.Folders(ctx, roots)
	if err != nil {
		return "", err
	}
	orderBy := args.OrderBy
	if args.SortDirection == "desc" {
		orderBy += " desc"
	}

	byID := map[string]map[string]any{}
	more := false
	for _, group := range chunks(folders, chunkSize) {
		page, err := d.ListFiles(ctx, drive.ListParams{
			Q:        SearchQuery(conditions, group),
			Fields:   resultFields,
			PageSize: args.MaxResults,
			OrderBy:  orderBy,
		})
		if err != nil {
			return "", err
		}
		if page.NextPageToken != "" || page.IncompleteSearch {
			more = true
		}
		for _, f := range page.Files {
			if id, _ := f["id"].(string); id != "" {
				byID[id] = f
			}
		}
	}

	// Drive sorted each chunk; the merge has to sort again. Names compare with
	// localeCompare, which can differ from Drive's collation at the margins.
	merged := make([]map[string]any, 0, len(byID))
	for _, f := range byID {
		merged = append(merged, f)
	}
	key := args.OrderBy
	direction := -1 // desc default
	if args.SortDirection == "asc" {
		direction = 1
	}
	sort.SliceStable(merged, func(i, j int) bool {
		x := jsStr(merged[i][key])
		y := jsStr(merged[j][key])
		var order int
		if key == "name" {
			order = localeCompare(x, y)
		} else if x < y {
			order = -1
		} else if x > y {
			order = 1
		}
		return order*direction < 0
	})
	if len(merged) > args.MaxResults {
		more = true
		merged = merged[:args.MaxResults]
	}

	files := make([]searchFileOut, 0, len(merged))
	for _, f := range merged {
		out := searchFileOut{
			ID:           f["id"],
			Name:         f["name"],
			MimeType:     f["mimeType"],
			ModifiedTime: f["modifiedTime"],
			CreatedTime:  f["createdTime"],
			URL:          f["webViewLink"],
			Owner:        nil,
		}
		if size, ok := f["size"]; ok && size != nil {
			if n, err := toInt64(size); err == nil {
				out.Size = &n
			}
		}
		if owners, ok := f["owners"].([]any); ok && len(owners) > 0 {
			if owner, ok := owners[0].(map[string]any); ok {
				if name, _ := owner["displayName"].(string); name != "" {
					out.Owner = name
				}
			}
		}
		files = append(files, out)
	}
	return stringifySearch(searchEnvelope{files, len(files), more}), nil
}

// stringifySearch is JSON.stringify(envelope, null, 2): fixed key order, no
// HTML escaping, no trailing newline.
func stringifySearch(e searchEnvelope) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(e); err != nil {
		panic(err) // the envelope is always marshalable
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

// jsStr is String(value ?? "") for the merge-sort key: absent/nil → "",
// strings pass through, numbers format as JS would (no trailing .0).
func jsStr(v any) string {
	switch sv := v.(type) {
	case nil:
		return ""
	case string:
		return sv
	case float64:
		return strconv.FormatFloat(sv, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(sv)
	}
	return fmt.Sprint(v)
}

func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case string:
		var out int64
		_, err := fmt.Sscanf(n, "%d", &out)
		return out, err
	default:
		return 0, fmt.Errorf("not a number")
	}
}
