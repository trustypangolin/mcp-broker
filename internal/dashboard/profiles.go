package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/supervisor"
)

type profileView struct {
	supervisor.Status
	Group string `json:"group,omitempty"`
	// Def is the entry as broker.yaml has it, for the edit form (admins only).
	Def    *config.ProfileEntry `json:"def,omitempty"`
	Issues []string             `json:"issues,omitempty"`
}

func (d *Dashboard) listProfiles(w http.ResponseWriter, r *http.Request) {
	cfg := d.b.Config()
	isAdmin := d.allowed(r, admin)
	var data []byte
	issues := map[string][]string{}
	if isAdmin {
		data, _ = os.ReadFile(cfg.Path)
		for _, i := range cfg.Check() {
			if name, ok := strings.CutPrefix(i.Where, "profiles."); ok {
				name, _, _ = strings.Cut(name, ".")
				issues[name] = append(issues[name], i.String())
			}
		}
	}
	out := []profileView{}
	for _, st := range d.b.Supervisor().List() {
		v := profileView{Status: st, Group: cfg.Profiles[st.Profile].Group}
		if isAdmin {
			v.Issues = issues[st.Profile]
			var e config.ProfileEntry
			if ok, err := config.GetEntry(data, "profiles", st.Profile, &e); ok && err == nil {
				v.Def = &e
			}
		}
		out = append(out, v)
	}
	writeJSON(w, map[string]any{"profiles": out, "admin": isAdmin})
}

// putProfile adds or replaces a profile. A running profile keeps the definition it was
// started with until it is restarted.
func (d *Dashboard) putProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName.MatchString(name) {
		jsonError(w, http.StatusBadRequest, "names are letters, digits, '.', '_' and '-'")
		return
	}
	var e config.ProfileEntry
	if err := decode(r, &e); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if strings.TrimSpace(e.Command) == "" {
		jsonError(w, http.StatusBadRequest, "a profile needs a command")
		return
	}
	e.Normalize()
	// ?new=1 is the add and duplicate forms: never replace an existing entry.
	if _, exists := d.b.Config().Profiles[name]; exists && r.URL.Query().Get("new") != "" {
		jsonError(w, http.StatusConflict, "a profile named "+name+" already exists; pick another name")
		return
	}
	d.b.Logger().Info("profile saved from dashboard", "profile", name)
	d.apply(w, "profiles", name, e)
}

// deleteProfile removes a stopped profile. A running one is refused: once it is out of the
// config the dashboard could no longer show or stop it.
func (d *Dashboard) deleteProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := d.b.Config().Profiles[name]; !ok {
		jsonError(w, http.StatusNotFound, "no such profile")
		return
	}
	if st := d.b.Supervisor().Status(name).State; st != supervisor.Stopped && st != supervisor.Exited {
		jsonError(w, http.StatusConflict, name+" is "+string(st)+"; stop it before removing it")
		return
	}
	d.b.Logger().Info("profile removed from dashboard", "profile", name)
	d.apply(w, "profiles", name, nil)
}

func (d *Dashboard) profileLogs(w http.ResponseWriter, r *http.Request) {
	n := 300
	fmt.Sscan(r.URL.Query().Get("n"), &n)
	lines := d.b.Supervisor().Logs(r.PathValue("name"), min(max(n, 1), 5000))
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, map[string]any{"lines": lines})
}

func (d *Dashboard) profilePlan(w http.ResponseWriter, r *http.Request) {
	what := r.URL.Query().Get("for")
	if what == "" {
		what = "start"
	}
	pl, err := d.b.Supervisor().Plan(r.PathValue("name"), what)
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"plan": pl, "text": pl.Text()})
}

// profileAction is the user acting from the dashboard, so no approval is asked.
func (d *Dashboard) profileAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	st, err := server.ProfileAction(r.Context(), d.b.Supervisor(), name, action)
	if err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	d.b.Logger().Info("profile action from dashboard", "profile", name, "action", action)
	writeJSON(w, st)
}
