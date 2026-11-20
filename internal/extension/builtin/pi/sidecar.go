package pi

import (
	"bytes"
	"errors"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	SidecarFile = "extensions/magpie.ts"
	SidecarMark = "// magpie: written by magpie"
)

func WithSidecar(plan sdk.AgentPlan, catalog, sidecar sdk.ActionSettingFile, render func() string) (sdk.AgentPlan, error) {
	if catalog.Path != CatalogFile || sidecar.Path != SidecarFile {
		return sdk.AgentPlan{}, errors.New("Pi extension requires catalog and extension snapshots")
	}
	if sidecar.Exists && !bytes.HasPrefix(edit.TrimBOM(sidecar.Body), []byte(SidecarMark)) {
		return plan, nil
	}
	for _, change := range plan.Files {
		if change.Path == CatalogFile {
			catalog.Body = change.Body
			break
		}
	}
	var want string
	if parseSettings(catalog.Body).Get("providers.magpie").Exists() {
		want = render()
	}
	if want == "" && !sidecar.Exists || want != "" && want == string(sidecar.Body) {
		return plan, nil
	}
	plan.Files = append(plan.Files, sdk.FileChange{
		Path: sidecar.Path, BeforeHash: sidecar.BeforeHash, Body: []byte(want), Remove: want == "",
	})
	return plan, nil
}
