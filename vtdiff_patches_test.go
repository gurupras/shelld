package shelld

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The differential test builds its reference from published crates plus the named
// diffs in referencePatches, and it is the expensive, opt-in test: it needs cargo, a
// shelld-rs checkout and minutes of build. These two check the part of that which
// can be checked without any of them, because the failure they cover is one that
// only shows up as a cargo error minutes in, on the machine of whoever added the
// next patch.
//
// What can go wrong is the shape of the block rather than the contents of a diff.
// [patch.crates-io] gives one path per crate, so two defects in one crate have to
// arrive as one directory holding both — and the natural way to add the second
// defect, appending it to the list, is the way that puts the crate in the block
// twice.

// TestEachPatchedCrateIsNamedOnce is the whole of that: cargo rejects a duplicate
// key in [patch.crates-io], so a block naming a crate twice makes every run of the
// differential fail to build with an error about the manifest rather than about
// terminals.
func TestEachPatchedCrateIsNamedOnce(t *testing.T) {
	if len(referencePatches) < 2 {
		t.Skip("fewer than two reference patches, so nothing can collide")
	}
	seen := map[string][]string{}
	for _, tg := range crateTargets() {
		seen[tg.crate] = append(seen[tg.crate], fmt.Sprint(len(tg.patches))+" patch(es)")
	}
	for crate, entries := range seen {
		if len(entries) > 1 {
			t.Errorf("[patch.crates-io] would name %s %d times (%v): cargo rejects the manifest, so the differential cannot build at all",
				crate, len(entries), entries)
		}
	}
	// And every declared patch reaches one of those directories, so that grouping
	// cannot quietly drop one.
	var got int
	for _, tg := range crateTargets() {
		got += len(tg.patches)
	}
	if got != len(referencePatches) {
		t.Errorf("grouping carried %d of %d declared patches", got, len(referencePatches))
	}
}

// TestAPatchedCrateHoldsEveryPatchDeclaredForIt materialises the directories for
// real and looks in them, because the grouping above only says what the block will
// claim. A crate served with one of its two fixes missing builds and runs, and what
// it reports is a divergence in whichever defect was dropped — a reference that is
// wrong in exactly the way the patches exist to stop.
func TestAPatchedCrateHoldsEveryPatchDeclaredForIt(t *testing.T) {
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("no patch(1)")
	}
	// Every crate, not only the multi-patch ones, because patchedCrates below
	// materialises the whole block. Unlike the check above, this one needs a
	// directory cargo has to have filled first, and a machine that has never run
	// the differential has not filled it.
	//
	// The skip stays on the parent t, and moving it into a subtest per crate would
	// be wrong: patchedCrates calls registryCrate for every target, which t.Fatals
	// on the first one missing, so a subtest scoped to a crate that is unpacked
	// would still die on a crate that is not. The precondition is global because
	// the thing it guards materialises the whole block, so a partially populated
	// registry has to skip the whole test or a machine with no Rust work on it
	// reports as broken -- which is the failure unpackedCrate's two answers exist
	// to avoid. What the loop does owe is all of the missing crates rather than
	// the first, so one run names everything that has to be populated.
	var missing []string
	for _, tg := range crateTargets() {
		if dir, pattern := unpackedCrate(t, tg.crate, tg.version); dir == "" {
			missing = append(missing, pattern)
		}
	}
	if len(missing) > 0 {
		t.Skipf("%d of %d crates are not unpacked, so the patched block cannot be "+
			"materialised: this test needs the registry the differential populates "+
			"(SHELLD_VTDIFF_STOCK=1). Nothing matched:\n\t%s",
			len(missing), len(crateTargets()), strings.Join(missing, "\n\t"))
	}
	// Counted because everything this test asserts is inside a condition: the skip
	// above leaves the suite green, len(tg.patches) < 2 skips a crate silently, and
	// an empty crateTargets() would satisfy both loops. So a green here is only a
	// pass if something was looked at, and the counts are what say so.
	crates, patches := 0, 0
	for _, tg := range crateTargets() {
		if len(tg.patches) < 2 {
			continue
		}
		crates++
		// Only reached for a crate with more than one patch, which is the case the
		// grouping exists for; a single-patch crate is the old behaviour and the
		// differential itself covers it.
		root := t.TempDir()
		block, reference := patchedCrates(t, root)
		dir := ""
		for _, ln := range strings.Split(block, "\n") {
			if name, rest, ok := strings.Cut(ln, " = { path = "); ok && name == tg.crate {
				dir = strings.Trim(strings.TrimSuffix(rest, " }"), `"`)
			}
		}
		if dir == "" {
			t.Fatalf("%s is not named in the block:\n%s", tg.crate, block)
		}
		for _, p := range tg.patches {
			patches++
			// A patch applied is a patch that cannot be applied again: dry-running
			// it in reverse succeeds only where its changes are already
			// present, which is what asks the directory rather than the digest.
			cmd := exec.Command("patch", "-p1", "-R", "--dry-run", "-d", dir)
			diff, err := os.ReadFile(p.diff)
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stdin = strings.NewReader(string(diff))
			cmd.Env = append(os.Environ(), "TMPDIR="+root)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s is served from %s without %s applied (%v):\n%s\nreference claims:%s",
					tg.crate, filepath.Base(dir), filepath.Base(p.diff), err, out, reference)
			}
		}
	}
	if crates == 0 || patches == 0 {
		t.Errorf("checked %d multi-patch crates and %d patches: nothing was asserted, so "+
			"this test passed on an absence rather than on the directories holding "+
			"their patches", crates, patches)
	}
}
