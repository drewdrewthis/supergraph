package tmux

import "testing"

func TestSupportsPaneExitedPerVersion(t *testing.T) {
	if supportsPaneExited("tmux 3.6a") {
		t.Error("3.6a must NOT be treated as supporting pane-exited (layout-change branch)")
	}
	if !supportsPaneExited("tmux 3.7") {
		t.Error("3.7 must be treated as supporting pane-exited")
	}
	if !supportsPaneExited("tmux 4.0") {
		t.Error("4.0 must support pane-exited")
	}
}

func TestIsStructuralVersionGated(t *testing.T) {
	// layout-change is structural on every version.
	if !isStructural("%layout-change ...", "tmux 3.6a") {
		t.Error("layout-change must trigger reconcile on 3.6a")
	}
	// pane-exited is only a recognised trigger where the version emits it.
	if isStructural("%pane-exited @1 @2 0", "tmux 3.6a") {
		t.Error("pane-exited must not be a 3.6a trigger")
	}
	if !isStructural("%pane-exited @1 @2 0", "tmux 3.7") {
		t.Error("pane-exited must trigger on 3.7")
	}
	// Non-structural / content lines never trigger.
	if isStructural("%output %1 hello", "tmux 3.7") {
		t.Error("output must never trigger a reconcile")
	}
}

func TestVersionAtLeast(t *testing.T) {
	if !versionAtLeast("tmux 3.6a", 3, 6) {
		t.Error("3.6a >= 3.6")
	}
	if versionAtLeast("tmux 3.5", 3, 6) {
		t.Error("3.5 < 3.6")
	}
	if !versionAtLeast("tmux 4.1", 3, 6) {
		t.Error("4.1 >= 3.6")
	}
}
