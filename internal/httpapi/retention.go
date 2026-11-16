package httpapi

import (
	"atlas-render/internal/model"
	"atlas-render/internal/service"
	"net/http"
	"time"
)

func (a API) previewRetention(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ArtifactIDs []string   `json:"artifactIds"`
		Before      *time.Time `json:"before"`
		Note        string     `json:"note"`
	}
	if e := decode(w, r, &b); e != nil {
		fail(w, e)
		return
	}
	v, e := a.Service.PreviewRetention(service.RetentionRequest{ArtifactIDs: b.ArtifactIDs, Before: b.Before, Note: b.Note})
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 201, v)
}

func (a API) listRetention(w http.ResponseWriter, r *http.Request) {
	write(w, 200, a.Service.RetentionPreviews())
}

func (a API) getRetention(w http.ResponseWriter, r *http.Request) {
	v, e := a.Service.RetentionPreview(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, v)
}

func (a API) executeRetention(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Token      string `json:"token"`
		ArtifactID string `json:"artifactId"`
	}
	if e := decode(w, r, &b); e != nil {
		fail(w, e)
		return
	}
	if b.Token == "" {
		fail(w, model.Invalid("retention preview token is required"))
		return
	}
	v, e := a.Service.ExecuteRetention(r.PathValue("id"), b.Token, b.ArtifactID)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, v)
}
