package http

import (
	"encoding/json"
	"net/http"

	"photoboot-picstop/internal/application/filter"
)

// FilterController handles GET /filters and PUT /filter.
type FilterController struct {
	port filter.IFilterPort
}

// NewFilterController builds the controller with the injected filter port.
func NewFilterController(port filter.IFilterPort) *FilterController {
	return &FilterController{port: port}
}

// ServeList handles GET /filters — returns a JSON array of available filter names.
func (c *FilterController) ServeList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET /filters"})
		return
	}
	names := c.port.ListFilters()
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, names)
}

// ServeSet handles PUT /filter — sets the active filter from a JSON body {name: "..."}.
func (c *FilterController) ServeSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use PUT /filter"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if err := c.port.SetActiveFilter(body.Name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"active": body.Name})
}
