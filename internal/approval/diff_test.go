package approval

import (
	"reflect"
	"testing"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

func TestVerifyDiff_SubsetNoDrift(t *testing.T) {
	scope := types.PlanScope{FilesTouched: []string{"a.go", "b.go", "c.go"}}
	status := " M a.go\n M b.go\n"
	r := VerifyDiff(status, scope, 2)
	if r.Drifted {
		t.Error("expected Drifted=false")
	}
	if len(r.ExtraFiles) != 0 {
		t.Errorf("expected no extras, got %v", r.ExtraFiles)
	}
	if r.AllowedDrift != 2 {
		t.Errorf("AllowedDrift = %d", r.AllowedDrift)
	}
}

func TestVerifyDiff_OneExtraWithinLimit(t *testing.T) {
	scope := types.PlanScope{FilesTouched: []string{"a.go"}}
	status := " M a.go\n?? extra.go\n"
	r := VerifyDiff(status, scope, 2)
	if r.Drifted {
		t.Errorf("expected no drift, got %+v", r)
	}
	if !reflect.DeepEqual(r.ExtraFiles, []string{"extra.go"}) {
		t.Errorf("ExtraFiles = %v", r.ExtraFiles)
	}
}

func TestVerifyDiff_ThreeExtrasExceedsLimit(t *testing.T) {
	scope := types.PlanScope{FilesTouched: []string{"a.go"}}
	status := " M a.go\n?? x.go\n?? y.go\n?? z.go\n"
	r := VerifyDiff(status, scope, 2)
	if !r.Drifted {
		t.Errorf("expected drift, got %+v", r)
	}
	if len(r.ExtraFiles) != 3 {
		t.Errorf("ExtraFiles = %v", r.ExtraFiles)
	}
}

func TestVerifyDiff_EmptyStatus(t *testing.T) {
	scope := types.PlanScope{FilesTouched: []string{"a.go"}}
	r := VerifyDiff("", scope, 0)
	if r.Drifted || len(r.ExtraFiles) != 0 {
		t.Errorf("expected clean, got %+v", r)
	}
}

func TestVerifyDiff_RenameUsesNewPath(t *testing.T) {
	scope := types.PlanScope{FilesTouched: []string{"new.go"}}
	status := "R  old.go -> new.go\n"
	r := VerifyDiff(status, scope, 0)
	if r.Drifted {
		t.Errorf("expected no drift for renamed file, got %+v", r)
	}
	if len(r.ExtraFiles) != 0 {
		t.Errorf("ExtraFiles = %v", r.ExtraFiles)
	}
}

func TestFilesFromGitStatus_RenameNewPath(t *testing.T) {
	files := FilesFromGitStatus("R  old.go -> new.go\n")
	if !reflect.DeepEqual(files, []string{"new.go"}) {
		t.Errorf("files = %v", files)
	}
}

func TestFilesFromGitStatus_Whitespace(t *testing.T) {
	status := "\n M  a.go\r\n?? b.go  \n\n"
	files := FilesFromGitStatus(status)
	if !reflect.DeepEqual(files, []string{"a.go", "b.go"}) {
		t.Errorf("files = %v", files)
	}
}
