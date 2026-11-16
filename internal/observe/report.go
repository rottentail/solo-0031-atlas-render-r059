package observe

import (
	"atlas-render/internal/codec"
	"atlas-render/internal/model"
	"image/color"
	"math"
)

type Comparison struct {
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	ChangedPixels int     `json:"changedPixels"`
	MaxDelta      int     `json:"maxDelta"`
	MeanDelta     float64 `json:"meanDelta"`
}

func Compare(a, b model.Artifact) (Comparison, error) {
	var out Comparison
	if a.Width != b.Width || a.Height != b.Height {
		return out, model.Conflict("artifact dimensions differ")
	}
	ia, e := codec.DecodeOutput(a.PNG)
	if e != nil {
		return out, e
	}
	ib, e := codec.DecodeOutput(b.PNG)
	if e != nil {
		return out, e
	}
	out.Width = a.Width
	out.Height = a.Height
	total := 0
	for y := 0; y < out.Height; y++ {
		for x := 0; x < out.Width; x++ {
			ca := color.NRGBAModel.Convert(ia.At(x, y)).(color.NRGBA)
			cb := color.NRGBAModel.Convert(ib.At(x, y)).(color.NRGBA)
			delta := 0
			for i, v := range []uint8{ca.R, ca.G, ca.B, ca.A} {
				w := []uint8{cb.R, cb.G, cb.B, cb.A}[i]
				d := int(math.Abs(float64(v) - float64(w)))
				delta += d
				if d > out.MaxDelta {
					out.MaxDelta = d
				}
			}
			if delta > 0 {
				out.ChangedPixels++
			}
			total += delta
		}
	}
	out.MeanDelta = float64(total) / float64(out.Width*out.Height*4)
	return out, nil
}
func Summary(st model.State) map[string]any {
	jobs := map[string]int{}
	for _, j := range st.Jobs {
		jobs[j.State]++
	}
	purged := 0
	bytes := 0
	for _, a := range st.Artifacts {
		if a.State == "purged" {
			purged++
			continue
		}
		bytes += len(a.PNG)
	}
	previews := map[string]int{}
	for _, p := range st.RetentionPreviews {
		previews[p.State]++
	}
	return map[string]any{
		"sceneCount": len(st.Scenes), "assetCount": len(st.Assets), "snapshotCount": len(st.Snapshots),
		"jobs": jobs, "artifactBytes": bytes, "liveResultCount": len(st.Artifacts) - purged,
		"purgedResultCount": purged, "retentionPreviews": previews, "bootId": st.BootID,
		"sequence": st.Sequence,
	}
}
