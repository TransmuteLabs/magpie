package gui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// The catalog and the manager retain authority over generations, files and
// actions. Discovery does not acquire a worker or enter the legacy Bun host.
type extensionWindowHost interface {
	Windows(context.Context) ([]extension.WindowModule, error)
	ReadWindowFile(context.Context, string, string, string) ([]byte, error)
	CallWindowBinding(context.Context, string, string, string, string, json.RawMessage) (json.RawMessage, error)
	CompareAndSwapWindowConfig(context.Context, string, string, string, json.RawMessage) (sdk.ConfigSnapshot, error)
	ReadyWindowServiceTarget(context.Context, string, string, string, string) (extension.WindowServiceTarget, func(), error)
}

const windowRequestLimit = (1 << 20) + (64 << 10) // a public 1 MiB value plus its envelope

var extensionWindows = func() extensionWindowHost { return extension.Default() }

type extensionWindowPart struct {
	id, revision, part, ref string
	dir, file               string // package-relative, never a caller-supplied disk path
	reads                   []string
	config                  sdk.ConfigSnapshot
}

func extensionWindowParts(ctx context.Context) ([]pluginPage, []pluginPanel, error) {
	host := extensionWindows()
	modules, err := host.Windows(ctx)
	if err != nil {
		return nil, nil, err
	}
	var pages []pluginPage
	var panels []pluginPanel
	for _, module := range modules {
		data, err := host.ReadWindowFile(ctx, module.ID, module.Revision, module.Metadata)
		if err != nil {
			return nil, nil, err
		}
		var metadata map[string]json.RawMessage
		if json.Unmarshal(data, &metadata) != nil || metadata == nil {
			return nil, nil, fmt.Errorf("invalid window metadata for %s", module.ID)
		}
		base := path.Dir(module.Metadata)
		target := filepath.Join(module.Directory, filepath.FromSlash(base))
		window, _ := plugin.WindowFromData(target, data)
		if window == nil {
			return nil, nil, fmt.Errorf("module %s has no readable window parts", module.ID)
		}
		key := "extension-" + hex.EncodeToString([]byte(module.ID))
		specID := "sdk:" + module.ID + "@" + module.PackageRevision + ":" + module.ConfigVersion
		reads := window.Reads
		if reads == nil {
			reads = []string{}
		}
		part := func(kind, dir, file string) *extensionWindowPart {
			rel, _ := filepath.Rel(target, dir)
			return &extensionWindowPart{
				id: module.ID, revision: module.Revision, part: kind,
				ref: pageToken("sdk\x00" + module.ID + "\x00" + module.Revision + "\x00" + module.Configuration.Version + "\x00" + kind),
				dir: path.Join(base, filepath.ToSlash(rel)), file: filepath.Base(file),
				reads: reads, config: module.Configuration,
			}
		}
		if spec := window.Page; spec != nil {
			p := part("window:page", spec.Dir, spec.File)
			page := pluginPage{Spec: specID, Name: module.ID, Key: key,
				Title: spec.Title, Titles: spec.Titles, Reads: reads, Ref: p.ref, window: p,
				token: p.ref, file: p.file, URL: pluginPagePrefix + p.ref + "/" + p.file,
				KeepAlive: spec.KeepAlive, Replaces: spec.Replaces}
			if page.Title == "" {
				page.Title = module.ID
			}
			if spec.Icon != "" {
				page.Icon = pluginPagePrefix + p.ref + "/" + spec.Icon
			}
			for _, action := range window.Actions {
				a := pluginAction{ID: action.ID, Menu: action.Menu, Title: action.Title, Titles: action.Titles}
				if a.Title == "" {
					a.Title, a.Titles = page.Title, page.Titles
				}
				page.Actions = append(page.Actions, a)
			}
			for _, binding := range module.Manifest.Bindings {
				if binding.From == p.part && binding.Service != "" {
					page.Service = pageServicePath + binding.ID + "/"
					break
				}
			}
			pages = append(pages, page)
		}
		for i, spec := range window.Panels {
			p := part("window:panel:"+strconv.Itoa(i), spec.Dir, spec.File)
			panel := pluginPanel{Spec: specID, Name: module.ID, Key: key + "-" + strconv.Itoa(i),
				Slot: spec.Slot, Title: spec.Title, Titles: spec.Titles, Reads: reads, Ref: p.ref, window: p,
				token: p.ref, file: p.file, URL: pluginPagePrefix + p.ref + "/" + p.file}
			if panel.Title == "" {
				panel.Title = module.ID
			}
			for _, binding := range module.Manifest.Bindings {
				if binding.From == p.part && binding.Service != "" {
					panel.Service = pageServicePath + binding.ID + "/"
					break
				}
			}
			panels = append(panels, panel)
		}
	}
	return pages, panels, nil
}

func resolveExtensionWindow(ctx context.Context, ref string) (*extensionWindowPart, error) {
	if len(ref) != 32 {
		return nil, errors.New("invalid module window reference")
	}
	pages, panels, err := extensionWindowParts(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		if p.Ref == ref {
			return p.window, nil
		}
	}
	for _, p := range panels {
		if p.Ref == ref {
			return p.window, nil
		}
	}
	return nil, errors.New("module window changed or is disabled")
}

// Only refs issued to the parent window are accepted. An iframe never supplies
// a module ID, revision, component, account scope, state key or endpoint URL.
func extensionWindowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/plugin-pages/call", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ref     string          `json:"ref"`
			Binding string          `json:"binding"`
			Request string          `json:"request"`
			Input   json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, windowRequestLimit)).Decode(&in); err != nil {
			fail(w, err)
			return
		}
		part, err := resolveExtensionWindow(r.Context(), in.Ref)
		if err != nil {
			fail(w, err)
			return
		}
		if len(in.Input) == 0 {
			in.Input = json.RawMessage("null")
		}
		if len(in.Request) > 256 {
			fail(w, errors.New("module request identity is too long"))
			return
		}
		ctx := r.Context()
		if in.Request != "" {
			ctx = extension.WithModuleRequestID(ctx, in.Request)
		}
		result, err := extensionWindows().CallWindowBinding(ctx, part.id, part.revision, part.part, in.Binding, in.Input)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, result)
	})
	mux.HandleFunc("POST /api/plugin-pages/config", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ref      string          `json:"ref"`
			Expected *string         `json:"expected"`
			Values   json.RawMessage `json:"values"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, windowRequestLimit)).Decode(&in); err != nil {
			fail(w, err)
			return
		}
		part, err := resolveExtensionWindow(r.Context(), in.Ref)
		if err != nil {
			fail(w, err)
			return
		}
		if in.Expected == nil && len(in.Values) == 0 {
			writeJSON(w, part.config)
			return
		}
		if in.Expected == nil || len(in.Values) == 0 {
			fail(w, errors.New("configuration requires an explicit CAS version and values"))
			return
		}
		config, err := extensionWindows().CompareAndSwapWindowConfig(r.Context(), part.id, part.revision, *in.Expected, in.Values)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, config)
	})
	mux.HandleFunc("GET /api/plugin-pages/changes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		timer := time.NewTimer(25 * time.Second)
		defer timer.Stop()
		for {
			version, err := extensionWindowVersion(r.Context())
			if err != nil {
				fail(w, err)
				return
			}
			if r.URL.Query().Get("version") != version {
				writeJSON(w, map[string]string{"version": version})
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
				writeJSON(w, map[string]string{"version": version})
				return
			case <-ticker.C:
			}
		}
	})
}

// Re-read metadata rather than relying on in-process notifications: a CLI in
// another process may install/disable a package or CAS its configuration.
func extensionWindowVersion(ctx context.Context) (string, error) {
	modules, err := extensionWindows().Windows(ctx)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, module := range modules {
		encoded, err := json.Marshal([]string{module.ID, module.Revision, module.Configuration.Version})
		if err != nil {
			return "", err
		}
		hash.Write(encoded)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func serveExtensionWindowFile(w http.ResponseWriter, r *http.Request, web bool) bool {
	ref, rel, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, pluginPagePrefix), "/")
	if !ok || len(ref) != 32 {
		return false
	}
	part, err := resolveExtensionWindow(r.Context(), ref)
	if err != nil {
		// Legacy tokens are resolved by the existing source. SDK tokens never
		// match it, including when catalog metadata is unreadable.
		return false
	}
	if strings.HasPrefix(rel, pageServicePath) {
		serveExtensionWindowService(w, r, part, rel)
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	read := func(rel string) ([]byte, error) {
		if rel == pluginBridgePath {
			return pageBridgeJS, nil
		}
		if rel == "" || path.IsAbs(rel) || strings.Contains(rel, "\\") {
			return nil, errors.New("invalid window file")
		}
		clean := path.Clean(rel)
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, errors.New("window file outside its part")
		}
		return extensionWindows().ReadWindowFile(r.Context(), part.id, part.revision, path.Join(part.dir, clean))
	}
	data, err := read(rel)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	var hashes []string
	if web && rel == part.file && isHTML(rel) {
		data, hashes = inlinePageWithReader(data, read)
		hashes = append(hashes, "data:")
	}
	w.Header().Set("Content-Security-Policy", pagePolicy(r, ref, hashes...))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, path.Base(rel), time.Time{}, bytes.NewReader(data))
	return true
}
