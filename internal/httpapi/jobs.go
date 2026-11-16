package httpapi

import (
	"atlas-render/internal/model"
	"atlas-render/internal/observe"
	"net/http"
)

func (a API) submit(w http.ResponseWriter, r *http.Request) {
	var b struct {
		SnapshotID string `json:"snapshotId"`
		Scale      int    `json:"scale"`
	}
	if e := decode(w, r, &b); e != nil {
		fail(w, e)
		return
	}
	v, e := a.Service.Submit(b.SnapshotID, b.Scale, r.Header.Get("Idempotency-Key"))
	if e != nil {
		fail(w, e)
		return
	}
	v.Payload = model.Snapshot{}
	write(w, 202, v)
}
func (a API) getJob(w http.ResponseWriter, r *http.Request) {
	v, ok := a.Service.Store.Read().Jobs[r.PathValue("id")]
	if !ok {
		fail(w, model.Missing("job not found"))
		return
	}
	v.Payload = model.Snapshot{}
	write(w, 200, v)
}
func (a API) cancel(w http.ResponseWriter, r *http.Request) {
	v, e := a.Service.Cancel(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	a.Worker.Cancel(v.ID)
	v.Payload = model.Snapshot{}
	write(w, 200, v)
}
func (a API) retry(w http.ResponseWriter, r *http.Request) {
	v, e := a.Service.Retry(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	v.Payload = model.Snapshot{}
	write(w, 202, v)
}
func (a API) download(w http.ResponseWriter, r *http.Request) {
	// Lease the result before reading it so a concurrent purge either waits or
	// reports a conflict instead of yanking bytes out of an in-flight response.
	release := a.Service.OpenRead(r.PathValue("id"))
	defer release()
	v, e := a.Service.Artifact(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	a.Service.NoteAccess(r.PathValue("id"))
	etag := `"` + v.Digest + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-cache")
	_, _ = w.Write(v.PNG)
}
func (a API) purge(w http.ResponseWriter, r *http.Request) {
	v, e := a.Service.Purge(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, map[string]any{"state": "purged", "artifactId": r.PathValue("id"), "execution": v})
}
func (a API) compare(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Left  string `json:"left"`
		Right string `json:"right"`
	}
	if e := decode(w, r, &b); e != nil {
		fail(w, e)
		return
	}
	release := a.Service.OpenRead(b.Left, b.Right)
	defer release()
	left, e := a.Service.Artifact(b.Left)
	if e != nil {
		fail(w, e)
		return
	}
	right, e := a.Service.Artifact(b.Right)
	if e != nil {
		fail(w, e)
		return
	}
	// Comparison is an access: a preview taken before it must be refreshed.
	a.Service.NoteAccess(b.Left, b.Right)
	v, e := observe.Compare(left, right)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, v)
}
func (a API) status(w http.ResponseWriter, r *http.Request) {
	write(w, 200, observe.Summary(a.Service.Store.Read()))
}
func (a API) events(w http.ResponseWriter, r *http.Request) {
	write(w, 200, a.Service.Store.Read().Events)
}
