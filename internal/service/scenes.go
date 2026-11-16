package service

import (
	"atlas-render/internal/model"
	"atlas-render/internal/scene"
	"strings"
	"time"
)

func (s *Service) PutScene(id, name string, expected int, d model.Document) (model.Scene, error) {
	var out model.Scene
	if !scene.ValidID(id) || strings.TrimSpace(name) == "" || len(name) > 160 {
		return out, model.Invalid("invalid scene id or name")
	}
	if e := scene.Validate(d); e != nil {
		return out, e
	}
	e := s.Store.Update(func(st *model.State) error {
		old, ok := st.Scenes[id]
		if old.Revision != expected {
			return model.Conflict("scene revision changed")
		}
		if ok && old.State != "active" {
			return model.Conflict("scene is archived")
		}
		for _, ref := range scene.References(d) {
			a, ok := st.Assets[ref]
			if !ok || a.State != "available" {
				return model.Conflict("scene references unavailable asset")
			}
		}
		out = model.Scene{ID: id, Name: name, Revision: old.Revision + 1, State: "active", Document: d, UpdatedAt: time.Now().UTC()}
		st.Scenes[id] = out
		st.Emit("scene.saved", id)
		return nil
	})
	return out, e
}
func (s *Service) SceneState(id string, expected int, target string) (model.Scene, error) {
	var out model.Scene
	if target != "active" && target != "archived" {
		return out, model.Invalid("unknown scene state")
	}
	e := s.Store.Update(func(st *model.State) error {
		v, ok := st.Scenes[id]
		if !ok {
			return model.Missing("scene not found")
		}
		if v.Revision != expected {
			return model.Conflict("scene revision changed")
		}
		if v.State == target {
			out = v
			return nil
		}
		v.State = target
		v.Revision++
		v.UpdatedAt = time.Now().UTC()
		st.Scenes[id] = v
		st.Emit("scene."+target, id)
		out = v
		return nil
	})
	return out, e
}
func (s *Service) Freeze(id string, expected int) (model.Snapshot, error) {
	var out model.Snapshot
	e := s.Store.Update(func(st *model.State) error {
		v, ok := st.Scenes[id]
		if !ok {
			return model.Missing("scene not found")
		}
		if v.Revision != expected || v.State != "active" {
			return model.Conflict("scene revision or state changed")
		}
		assets := map[string]model.Asset{}
		for _, ref := range scene.References(v.Document) {
			a, ok := st.Assets[ref]
			if !ok || a.State != "available" {
				return model.Conflict("snapshot references unavailable asset")
			}
			assets[ref] = a
		}
		out = model.Snapshot{ID: model.ID("snap", st.Sequence+1), SceneID: id, Revision: v.Revision, State: "active", Document: v.Document, Assets: assets, CreatedAt: time.Now().UTC()}
		out.Digest = model.SnapshotDigest(out)
		st.Snapshots[out.ID] = out
		st.Emit("snapshot.frozen", out.ID)
		return nil
	})
	return out, e
}
func (s *Service) Revoke(id string) error {
	return s.Store.Update(func(st *model.State) error {
		v, ok := st.Snapshots[id]
		if !ok {
			return model.Missing("snapshot not found")
		}
		if v.State == "revoked" {
			return nil
		}
		v.State = "revoked"
		st.Snapshots[id] = v
		st.Emit("snapshot.revoked", id)
		return nil
	})
}
