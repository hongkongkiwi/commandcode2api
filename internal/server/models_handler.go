package server

import (
	"net/http"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/config"
)

// HandleModels ports handleModels: live catalog (5-min cache) with
// hardcoded fallback, OpenAI list shape.
func (d *Deps) HandleModels(w http.ResponseWriter, r *http.Request) {
	apiKey := ExtractAPIKey(r.Header)
	models := d.Models.List(r.Context(), apiKey, d.Cfg.UseProviderModels)
	now := cc.NowUnix()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m["id"], "object": "model", "created": now, "owned_by": "command-code",
		})
	}
	writeJSONBody(w, 200, map[string]any{"object": "list", "data": data})
}

var _ = config.EnvInt
