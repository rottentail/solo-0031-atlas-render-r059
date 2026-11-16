package service

import (
	"atlas-render/internal/model"
	"atlas-render/internal/raster"
	"atlas-render/internal/scene"
	"time"
)

func (s *Service) Submit(snapshotID string, scale int, key string) (model.Job, error) {
	var out model.Job
	if !scene.ValidID(key) {
		return out, model.Invalid("idempotency key required (1..80 identifier characters)")
	}
	fingerprint := model.CanonicalDigest([]any{snapshotID, scale})
	e := s.Store.Update(func(st *model.State) error {
		if r, ok := st.Receipts[key]; ok {
			if r.Fingerprint != fingerprint {
				return model.Conflict("idempotency key used for different request")
			}
			out = st.Jobs[r.JobID]
			return nil
		}
		snap, ok := st.Snapshots[snapshotID]
		if !ok {
			return model.Missing("snapshot not found")
		}
		if snap.State != "active" {
			return model.Conflict("snapshot is revoked")
		}
		if _, e := raster.Compile(snap, scale); e != nil {
			return e
		}
		pending := 0
		for _, j := range st.Jobs {
			if j.State == "queued" || j.State == "running" {
				pending++
			}
		}
		if pending >= 64 {
			return model.Busy("render queue is full")
		}
		now := time.Now().UTC()
		out = model.Job{ID: model.ID("job", st.Sequence+1), SnapshotID: snapshotID, State: "queued", Scale: scale, Attempt: 1, CreatedAt: now, UpdatedAt: now, Payload: snap}
		st.Jobs[out.ID] = out
		st.Receipts[key] = model.Receipt{Key: key, Fingerprint: fingerprint, JobID: out.ID}
		st.Emit("job.queued", out.ID)
		return nil
	})
	return out, e
}
func (s *Service) Cancel(id string) (model.Job, error) {
	var out model.Job
	e := s.Store.Update(func(st *model.State) error {
		j, ok := st.Jobs[id]
		if !ok {
			return model.Missing("job not found")
		}
		if j.State == "cancelled" {
			out = j
			return nil
		}
		if j.State != "queued" && j.State != "running" {
			return model.Conflict("job has already finished")
		}
		j.State = "cancelled"
		j.UpdatedAt = time.Now().UTC()
		st.Jobs[id] = j
		st.Emit("job.cancelled", id)
		out = j
		return nil
	})
	return out, e
}
func (s *Service) Retry(id string) (model.Job, error) {
	var out model.Job
	e := s.Store.Update(func(st *model.State) error {
		j, ok := st.Jobs[id]
		if !ok {
			return model.Missing("job not found")
		}
		if j.State != "failed" && j.State != "cancelled" {
			return model.Conflict("only failed or cancelled jobs can retry")
		}
		n := 0
		for _, v := range st.Jobs {
			if v.State == "queued" || v.State == "running" {
				n++
			}
		}
		if n >= 64 {
			return model.Busy("render queue is full")
		}
		j.State = "queued"
		j.Error = ""
		j.Attempt++
		j.UpdatedAt = time.Now().UTC()
		st.Jobs[id] = j
		st.Emit("job.retried", id)
		out = j
		return nil
	})
	return out, e
}

// Purge is the single-artifact compatibility entry. It creates a fresh preview
// for exactly that result and executes it, so even the direct DELETE observes
// the same staleness, expiry, access and concurrency rules as the preview flow.
func (s *Service) Purge(id string) (model.RetentionExecution, error) {
	if _, ok := s.Store.Read().Artifacts[id]; !ok {
		return model.RetentionExecution{}, model.Missing("artifact not found")
	}
	p, e := s.PreviewRetention(RetentionRequest{ArtifactIDs: []string{id}, Note: "direct artifact purge"})
	if e != nil {
		return model.RetentionExecution{}, e
	}
	out, e := s.ExecuteRetention(p.ID, p.Token, id)
	if e != nil {
		// The direct entry has no operator-visible preview to retry, so its
		// internal one-shot preview is consumed rather than left pending.
		s.retirePreview(p.ID)
	}
	return out, e
}

// Artifact returns a live result. Purged results yield a stable 410 carrying
// the retained successful-job facts instead of pretending the job never ran.
func (s *Service) Artifact(id string) (model.Artifact, error) {
	st := s.Store.Read()
	a, ok := st.Artifacts[id]
	if !ok {
		return a, model.Missing("artifact not found")
	}
	if a.State == "purged" {
		return a, ArtifactGone(a, st.Jobs[a.JobID])
	}
	return a, nil
}
