// Regression tests for policy-store edge cases found in compatibility review.
package policy

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Finding 1: JS string.length/.slice count UTF-16 code units, not bytes.
func TestUTF16NameAndLabelLimits(t *testing.T) {
	store, err := OpenPolicyStore(filepath.Join(t.TempDir(), "u.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 40 emoji = 80 UTF-16 units (accepted by TS) but 160 bytes (rejected by a
	// naive Go port).
	emoji40 := strings.Repeat("😀", 40)
	if err := store.CreateAgent("emoji-ok", emoji40); err != nil {
		t.Errorf("40-emoji name must be accepted (JS parity): %v", err)
	}
	// 41 emoji = 82 units: rejected in both worlds.
	if err := store.CreateAgent("emoji-bad", strings.Repeat("😀", 41)); err != ErrInvalidName {
		t.Errorf("41-emoji name must be invalid_name, got %v", err)
	}

	// Label slice at 80 UTF-16 units must not split a surrogate pair.
	agent := "emoji-ok"
	label := strings.Repeat("😀", 45) // 90 units -> sliced to 80 units = 40 emoji
	id, err := store.InsertToken(agent, HashToken("x"), "prefix", label)
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.GetToken(id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Label != emoji40 {
		t.Errorf("label sliced wrong: %d emoji expected, got %q", 40, row.Label)
	}
}

// Finding 2: concurrent same-slug CreateAgent yields one ErrExists, never a
// raw SQLite constraint error.
func TestConcurrentCreateAgent(t *testing.T) {
	store, err := OpenPolicyStore(filepath.Join(t.TempDir(), "race.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.CreateAgent("twin", "Twin")
		}(i)
	}
	wg.Wait()
	var ok, exists int
	for _, err := range errs {
		switch err {
		case nil:
			ok++
		case ErrExists:
			exists++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || exists != len(errs)-1 {
		t.Errorf("ok=%d exists=%d", ok, exists)
	}
}

// Finding 2: concurrent RemoveScope yields exactly one success.
func TestConcurrentRemoveScope(t *testing.T) {
	store, err := OpenPolicyStore(filepath.Join(t.TempDir(), "race2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateAgent("aa", "A"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddScope("aa", "folderXxxxxxxx", "X", false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.RemoveScope("aa", "folderXxxxxxxx")
		}(i)
	}
	wg.Wait()
	var ok, notFound int
	for _, err := range errs {
		switch err {
		case nil:
			ok++
		case ErrNotFound:
			notFound++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || notFound != len(errs)-1 {
		t.Errorf("ok=%d notFound=%d", ok, notFound)
	}
}

// Finding 3: JS String(null) === "null".
func TestParseResourcesNull(t *testing.T) {
	got := parseResources(`["a", null, 1]`)
	want := []string{"a", "null", "1"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("parseResources: %v", got)
	}
}
