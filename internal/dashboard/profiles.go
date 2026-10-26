package dashboard

import (
	"fmt"
	"net/http"

	"github.com/trustypangolin/mcp-broker/internal/server"
)

func (d *Dashboard) listProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"profiles": d.b.Supervisor().List(), "admin": d.allowed(r, admin)})
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
