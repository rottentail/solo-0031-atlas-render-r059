package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// PreviewTTL bounds how long an operator preview may be executed. A preview is
// a statement about observed state, never a standing authorization.
const PreviewTTL = 10 * time.Minute

// RetentionSelector chooses the results a preview should inspect. At least one
// of ArtifactIDs or Before must be set. ArtifactIDs names exact results; Before
// selects live results produced strictly before the cutoff.
type RetentionSelector struct {
	ArtifactIDs []string   `json:"artifactIds,omitempty"`
	Before      *time.Time `json:"before,omitempty"`
	Note        string     `json:"note,omitempty"`
}

// RetentionItem records one result's impact and eligibility at preview time.
type RetentionItem struct {
	ArtifactID string     `json:"artifactId"`
	JobID      string     `json:"jobId,omitempty"`
	JobState   string     `json:"jobState,omitempty"`
	State      string     `json:"state"`
	Digest     string     `json:"digest,omitempty"`
	Width      int        `json:"width,omitempty"`
	Height     int        `json:"height,omitempty"`
	Bytes      int        `json:"bytes,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	// ReadsAtPreview is the artifact's completed read counter when this item was
	// inspected. Any later download or comparison changes it and invalidates the
	// item, so a preview can never authorize purging bytes that are in use.
	ReadsAtPreview uint64 `json:"readsAtPreview"`
	Eligible       bool   `json:"eligible"`
	Reason         string `json:"reason"`
	Hold           string `json:"hold,omitempty"`
}

// RetentionPreview is the persisted, auditable record of a preview.
type RetentionPreview struct {
	ID          string              `json:"id"`
	Token       string              `json:"token"`
	BootID      string              `json:"bootId"`
	State       string              `json:"state"`
	CreatedAt   time.Time           `json:"createdAt"`
	ExpiresAt   time.Time           `json:"expiresAt"`
	Sequence    int                 `json:"sequence"`
	Fingerprint string              `json:"fingerprint"`
	Selector    RetentionSelector   `json:"selector"`
	Items       []RetentionItem     `json:"items"`
	ExecutedAt  *time.Time          `json:"executedAt,omitempty"`
	Outcome     *RetentionExecution `json:"outcome,omitempty"`
}

// RetentionPlan is the immutable part of a preview covered by its token.
type RetentionPlan struct {
	ID          string            `json:"id"`
	BootID      string            `json:"bootId"`
	CreatedAt   time.Time         `json:"createdAt"`
	ExpiresAt   time.Time         `json:"expiresAt"`
	Sequence    int               `json:"sequence"`
	Fingerprint string            `json:"fingerprint"`
	Selector    RetentionSelector `json:"selector"`
	Items       []RetentionItem   `json:"items"`
}

// RetentionToken binds an execution request to the exact previewed plan.
func RetentionToken(p RetentionPreview) string {
	return CanonicalDigest(RetentionPlan{
		ID: p.ID, BootID: p.BootID, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
		Sequence: p.Sequence, Fingerprint: p.Fingerprint,
		Selector: p.Selector, Items: p.Items,
	})
}

// ValidateRetentionPreview checks persisted preview records. The token makes
// the plan tamper-evident without trusting the caller.
func ValidateRetentionPreview(p RetentionPreview) error {
	if p.ID == "" || p.Token == "" || p.BootID == "" {
		return fmt.Errorf("invalid retention preview record")
	}
	switch p.State {
	case "pending", "executed", "stale":
	default:
		return fmt.Errorf("invalid retention preview state")
	}
	if p.CreatedAt.IsZero() || !p.ExpiresAt.After(p.CreatedAt) {
		return fmt.Errorf("invalid retention preview window")
	}
	if (p.State == "executed") != (p.ExecutedAt != nil) {
		return fmt.Errorf("invalid retention preview execution record")
	}
	if (p.State == "executed") != (p.Outcome != nil) {
		return fmt.Errorf("invalid retention preview outcome record")
	}
	if p.Token != RetentionToken(p) {
		return fmt.Errorf("retention preview token mismatch")
	}
	if len(p.Items) > 200 {
		return fmt.Errorf("retention preview has too many items")
	}
	seen := map[string]bool{}
	for _, item := range p.Items {
		if item.ArtifactID == "" || item.Reason == "" || seen[item.ArtifactID] {
			return fmt.Errorf("invalid retention preview item")
		}
		seen[item.ArtifactID] = true
		switch item.State {
		case "live", "purged", "missing":
		default:
			return fmt.Errorf("invalid retention preview item state")
		}
		if item.Eligible && (item.State != "live" || item.Hold != "") {
			return fmt.Errorf("ineligible retention preview item marked eligible")
		}
	}
	return nil
}

// PurgeOutcome records the retained facts of one purged result.
type PurgeOutcome struct {
	ArtifactID    string     `json:"artifactId"`
	JobID         string     `json:"jobId,omitempty"`
	JobState      string     `json:"jobState,omitempty"`
	State         string     `json:"state"`
	Digest        string     `json:"digest,omitempty"`
	Width         int        `json:"width,omitempty"`
	Height        int        `json:"height,omitempty"`
	BytesReleased int        `json:"bytesReleased"`
	PurgedAt      *time.Time `json:"purgedAt,omitempty"`
}

// RetentionExecution is both the stored outcome and the execution response.
type RetentionExecution struct {
	PreviewID      string         `json:"previewId"`
	State          string         `json:"state"`
	Only           string         `json:"only,omitempty"`
	Purged         []PurgeOutcome `json:"purged"`
	BytesReleased  int            `json:"bytesReleased"`
	RemainingBytes int            `json:"remainingArtifactBytes"`
	ExecutedAt     time.Time      `json:"executedAt"`
	Replayed       bool           `json:"replayed,omitempty"`
}

type retentionFact struct {
	ID       string
	JobID    string
	State    string
	Digest   string
	Width    int
	Height   int
	Bytes    int
	PurgedAt *time.Time
}

// RetentionFingerprint summarizes the result facts a preview depends on. Any
// new completed result, purge, or byte/digest change yields a new fingerprint.
func RetentionFingerprint(st State) string {
	facts := make([]retentionFact, 0, len(st.Artifacts))
	for _, a := range st.Artifacts {
		facts = append(facts, retentionFact{ID: a.ID, JobID: a.JobID, State: a.State, Digest: a.Digest, Width: a.Width, Height: a.Height, Bytes: len(a.PNG), PurgedAt: a.PurgedAt})
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].ID < facts[j].ID })
	return CanonicalDigest(facts)
}

// NewBootID returns a random boot identity. It is not derived from wall time
// so two processes starting in the same instant still differ.
func NewBootID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
