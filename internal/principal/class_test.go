package principal_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/principal"
)

// The table in docs/design.md#access-control, in a test: if the design changes
// what a class may do, this is what fails.
func TestClassRights(t *testing.T) {
	tests := []struct {
		class principal.Class
		read  principal.Read
		write principal.Write
	}{
		{principal.ClassObserver, principal.ReadScoped, 0},
		{principal.ClassWorker, principal.ReadScopedCode, principal.WriteAssert},
		{principal.ClassOrchestrator, principal.ReadScopedCode, principal.WriteAssert | principal.WriteSubscribe},
		{principal.ClassSteward, principal.ReadAll, principal.WriteAssert | principal.WriteSubscribe | principal.WriteRatify | principal.WriteMerge},
		// A class that is not one of the four reads nothing and writes
		// nothing: a typo that got past validation must not read everything.
		{principal.Class("admin"), principal.ReadNone, 0},
		{principal.Class(""), principal.ReadNone, 0},
	}
	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			got := tt.class.Rights()
			if got.Read != tt.read || got.Write != tt.write {
				t.Errorf("Rights() = {%s %s}, want {%s %s}", got.Read, got.Write, tt.read, tt.write)
			}
		})
	}
}

// The four classes are a chain, which is what makes intersecting two of them
// the smaller of the two rather than an arbitrary choice.
func TestClassesAreAChain(t *testing.T) {
	classes := principal.Classes()
	if !slices.Equal(classes, []principal.Class{
		principal.ClassObserver, principal.ClassWorker, principal.ClassOrchestrator, principal.ClassSteward,
	}) {
		t.Fatalf("Classes() = %v", classes)
	}
	for i := 1; i < len(classes); i++ {
		lower, higher := classes[i-1].Rights(), classes[i].Rights()
		if higher.Read < lower.Read {
			t.Errorf("%s reads %s, less than %s's %s", classes[i], higher.Read, classes[i-1], lower.Read)
		}
		if !higher.Write.Has(lower.Write) {
			t.Errorf("%s writes %s, which does not include %s's %s", classes[i], higher.Write, classes[i-1], lower.Write)
		}
	}
	// Classes() hands out a copy: a caller sorting it must not reorder the
	// chain for everyone else.
	classes[0] = principal.ClassSteward
	if principal.Classes()[0] != principal.ClassObserver {
		t.Error("Classes() shares its slice")
	}
}

func TestClassValid(t *testing.T) {
	for _, c := range principal.Classes() {
		if !c.Valid() {
			t.Errorf("%s is not valid", c)
		}
	}
	for _, c := range []principal.Class{"", "admin", "Observer", "human"} {
		if principal.Class(c).Valid() {
			t.Errorf("%q is valid", c)
		}
	}
}

func TestRightsIntersect(t *testing.T) {
	tests := []struct {
		name    string
		a, b    principal.Rights
		want    principal.Rights
		wantStr string
	}{{
		name: "the lesser reach and the writes both allow",
		a:    principal.Rights{Read: principal.ReadAll, Write: principal.WriteAssert | principal.WriteRatify},
		b:    principal.Rights{Read: principal.ReadScoped, Write: principal.WriteAssert | principal.WriteSubscribe},
		want: principal.Rights{Read: principal.ReadScoped, Write: principal.WriteAssert},
	}, {
		name: "the zero value grants nothing to anyone",
		a:    principal.ClassSteward.Rights(),
		b:    principal.Rights{},
		want: principal.Rights{},
	}, {
		name: "intersecting with itself changes nothing",
		a:    principal.ClassWorker.Rights(),
		b:    principal.ClassWorker.Rights(),
		want: principal.ClassWorker.Rights(),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Intersect(tt.b); got != tt.want {
				t.Errorf("Intersect = %+v, want %+v", got, tt.want)
			}
			if got := tt.b.Intersect(tt.a); got != tt.want {
				t.Errorf("Intersect the other way round = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestWriteHasAndString(t *testing.T) {
	all := principal.WriteAssert | principal.WriteSubscribe | principal.WriteRatify | principal.WriteMerge
	if !all.Has(principal.WriteRatify | principal.WriteMerge) {
		t.Error("everything does not have ratify and merge")
	}
	// Has means all of them, not any of them.
	if (principal.WriteAssert).Has(principal.WriteAssert | principal.WriteRatify) {
		t.Error("assert alone has assert and ratify")
	}
	if principal.Write(0).Has(principal.WriteAssert) {
		t.Error("nothing has assert")
	}
	// An empty set has an empty set: a caller asking "may it do nothing" is
	// not told no.
	if !principal.Write(0).Has(0) {
		t.Error("nothing does not have nothing")
	}

	tests := []struct {
		in   principal.Write
		want string
	}{
		{0, "none"},
		{principal.WriteAssert, "assert"},
		{all, "assert,subscribe,ratify,merge"},
		{principal.WriteMerge | principal.WriteAssert, "assert,merge"},
		{1 << 7, "unknown"},
		{principal.WriteAssert | 1<<7, "assert,unknown"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Write(%d).String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReadString(t *testing.T) {
	tests := []struct {
		in   principal.Read
		want string
	}{
		{principal.ReadNone, "none"},
		{principal.ReadScoped, "scoped"},
		{principal.ReadScopedCode, "scoped+code"},
		{principal.ReadAll, "all"},
		{principal.Read(9), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Read(%d).String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}
