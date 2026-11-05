package dashboard

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/importer"
)

// importFile is a client config pasted or uploaded into the import dialog.
type importFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type importRequest struct {
	Files   []importFile               `json:"files"`
	Scan    bool                       `json:"scan"` // also read the client configs on this computer
	Choices map[string]importer.Choice `json:"choices"`
}

type importSource struct {
	Name    string `json:"name"`
	Client  string `json:"client"`
	Servers int    `json:"servers"`
	Error   string `json:"error,omitempty"`
}

// importItem is one planned server, for the dialog. Secret values never leave the broker:
// only their names are shown.
type importItem struct {
	ID         string                `json:"id"`
	Server     string                `json:"server"`
	Source     string                `json:"source"`
	Client     string                `json:"client"`
	Scope      string                `json:"scope,omitempty"`
	Status     string                `json:"status"` // new, variant, existing or skip
	Name       string                `json:"name"`
	VariantOf  string                `json:"variant_of,omitempty"`
	Diffs      []string              `json:"diffs,omitempty"`
	Entry      config.ConnectorEntry `json:"entry"`
	Secrets    []string              `json:"secrets,omitempty"`
	Notes      []string              `json:"notes,omitempty"`
	Visible    []string              `json:"visible"`
	Also       []string              `json:"also,omitempty"`
	Skip       string                `json:"skip,omitempty"`
	Disabled   bool                  `json:"disabled,omitempty"`
	GroupsWith bool                  `json:"groups_existing,omitempty"`
}

// importBodyLimit allows a large ~/.claude.json (it keeps history) to be uploaded.
const importBodyLimit = 32 << 20

// planImport reads the files (and this computer's configs, when asked), plans the import
// and applies the person's choices.
func (d *Dashboard) planImport(w http.ResponseWriter, r *http.Request) (*config.Config, []importSource, []importer.Item, bool) {
	var req importRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, importBodyLimit)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return nil, nil, nil, false
	}
	if len(req.Files) == 0 && !req.Scan {
		jsonError(w, http.StatusBadRequest, "add a file, paste one, or scan this computer")
		return nil, nil, nil, false
	}
	cfg := d.b.Config()
	var sources []importSource
	var servers []importer.Server
	for i, f := range req.Files {
		name := f.Name
		if name == "" {
			name = fmt.Sprintf("pasted-%d", i+1)
		}
		s, src, err := importer.Parse(importer.SourceFor(name), []byte(f.Content))
		is := importSource{Name: name, Client: string(src.Client), Servers: len(s)}
		if err != nil {
			is.Error = err.Error()
		}
		sources = append(sources, is)
		servers = append(servers, s...)
	}
	if req.Scan {
		for _, src := range importer.ScanSources() {
			s, err := importer.Read(src)
			is := importSource{Name: src.Path, Client: string(src.Client), Servers: len(s)}
			if err != nil {
				is.Error = err.Error()
			}
			if len(s) > 0 {
				is.Client = string(s[0].Source.Client)
			}
			sources = append(sources, is)
			servers = append(servers, s...)
		}
	}
	items := importer.Plan(cfg, servers, importer.Options{})
	if err := importer.Choose(cfg, items, req.Choices); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return nil, nil, nil, false
	}
	return cfg, sources, items, true
}

func viewItem(it importer.Item) importItem {
	v := importItem{ID: it.ID(), Server: it.Server.Name, Source: it.Server.Source.Path, Client: string(it.Server.Source.Client),
		Scope: it.Server.Scope, Name: it.Name, VariantOf: it.VariantOf, Diffs: it.Diffs, Entry: it.Entry,
		Secrets: slices.Sorted(maps.Keys(it.Secrets)), Notes: it.Notes, Visible: it.Visible, Skip: it.Skip,
		Disabled: it.Server.Disabled, GroupsWith: it.GroupExisting}
	if v.Visible == nil {
		v.Visible = []string{}
	}
	for _, s := range it.Also {
		v.Also = append(v.Also, s.Path)
	}
	switch {
	case it.Skip != "":
		v.Status = "skip"
	case it.Existing:
		v.Status = "existing"
	case it.VariantOf != "":
		v.Status = "variant"
	default:
		v.Status = "new"
	}
	return v
}

// importPlan shows what importing the files would do, with the person's edits applied.
func (d *Dashboard) importPlan(w http.ResponseWriter, r *http.Request) {
	_, sources, items, ok := d.planImport(w, r)
	if !ok {
		return
	}
	out := []importItem{}
	for _, it := range items {
		out = append(out, viewItem(it))
	}
	if sources == nil {
		sources = []importSource{}
	}
	writeJSON(w, map[string]any{"sources": sources, "items": out})
}

// importApply plans again from the same files and choices, then writes the connectors
// (secrets to env files) and reloads.
func (d *Dashboard) importApply(w http.ResponseWriter, r *http.Request) {
	cfg, _, items, ok := d.planImport(w, r)
	if !ok {
		return
	}
	n, err := importer.Apply(cfg, items)
	if err != nil {
		jsonError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var added []string
	for _, it := range items {
		if it.Imported() {
			added = append(added, it.Name)
		}
	}
	res, err := d.b.Reload()
	if err != nil {
		jsonError(w, http.StatusUnprocessableEntity, fmt.Sprintf("added %d connector(s), but the reload failed: %v", n, err))
		return
	}
	writeJSON(w, map[string]any{"added": added, "reload": res})
}
