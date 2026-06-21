package checks

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/orkanoio/orkano/api/check"
)

// passing is a minimal valid check whose probe always passes.
func passing(id string, requires ...string) check.Check {
	return check.Check{
		ID:       id,
		Severity: check.SeverityInfo,
		Requires: requires,
		Probe: func(context.Context) (check.Result, error) {
			return check.Result{Status: check.StatusPass}, nil
		},
	}
}

func TestRegisterValidation(t *testing.T) {
	valid := passing("ok")

	tests := []struct {
		name    string
		check   check.Check
		wantErr error
	}{
		{"valid", valid, nil},
		{"empty ID", check.Check{Severity: check.SeverityInfo, Probe: valid.Probe}, ErrEmptyID},
		{"no probe", check.Check{ID: "x", Severity: check.SeverityInfo}, ErrNoProbe},
		{"empty severity", check.Check{ID: "x", Probe: valid.Probe}, ErrInvalidSeverity},
		{"bogus severity", check.Check{ID: "x", Severity: "nope", Probe: valid.Probe}, ErrInvalidSeverity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := New().Register(tt.check)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Register() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	r := New()
	if err := r.Register(passing("dup")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(passing("dup")); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("second Register = %v, want ErrDuplicateID", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 (duplicate must not be stored)", r.Len())
	}
}

func TestMustRegisterPanicsOnBadCheck(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustRegister did not panic on an invalid check")
		}
	}()
	New().MustRegister(check.Check{ID: "", Severity: check.SeverityInfo})
}

func TestGet(t *testing.T) {
	r := New()
	r.MustRegister(passing("net.dns"))
	if _, ok := r.Get("net.dns"); !ok {
		t.Fatal("Get(net.dns) not found after register")
	}
	if _, ok := r.Get("absent"); ok {
		t.Fatal("Get(absent) reported found")
	}
}

func planIDs(t *testing.T, r *Registry) []string {
	t.Helper()
	order, err := r.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	ids := make([]string, len(order))
	for i, c := range order {
		ids[i] = c.ID
	}
	return ids
}

func TestPlanRespectsRequires(t *testing.T) {
	r := New()
	// Register a dependent before its requirement to prove Plan reorders by
	// the graph, not by registration order.
	r.MustRegister(passing("ports.free", "ssh.reachable"))
	r.MustRegister(passing("ssh.reachable"))

	ids := planIDs(t, r)
	if got, want := indexOf(ids, "ssh.reachable"), indexOf(ids, "ports.free"); got > want {
		t.Fatalf("requirement ran after dependent: order=%v", ids)
	}
}

func TestPlanTieBreaksOnRegistrationOrder(t *testing.T) {
	r := New()
	// All independent: execution order must equal registration order.
	for _, id := range []string{"c", "a", "b"} {
		r.MustRegister(passing(id))
	}
	if got := planIDs(t, r); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("Plan order = %v, want registration order [c a b]", got)
	}
}

func TestPlanDiamond(t *testing.T) {
	r := New()
	r.MustRegister(passing("root"))
	r.MustRegister(passing("left", "root"))
	r.MustRegister(passing("right", "root"))
	r.MustRegister(passing("join", "left", "right"))

	ids := planIDs(t, r)
	if indexOf(ids, "root") != 0 {
		t.Fatalf("root not first: %v", ids)
	}
	if indexOf(ids, "join") != len(ids)-1 {
		t.Fatalf("join not last: %v", ids)
	}
	for _, mid := range []string{"left", "right"} {
		if indexOf(ids, mid) < indexOf(ids, "root") || indexOf(ids, mid) > indexOf(ids, "join") {
			t.Fatalf("%s out of order: %v", mid, ids)
		}
	}
}

func TestPlanMissingRequirement(t *testing.T) {
	r := New()
	r.MustRegister(passing("needs", "absent"))
	if _, err := r.Plan(); !errors.Is(err, ErrMissingRequirement) {
		t.Fatalf("Plan = %v, want ErrMissingRequirement", err)
	}
}

func TestPlanCycle(t *testing.T) {
	r := New()
	r.MustRegister(passing("a", "b"))
	r.MustRegister(passing("b", "a"))
	if _, err := r.Plan(); !errors.Is(err, ErrCycle) {
		t.Fatalf("Plan = %v, want ErrCycle", err)
	}
}

func TestPlanSelfCycle(t *testing.T) {
	r := New()
	r.MustRegister(passing("a", "a"))
	if _, err := r.Plan(); !errors.Is(err, ErrCycle) {
		t.Fatalf("Plan = %v, want ErrCycle", err)
	}
}

func TestPlanEmptyRegistry(t *testing.T) {
	if ids := planIDs(t, New()); len(ids) != 0 {
		t.Fatalf("empty registry planned %v", ids)
	}
}

func indexOf(s []string, v string) int { return slices.Index(s, v) }
