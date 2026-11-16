package service

import (
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/scene"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxPreviewItems  = 200
	maxPendingReview = 64
	maxPreviewRecord = 128
)

// readGate tracks results that a download or comparison is actively reading.
// Purging waits for the gate so a result cannot be removed between an operator
// preview and the response that is already being written.
type readGate struct {
	mu     sync.Mutex
	counts map[string]int
}

func newReadGate() *readGate { return &readGate{counts: map[string]int{}} }

func (g *readGate) acquire(ids ...string) func() {
	g.mu.Lock()
	for _, id := range ids {
		g.counts[id]++
	}
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		for _, id := range ids {
			if g.counts[id]--; g.counts[id] <= 0 {
				delete(g.counts, id)
			}
		}
		g.mu.Unlock()
	}
}

func (g *readGate) held(ids []string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.heldLocked(ids)
}

func (g *readGate) heldLocked(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		if g.counts[id] > 0 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// exclusive runs fn while no download or comparison can acquire any of ids.
func (g *readGate) exclusive(ids []string, fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if held := g.heldLocked(ids); len(held) > 0 {
		return model.ConflictDetails("result is being read by a concurrent request", map[string]any{
			"reason":      "artifact-in-use",
			"artifactIds": held,
		})
	}
	return fn()
}

// Service carries the read gate and per-artifact read counters. Counters are
// in-process on purpose: a restart also changes BootID, so every preview from a
// previous process is already unusable.
type Service struct {
	Store *persist.Store
	gate  *readGate
	reads sync.Map // artifact id -> *atomic.Uint64
	// PreviewTTL is how long a preview may be executed. Operators configure it
	// through ATLAS_RETENTION_PREVIEW_TTL; the bound keeps a preview a short
	// observation window rather than a standing authorization.
	PreviewTTL time.Duration
}

// New builds a service with its retention gate initialized.
func New(st *persist.Store) *Service {
	return &Service{Store: st, gate: newReadGate(), PreviewTTL: model.PreviewTTL}
}

// NoteAccess records that results were downloaded or compared. Any preview
// item inspected before this access must be refreshed before it can purge.
func (s *Service) NoteAccess(ids ...string) {
	for _, id := range ids {
		v, _ := s.reads.LoadOrStore(id, new(atomic.Uint64))
		v.(*atomic.Uint64).Add(1)
	}
}

// OpenRead leases ids for the duration of a download or comparison. The
// returned release function must always be called.
func (s *Service) OpenRead(ids ...string) func() { return s.gate.acquire(ids...) }

func (s *Service) readsOf(id string) uint64 {
	if v, ok := s.reads.Load(id); ok {
		return v.(*atomic.Uint64).Load()
	}
	return 0
}

type RetentionRequest struct {
	ArtifactIDs []string   `json:"artifactIds"`
	Before      *time.Time `json:"before"`
	Note        string     `json:"note"`
}

type RetentionPreviewView struct {
	model.RetentionPreview
	EligibleCount    int      `json:"eligibleCount"`
	HeldCount        int      `json:"heldCount"`
	BytesReclaimable int      `json:"bytesReclaimable"`
	Expired          bool     `json:"expired"`
	CurrentlyHeld    []string `json:"currentlyHeld"`
}

// PreviewRetention inspects the selected results and persists an expiring,
// token-bound plan. It never deletes anything.
func (s *Service) PreviewRetention(req RetentionRequest) (RetentionPreviewView, error) {
	if len(req.ArtifactIDs) == 0 && req.Before == nil {
		return RetentionPreviewView{}, model.Invalid("retention preview requires artifactIds or before")
	}
	if len(req.ArtifactIDs) > maxPreviewItems {
		return RetentionPreviewView{}, model.Invalid("retention preview selects at most %d results", maxPreviewItems)
	}
	if len(req.Note) > 200 {
		return RetentionPreviewView{}, model.Invalid("retention note is limited to 200 characters")
	}
	for _, id := range req.ArtifactIDs {
		if !scene.ValidID(id) {
			return RetentionPreviewView{}, model.Invalid("invalid artifact id in retention selector")
		}
	}
	if req.Before != nil && req.Before.IsZero() {
		return RetentionPreviewView{}, model.Invalid("retention before timestamp is zero")
	}
	// Resolve the selection from a point-in-time read first so the read gate is
	// only ever acquired outside the store lock (execution locks it in the
	// opposite order).
	probe := s.Store.Read()
	ids := selectRetentionIDs(probe, req)
	if len(ids) > maxPreviewItems {
		return RetentionPreviewView{}, model.Invalid("retention preview matches more than %d results", maxPreviewItems)
	}
	held := s.gate.held(ids)
	var view RetentionPreviewView
	e := s.Store.Update(func(st *model.State) error {
		ids := selectRetentionIDs(*st, req)
		if len(ids) > maxPreviewItems {
			return model.Invalid("retention preview matches more than %d results", maxPreviewItems)
		}
		sort.Strings(ids)
		now := time.Now().UTC()
		// Expired pending previews are retired in place before counting, so an
		// operator who previews and walks away cannot block new previews.
		pending := 0
		for id, p := range st.RetentionPreviews {
			if p.State != "pending" {
				continue
			}
			if !now.Before(p.ExpiresAt) {
				p.State = "stale"
				st.RetentionPreviews[id] = p
				continue
			}
			pending++
		}
		if pending >= maxPendingReview {
			return model.Busy("too many pending retention previews; execute or wait for one to expire")
		}
		p := model.RetentionPreview{
			ID:          model.ID("prev", st.Sequence+1),
			BootID:      st.BootID,
			State:       "pending",
			CreatedAt:   now,
			ExpiresAt:   now.Add(s.previewTTL()),
			Sequence:    st.Sequence + 1,
			Fingerprint: model.RetentionFingerprint(*st),
			Selector:    model.RetentionSelector{ArtifactIDs: req.ArtifactIDs, Before: req.Before, Note: req.Note},
		}
		p.Items = []model.RetentionItem{}
		for _, id := range ids {
			p.Items = append(p.Items, retentionItem(*st, id, held, s.readsOf(id)))
		}
		p.Token = model.RetentionToken(p)
		st.RetentionPreviews[p.ID] = p
		st.Emit("retention.previewed", p.ID)
		prunePreviews(st)
		view = s.previewView(p, held)
		return nil
	})
	return view, e
}

// selectRetentionIDs expands a selector against a state snapshot. Explicit IDs
// are always included so the preview can report missing or already-purged
// results instead of silently dropping them. The function is pure: callers may
// evaluate it both before and inside a transaction.
func selectRetentionIDs(st model.State, req RetentionRequest) []string {
	ids := append([]string(nil), req.ArtifactIDs...)
	explicit := map[string]bool{}
	for _, id := range req.ArtifactIDs {
		explicit[id] = true
	}
	if req.Before != nil {
		for id, a := range st.Artifacts {
			if a.State == "live" && !a.CreatedAt.IsZero() && a.CreatedAt.Before(*req.Before) && !explicit[id] {
				ids = append(ids, id)
			}
		}
	}
	unique := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !unique[id] {
			unique[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func retentionItem(st model.State, id string, held []string, reads uint64) model.RetentionItem {
	item := model.RetentionItem{ArtifactID: id, Reason: "artifact not found", State: "missing", ReadsAtPreview: reads}
	a, ok := st.Artifacts[id]
	if !ok {
		return item
	}
	item.JobID = a.JobID
	if j, ok := st.Jobs[a.JobID]; ok {
		item.JobState = j.State
	}
	item.Digest = a.Digest
	item.Width = a.Width
	item.Height = a.Height
	if !a.CreatedAt.IsZero() {
		t := a.CreatedAt
		item.CreatedAt = &t
	}
	item.State = a.State
	if a.State == "purged" {
		item.Reason = "result was already purged"
		return item
	}
	if item.JobState != "succeeded" {
		item.Reason = "result job is not succeeded"
		return item
	}
	for _, h := range held {
		if h == id {
			item.Eligible = false
			item.Hold = "in-use"
			item.Reason = "result is being downloaded or compared"
			return item
		}
	}
	item.Bytes = len(a.PNG)
	item.Eligible = true
	item.Reason = "live result eligible for purge"
	return item
}

func prunePreviews(st *model.State) {
	type entry struct {
		id string
		p  model.RetentionPreview
	}
	all := make([]entry, 0, len(st.RetentionPreviews))
	for id, p := range st.RetentionPreviews {
		all = append(all, entry{id, p})
	}
	if len(all) <= maxPreviewRecord {
		return
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].p.State == "pending" && all[j].p.State != "pending" {
			return false
		}
		if all[j].p.State == "pending" && all[i].p.State != "pending" {
			return true
		}
		return all[i].p.CreatedAt.Before(all[j].p.CreatedAt)
	})
	for _, v := range all[:len(all)-maxPreviewRecord] {
		delete(st.RetentionPreviews, v.id)
	}
}

func (s *Service) previewView(p model.RetentionPreview, held []string) RetentionPreviewView {
	v := RetentionPreviewView{RetentionPreview: p, CurrentlyHeld: held}
	if v.CurrentlyHeld == nil {
		v.CurrentlyHeld = []string{}
	}
	for _, item := range p.Items {
		if item.Eligible {
			v.EligibleCount++
			v.BytesReclaimable += item.Bytes
		}
		if item.Hold != "" {
			v.HeldCount++
		}
	}
	v.Expired = !time.Now().UTC().Before(p.ExpiresAt)
	return v
}

// RetentionPreview returns a stored preview with current usability signals.
func (s *Service) RetentionPreview(id string) (RetentionPreviewView, error) {
	p, ok := s.Store.Read().RetentionPreviews[id]
	if !ok {
		return RetentionPreviewView{}, model.Missing("retention preview not found")
	}
	ids := []string{}
	for _, item := range p.Items {
		ids = append(ids, item.ArtifactID)
	}
	return s.previewView(p, s.gate.held(ids)), nil
}

// RetentionPreviews lists previews newest first.
func (s *Service) RetentionPreviews() []RetentionPreviewView {
	st := s.Store.Read()
	views := make([]RetentionPreviewView, 0, len(st.RetentionPreviews))
	for _, p := range st.RetentionPreviews {
		views = append(views, s.previewView(p, nil))
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].ID > views[j].ID
		}
		return views[i].CreatedAt.After(views[j].CreatedAt)
	})
	return views
}

// ExecuteRetention purges the eligible items of a pending preview. When only
// is set, the preview must cover and have marked that artifact eligible.
func (s *Service) ExecuteRetention(previewID, token, only string) (model.RetentionExecution, error) {
	peek, ok := s.Store.Read().RetentionPreviews[previewID]
	if !ok {
		return model.RetentionExecution{}, model.Missing("retention preview not found")
	}
	// A repeated execution with the same token and scope is an idempotent
	// replay of the recorded outcome; it never purges a second time.
	if out, ok := replayExecution(peek, token, only); ok {
		return out, nil
	}
	targets := []string{}
	for _, item := range peek.Items {
		if item.Eligible {
			targets = append(targets, item.ArtifactID)
		}
	}
	if only != "" {
		targets = []string{only}
	}
	var out model.RetentionExecution
	var refusal error
	e := s.gate.exclusive(targets, func() error {
		return s.Store.Update(func(st *model.State) error {
			p, ok := st.RetentionPreviews[previewID]
			if !ok {
				return model.Missing("retention preview not found")
			}
			// Re-check under the transaction so two concurrent duplicate
			// executions cannot purge twice or lose a committed outcome.
			if replay, ok := replayExecution(p, token, only); ok {
				out = replay
				return errReplayed
			}
			if reason, e := s.checkPreviewUsable(*st, p, token); e != nil {
				// A preview is a one-shot observation. When the world it described
				// has moved, retire it so it cannot be reused and cannot occupy the
				// pending cap. Refusals that merely report a transient condition
				// (bytes in use, nothing eligible yet) leave the preview pending so
				// the operator may retry while it is still fresh.
				if p.State == "pending" && (reason == "access-changed" || reason == "results-changed" || reason == "expired" || reason == "restarted") {
					p.State = "stale"
					st.RetentionPreviews[p.ID] = p
					st.Emit("retention.stale", p.ID)
				}
				refusal = e
				return nil
			}
			selected := map[string]model.RetentionItem{}
			for _, item := range p.Items {
				selected[item.ArtifactID] = item
			}
			apply := []string{}
			if only == "" {
				for _, item := range p.Items {
					if item.Eligible {
						apply = append(apply, item.ArtifactID)
					}
				}
			} else {
				item, ok := selected[only]
				if !ok {
					return model.ConflictDetails("artifact is not covered by this retention preview", previewDetails(p, "artifact-not-covered", only))
				}
				if !item.Eligible && item.State != "purged" {
					return model.ConflictDetails("artifact was not eligible when the preview was created", previewDetails(p, "artifact-not-eligible", only))
				}
				apply = []string{only}
			}
			if len(apply) == 0 {
				held := []string{}
				for _, item := range p.Items {
					if item.Hold != "" {
						held = append(held, item.ArtifactID)
					}
				}
				if len(held) > 0 {
					d := previewDetails(p, "artifact-in-use", "")
					d["artifactIds"] = held
					return model.ConflictDetails("result is being read by a concurrent request", d)
				}
				return model.ConflictDetails("retention preview has no eligible results", previewDetails(p, "no-eligible-results", ""))
			}
			now := time.Now().UTC()
			out = model.RetentionExecution{PreviewID: p.ID, State: "executed", Only: only, Purged: []model.PurgeOutcome{}, ExecutedAt: now}
			for _, id := range apply {
				a := st.Artifacts[id]
				outcome := model.PurgeOutcome{ArtifactID: id, JobID: a.JobID, Digest: a.Digest, Width: a.Width, Height: a.Height}
				if j, ok := st.Jobs[a.JobID]; ok {
					outcome.JobState = j.State
				}
				if a.State == "purged" {
					outcome.State = "already-purged"
					if a.PurgedAt != nil {
						t := *a.PurgedAt
						outcome.PurgedAt = &t
					}
					out.Purged = append(out.Purged, outcome)
					continue
				}
				if a.State != "live" {
					return model.ConflictDetails("artifact state changed since the preview", previewDetails(p, "result-state-changed", id))
				}
				outcome.BytesReleased = len(a.PNG)
				a.State = "purged"
				a.PNG = nil
				a.PurgedAt = &now
				st.Artifacts[id] = a
				st.Emit("artifact.purged", id)
				outcome.State = "purged"
				outcome.PurgedAt = &now
				out.Purged = append(out.Purged, outcome)
				out.BytesReleased += outcome.BytesReleased
			}
			for _, a := range st.Artifacts {
				out.RemainingBytes += len(a.PNG)
			}
			p.State = "executed"
			p.ExecutedAt = &now
			stored := out
			stored.Replayed = false
			p.Outcome = &stored
			st.RetentionPreviews[p.ID] = p
			st.Emit("retention.executed", p.ID)
			return nil
		})
	})
	if errors.Is(e, errReplayed) {
		return out, nil
	}
	if e != nil {
		return model.RetentionExecution{}, e
	}
	if refusal != nil {
		return model.RetentionExecution{}, refusal
	}
	return out, e
}

var errReplayed = errors.New("retention execution replayed")

func replayExecution(p model.RetentionPreview, token, only string) (model.RetentionExecution, bool) {
	if p.State != "executed" || p.Token != token || p.Outcome == nil {
		return model.RetentionExecution{}, false
	}
	if p.Outcome.Only != only {
		return model.RetentionExecution{}, false
	}
	out := model.Clone(*p.Outcome)
	out.Replayed = true
	return out, true
}

// retirePreview consumes a pending preview that will not be executed, so an
// abandoned internal preview cannot occupy the pending cap.
func (s *Service) retirePreview(id string) {
	_ = s.Store.Update(func(st *model.State) error {
		p, ok := st.RetentionPreviews[id]
		if !ok || p.State != "pending" {
			return nil
		}
		p.State = "stale"
		st.RetentionPreviews[id] = p
		st.Emit("retention.stale", id)
		return nil
	})
}

func (s *Service) previewTTL() time.Duration {
	if s.PreviewTTL <= 0 {
		return model.PreviewTTL
	}
	return s.PreviewTTL
}

func (s *Service) checkPreviewUsable(st model.State, p model.RetentionPreview, token string) (string, error) {
	if p.Token != token || model.RetentionToken(p) != token {
		return "token-mismatch", model.ConflictDetails("retention preview token does not match", previewDetails(p, "token-mismatch", ""))
	}
	// Boot identity is checked before the stale flag so a restart reports its
	// actual cause; pending previews are also stamped stale for the audit list.
	if st.BootID != p.BootID {
		return "restarted", model.ConflictDetails("service restarted since the preview was created", previewDetails(p, "restarted", ""))
	}
	switch p.State {
	case "executed":
		return "already-executed", model.ConflictDetails("retention preview was already executed", previewDetails(p, "already-executed", ""))
	case "stale":
		return "stale", model.ConflictDetails("retention preview is stale", previewDetails(p, "stale", ""))
	}
	if !time.Now().UTC().Before(p.ExpiresAt) {
		return "expired", model.ConflictDetails("retention preview has expired", previewDetails(p, "expired", ""))
	}
	changed := []string{}
	for _, item := range p.Items {
		if s.readsOf(item.ArtifactID) != item.ReadsAtPreview {
			changed = append(changed, item.ArtifactID)
		}
	}
	if len(changed) > 0 {
		d := previewDetails(p, "access-changed", "")
		d["artifactIds"] = changed
		return "access-changed", model.ConflictDetails("results were downloaded or compared after the preview; create a new preview", d)
	}
	if current := model.RetentionFingerprint(st); current != p.Fingerprint {
		return "results-changed", model.ConflictDetails("result set changed after the preview; create a new preview", previewDetails(p, "results-changed", ""))
	}
	return "", nil
}

func previewDetails(p model.RetentionPreview, reason, artifact string) map[string]any {
	d := map[string]any{
		"reason":       reason,
		"previewId":    p.ID,
		"previewState": p.State,
		"expiresAt":    p.ExpiresAt,
		"hint":         "POST /v1/retention/previews to inspect current impact",
	}
	if artifact != "" {
		d["artifactId"] = artifact
	}
	return d
}

// ArtifactGone builds the stable response for a purged result. The successful
// job and its digest/dimensions stay visible so the purge never rewrites job
// history into an unfinished state.
func ArtifactGone(a model.Artifact, job model.Job) error {
	d := map[string]any{
		"artifactId": a.ID,
		"jobId":      a.JobID,
		"jobState":   job.State,
		"digest":     a.Digest,
		"width":      a.Width,
		"height":     a.Height,
		"purged":     true,
	}
	if a.PurgedAt != nil {
		d["purgedAt"] = a.PurgedAt
	}
	return model.GoneDetails(fmt.Sprintf("artifact %s was purged; its successful job record is retained", a.ID), d)
}
